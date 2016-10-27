// Copyright 2016 The Cockroach Authors.

//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Radu Berinde (radu@cockroachlabs.com)

package sql

import (
	"bytes"
	"fmt"
	"math"

	"github.com/cockroachdb/cockroach/pkg/gossip"
	"github.com/cockroachdb/cockroach/pkg/internal/client"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql/distsql"
	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/util/encoding"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/uuid"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
)

// distSQLPLanner implements distSQL physical planning logic.
//
// A rough overview of the process:
//
//  - the plan is based on a planNode tree (in the future it will be based on an
//    intermediate representation tree). Only a subset of the possible trees is
//    supported (this can be checked via CheckSupport).
//
//  - we generate a physicalPlan for the planNode tree recursively. The
//    physicalPlan consists of a network of processors and streams, with a set
//    of unconnected "result streams". The physicalPlan also has information on
//    ordering and on the mapping planNode columns to columns in the result
//    streams (all result streams have the same schema).
//
//    The physicalPlan for a scanNode leaf consists of TableReaders, one for each node
//    that has a range.
//
//  - for each an internal planNode we start with the plan of the child node(s)
//    and add processing stages (connected to the result streams of the children
//    node).
type distSQLPlanner struct {
	nodeDesc     roachpb.NodeDescriptor
	distSQLSrv   *distsql.ServerImpl
	spanResolver *distsql.SpanResolver
}

const resolverPolicy = distsql.BinPackingLeaseHolderChoice

func newDistSQLPlanner(
	nodeDesc roachpb.NodeDescriptor,
	distSQLSrv *distsql.ServerImpl,
	distSender *kv.DistSender,
	gossip *gossip.Gossip,
) *distSQLPlanner {
	dsp := &distSQLPlanner{
		nodeDesc:   nodeDesc,
		distSQLSrv: distSQLSrv,
	}
	dsp.spanResolver = distsql.NewSpanResolver(distSender, gossip, nodeDesc, resolverPolicy)
	return dsp
}

// CheckSupport looks at a planNode tree and decides if DistSQL is equipped to
// handle the query.
func (dsp *distSQLPlanner) CheckSupport(tree planNode) error {
	switch n := tree.(type) {
	case *selectTopNode:
		if n.group != nil {
			return errors.Errorf("grouping not supported yet")
		}
		if n.window != nil {
			return errors.Errorf("windows not supported yet")
		}
		if n.sort != nil {
			return errors.Errorf("sorting not supported yet")
		}
		if n.distinct != nil {
			return errors.Errorf("distinct not supported yet")
		}
		if n.limit != nil {
			return errors.Errorf("limit not supported yet")
		}
		return dsp.CheckSupport(n.source)

	case *selectNode:
		if n.filter != nil {
			return errors.Errorf("filter not supported at select level yet")
		}
		return dsp.CheckSupport(n.source.plan)

	case *scanNode:
		return nil

	default:
		return errors.Errorf("unsupported node %T", tree)
	}
}

// planningCtx contains data used and updated throughout the planning process of
// a single query.
type planningCtx struct {
	ctx      context.Context
	spanIter *distsql.SpanResolverIterator
	// nodeAddresses contains addresses for all NodeIDs that are referenced by any
	// physicalPlan we generate with this context.
	nodeAddresses map[roachpb.NodeID]string
}

type processor struct {
	node roachpb.NodeID
	// the spec for the processor; note that the StreamEndpointSpecs in the input
	// synchronizers and output routers are not set until the end of the planning
	// process.
	spec distsql.ProcessorSpec
}

type stream struct {
	sourceProcessor int
	// sourceSlot identifies the output for multi-output processors.
	sourceSlot int

	destProcessor int
	// destSlot identifies the input for multi-input processors.
	destSlot int
}

type resultStream struct {
	processor int
	// sourceSlot identifies the output for multi-output processors.
	slot int
}

// physicalPlan is a partial physical plan which corresponds to a planNode. It
// represents a network of processors, with a set of result streams that are
// unconnected on the output side. These plans are built recursively on a
// planNode tree.
type physicalPlan struct {
	processors []processor
	streams    []stream

	resultStreams []resultStream

	// planToStreamColMap maps planNode Columns() to columns in the result streams.
	planToStreamColMap []int

	// ordering guarantee for the result streams that must be maintained in order
	// to guarantee the same ordering as the corresponding planNode.
	//
	// TODO(radu): in general, guaranteeing an ordering is not free (requires
	// ordered synchronizers when merging streams). We should determine if (and to
	// what extent) the ordering of a planNode is actually used by its parent.
	ordering distsql.Ordering
}

// The distsql Expression uses the placeholder syntax (@1, @2, @3..) to
// refer to columns. We format the expression using an IndexedVar formatting
// interceptor. A columnMap can optionally be used to remap the indices.
func distSQLExpression(expr parser.TypedExpr, columnMap []int) distsql.Expression {
	if expr == nil {
		return distsql.Expression{}
	}
	var f parser.FmtFlags
	if columnMap == nil {
		f = parser.FmtIndexedVarFormat(
			func(buf *bytes.Buffer, _ parser.FmtFlags, _ parser.IndexedVarContainer, idx int) {
				fmt.Fprintf(buf, "@%d", idx+1)
			},
		)
	} else {
		f = parser.FmtIndexedVarFormat(
			func(buf *bytes.Buffer, _ parser.FmtFlags, _ parser.IndexedVarContainer, idx int) {
				remappedIdx := columnMap[idx]
				if remappedIdx < 0 {
					panic(fmt.Sprintf("unmapped index %d", idx))
				}
				fmt.Fprintf(buf, "@%d", remappedIdx+1)
			},
		)
	}
	var buf bytes.Buffer
	expr.Format(&buf, f)
	return distsql.Expression{Expr: buf.String()}
}

type spanSplit struct {
	node  roachpb.NodeID
	spans roachpb.Spans
}

func (dsp *distSQLPlanner) splitSpans(
	planCtx *planningCtx, spans roachpb.Spans,
) ([]spanSplit, error) {
	if len(spans) == 0 {
		panic("no spans")
	}
	ctx := planCtx.ctx
	splits := make([]spanSplit, 0, 1)
	nodeMap := make(map[roachpb.NodeID]int)
	it := planCtx.spanIter
	for _, span := range spans {
		var rspan roachpb.RSpan
		var err error
		if rspan.Key, err = keys.Addr(span.Key); err != nil {
			return nil, err
		}
		if rspan.EndKey, err = keys.Addr(span.EndKey); err != nil {
			return nil, err
		}

		lastNodeID := roachpb.NodeID(0)
		for it.Seek(ctx, span, kv.Ascending); ; it.Next(ctx) {
			if !it.Valid() {
				return nil, it.Error()
			}
			replInfo, err := it.ReplicaInfo(ctx)
			if err != nil {
				return nil, err
			}
			desc := it.Desc()

			var trimmedSpan roachpb.Span
			if rspan.Key.Less(desc.StartKey) {
				trimmedSpan.Key = desc.StartKey.AsRawKey()
			} else {
				trimmedSpan.Key = span.Key
			}
			if desc.EndKey.Less(rspan.EndKey) {
				trimmedSpan.EndKey = desc.EndKey.AsRawKey()
			} else {
				trimmedSpan.EndKey = span.EndKey
			}

			nodeID := replInfo.NodeDesc.NodeID
			idx, ok := nodeMap[nodeID]
			if !ok {
				idx = len(splits)
				splits = append(splits, spanSplit{node: nodeID})
				nodeMap[nodeID] = idx
				if _, ok := planCtx.nodeAddresses[nodeID]; !ok {
					planCtx.nodeAddresses[nodeID] = replInfo.NodeDesc.Address.String()
				}
			}
			split := &splits[idx]

			if lastNodeID == nodeID {
				// Two consecutive ranges on the same node, merge the spans.
				if !split.spans[len(split.spans)-1].EndKey.Equal(trimmedSpan.Key) {
					log.Fatalf(ctx, "expected consecutive span pieces %v %v", split.spans, trimmedSpan)
				}
				split.spans[len(split.spans)-1].EndKey = trimmedSpan.EndKey
			} else {
				split.spans = append(split.spans, trimmedSpan)
			}

			lastNodeID = nodeID
			if !it.NeedAnother() {
				break
			}
		}
	}
	return splits, nil
}

// initTableReaderSpec initializes a TableReaderSpec that corresponds to a
// scanNode, except for the Spans.
func initTableReaderSpec(n *scanNode) (distsql.TableReaderSpec, error) {
	s := distsql.TableReaderSpec{
		Table:   n.desc,
		Reverse: n.reverse,
	}
	if n.index != &n.desc.PrimaryIndex {
		for i := range n.desc.Indexes {
			if n.index == &n.desc.Indexes[i] {
				s.IndexIdx = uint32(i + 1)
				break
			}
		}
		if s.IndexIdx == 0 {
			return distsql.TableReaderSpec{}, errors.Errorf("invalid scanNode index %d", n.index)
		}
	}
	s.OutputColumns = make([]uint32, 0, len(n.resultColumns))
	for i := range n.resultColumns {
		if n.valNeededForCol[i] {
			s.OutputColumns = append(s.OutputColumns, uint32(i))
		}
	}
	if n.limitSoft {
		s.SoftLimit = n.limitHint
	} else {
		s.HardLimit = n.limitHint
	}

	s.Filter = distSQLExpression(n.filter, nil)
	return s, nil
}

func (dsp *distSQLPlanner) convertOrdering(
	planOrdering sqlbase.ColumnOrdering, planToStreamColMap []int,
) distsql.Ordering {
	if len(planOrdering) == 0 {
		return distsql.Ordering{}
	}
	ordering := distsql.Ordering{
		Columns: make([]distsql.Ordering_Column, 0, len(planOrdering)),
	}
	for _, col := range planOrdering {
		streamColIdx := planToStreamColMap[col.ColIdx]
		if streamColIdx == -1 {
			// This column is not part of the output. The rest of the ordering is
			// irrelevant.
			break
		}
		oc := distsql.Ordering_Column{
			ColIdx:    uint32(streamColIdx),
			Direction: distsql.Ordering_Column_ASC,
		}
		if col.Direction == encoding.Descending {
			oc.Direction = distsql.Ordering_Column_DESC
		}
		ordering.Columns = append(ordering.Columns, oc)
	}
	return ordering
}

func (dsp *distSQLPlanner) createTableReaders(
	planCtx *planningCtx, n *scanNode,
) (physicalPlan, error) {
	spec, err := initTableReaderSpec(n)
	if err != nil {
		return physicalPlan{}, err
	}
	planToStreamColMap := make([]int, len(n.resultColumns))
	for i := range spec.OutputColumns {
		planToStreamColMap[i] = -1
	}
	for i, col := range spec.OutputColumns {
		planToStreamColMap[col] = i
	}
	ordering := dsp.convertOrdering(n.ordering.ordering, planToStreamColMap)

	spans := n.spans
	if len(n.spans) == 0 {
		// If no spans were specified retrieve all of the keys that start with our
		// index key prefix.
		start := roachpb.Key(sqlbase.MakeIndexKeyPrefix(&n.desc, n.index.ID))
		spans = roachpb.Spans{{Key: start, EndKey: start.PrefixEnd()}}
	}

	spanSplits, err := dsp.splitSpans(planCtx, spans)
	if err != nil {
		return physicalPlan{}, err
	}
	var p physicalPlan
	for _, s := range spanSplits {
		proc := processor{
			node: s.node,
		}

		tr := &distsql.TableReaderSpec{}
		*tr = spec
		tr.Spans = make([]distsql.TableReaderSpan, len(s.spans))
		for i := range s.spans {
			tr.Spans[i].Span = s.spans[i]
		}

		proc.spec.Core.SetValue(tr)
		proc.spec.Output = make([]distsql.OutputRouterSpec, 1)
		proc.spec.Output[0].Type = distsql.OutputRouterSpec_MIRROR

		pIdx := len(p.processors)
		p.processors = append(p.processors, proc)
		p.resultStreams = append(p.resultStreams, resultStream{processor: pIdx, slot: 0})
		p.planToStreamColMap = planToStreamColMap
		p.ordering = ordering
	}
	return p, nil
}

// selectRenders takes a physicalPlan that reflects a select source and updates
// it to reflect the select node. An evaluator stage is added if needed.
func (dsp *distSQLPlanner) selectRenders(
	planCtx *planningCtx, p *physicalPlan, n *selectNode,
) error {
	// First check if we need an Evaluator, or we are just returning values.
	needEval := false
	for _, e := range n.render {
		if _, ok := e.(*parser.IndexedVar); !ok {
			needEval = true
			break
		}
	}
	if !needEval {
		// We just need to update planToStreamColMap to reflect the select node.
		planToStreamColMap := make([]int, len(n.render))
		for i := range planToStreamColMap {
			planToStreamColMap[i] = -1
		}
		for i, e := range n.render {
			idx := e.(*parser.IndexedVar).Idx
			streamCol := p.planToStreamColMap[idx]
			if streamCol == -1 {
				panic(fmt.Sprintf("render %d refers to column %d not in source", i, idx))
			}
			planToStreamColMap[i] = streamCol
		}
		p.planToStreamColMap = planToStreamColMap
		return nil
	}
	// TODO(radu): add an evaluator stage.
	panic("render expressions not implemented")
}

func (dsp *distSQLPlanner) createPlanForNode(
	planCtx *planningCtx, node planNode,
) (physicalPlan, error) {
	switch n := node.(type) {
	case *scanNode:
		return dsp.createTableReaders(planCtx, n)

	case *selectNode:
		plan, err := dsp.createPlanForNode(planCtx, n.source.plan)
		if err != nil {
			return physicalPlan{}, err
		}
		if err := dsp.selectRenders(planCtx, &plan, n); err != nil {
			return physicalPlan{}, err
		}
		return plan, nil

	case *selectTopNode:
		return dsp.createPlanForNode(planCtx, n.source)

	default:
		panic(fmt.Sprintf("unsupported node type %T", n))
	}
}

// mergeResultStreams connects a set of resultStreams to a synchronizer. The
// synchronizer is configured with the provided ordering.
func (dsp *distSQLPlanner) mergeResultStreams(
	p *physicalPlan,
	resultStreams []resultStream,
	ordering distsql.Ordering,
	destProcessor, destSlot int,
) {
	proc := &p.processors[destProcessor]
	if len(ordering.Columns) == 0 || len(resultStreams) == 1 {
		proc.spec.Input[destSlot].Type = distsql.InputSyncSpec_UNORDERED
	} else {
		proc.spec.Input[destSlot].Type = distsql.InputSyncSpec_ORDERED
		proc.spec.Input[destSlot].Ordering = ordering
	}

	for _, rs := range resultStreams {
		p.streams = append(p.streams, stream{
			sourceProcessor: rs.processor,
			sourceSlot:      rs.slot,
			destProcessor:   destProcessor,
			destSlot:        destSlot,
		})
	}
}

// addFinalStage adds a final stage that brings the results back to this node.
func (dsp *distSQLPlanner) addFinalStage(p *physicalPlan) {
	thisNodeID := dsp.nodeDesc.NodeID
	if len(p.resultStreams) == 1 {
		proc := p.resultStreams[0].processor
		if p.processors[proc].node == thisNodeID {
			// What luck! The plan has a single result stream, and it's coming from a
			// processor on this node.
			return
		}
	}
	proc := processor{node: thisNodeID}
	proc.spec.Core.SetValue(&distsql.NoopCoreSpec{})

	// Add a no-op processor.
	pIdx := len(p.processors)
	p.processors = append(p.processors, proc)

	// Connect the result streams to the no-op processor.
	dsp.mergeResultStreams(p, p.resultStreams, p.ordering, pIdx, 0)

	// We now have a single result stream.
	p.resultStreams = p.resultStreams[:1]
	p.resultStreams[0] = resultStream{processor: pIdx, slot: 0}
}

// populateEndpoints processes p.streams and adds the corresponding
// StreamEndpointSpects to the processors' input and output specs.
func (dsp *distSQLPlanner) populateEndpoints(planCtx *planningCtx, p *physicalPlan) {
	for sIdx, s := range p.streams {
		p1 := &p.processors[s.sourceProcessor]
		p2 := &p.processors[s.destProcessor]
		endpoint := distsql.StreamEndpointSpec{StreamID: distsql.StreamID(sIdx)}
		if p1.node == p2.node {
			endpoint.Type = distsql.StreamEndpointSpec_LOCAL
		} else {
			endpoint.Type = distsql.StreamEndpointSpec_REMOTE
		}
		p2.spec.Input[s.destSlot].Streams = append(p2.spec.Input[s.destSlot].Streams, endpoint)
		if endpoint.Type == distsql.StreamEndpointSpec_REMOTE {
			endpoint.TargetAddr = planCtx.nodeAddresses[p2.node]
		}
		p1.spec.Output[s.sourceSlot].Streams = append(p1.spec.Output[s.sourceSlot].Streams, endpoint)
	}

	// Populate the endpoint for the result.
	resStream := p.resultStreams[0]
	proc := &p.processors[resStream.processor]
	out := &proc.spec.Output[resStream.slot]
	out.Streams = append(out.Streams, distsql.StreamEndpointSpec{
		Type: distsql.StreamEndpointSpec_SYNC_RESPONSE,
	})
}

// PlanAndRun generates a physical plan from a planNode tree and executes it. It
// assumes that the tree is supported (see CheckSupport).
//
// Note that errors that happen while actually running the flow are reported to
// recv, not returned by this function.
func (dsp *distSQLPlanner) PlanAndRun(
	ctx context.Context, txn *client.Txn, tree planNode, recv *distSQLReceiver,
) error {
	// Trigger limit propagation.
	tree.SetLimitHint(math.MaxInt64, true)

	planCtx := planningCtx{
		ctx:           ctx,
		spanIter:      dsp.spanResolver.NewSpanResolverIterator(),
		nodeAddresses: make(map[roachpb.NodeID]string),
	}
	thisNodeID := dsp.nodeDesc.NodeID
	planCtx.nodeAddresses[thisNodeID] = dsp.nodeDesc.Address.String()

	plan, err := dsp.createPlanForNode(&planCtx, tree)
	if err != nil {
		return err
	}

	dsp.addFinalStage(&plan)
	dsp.populateEndpoints(&planCtx, &plan)

	recv.resultToStreamColMap = plan.planToStreamColMap

	// Split the processors by nodeID to create the FlowSpecs.
	flowID := distsql.FlowID{UUID: uuid.MakeV4()}
	nodeIDMap := make(map[roachpb.NodeID]int)
	nodeIDs := make([]roachpb.NodeID, 0, len(planCtx.nodeAddresses))
	flows := make([]distsql.FlowSpec, 0, len(planCtx.nodeAddresses))

	for _, p := range plan.processors {
		idx, ok := nodeIDMap[p.node]
		if !ok {
			flow := distsql.FlowSpec{FlowID: flowID}
			idx = len(flows)
			flows = append(flows, flow)
			nodeIDs = append(nodeIDs, p.node)
			nodeIDMap[p.node] = idx
		}
		flows[idx].Processors = append(flows[idx].Processors, p.spec)
	}

	// Start the flows on all other nodes.
	for i, nodeID := range nodeIDs {
		if nodeID == thisNodeID {
			// Skip this node.
			continue
		}
		req := distsql.SetupFlowRequest{
			Txn:  txn.Proto,
			Flow: flows[i],
		}
		if err := distsql.SetFlowRequestTrace(ctx, &req); err != nil {
			return err
		}
		conn, err := dsp.distSQLSrv.RPCContext.GRPCDial(planCtx.nodeAddresses[nodeID])
		if err != nil {
			return err
		}
		client := distsql.NewDistSQLClient(conn)
		// TODO(radu): we are not waiting for the flows to complete, but we are
		// still waiting for a round trip; we should start the flows in parallel, at
		// least if there are enough of them.
		if resp, err := client.SetupFlow(context.Background(), &req); err != nil {
			return err
		} else if resp.Error != nil {
			return resp.Error.GoError()
		}
	}
	localReq := distsql.SetupFlowRequest{
		Txn:  txn.Proto,
		Flow: flows[nodeIDMap[thisNodeID]],
	}
	if err := distsql.SetFlowRequestTrace(ctx, &localReq); err != nil {
		return err
	}
	flow, err := dsp.distSQLSrv.SetupSyncFlow(ctx, &localReq, recv)
	if err != nil {
		return err
	}
	// TODO(radu): this should go through the flow scheduler.
	flow.Start(func() {})
	flow.Wait()
	flow.Cleanup()

	return nil
}

type distSQLReceiver struct {
	// rows is the container where we store the nodes; if we only need the count
	// of the rows, it is nil.
	rows *RowContainer
	// resultToStreamColMap maps result columns to columns in the distsql results
	// stream.
	resultToStreamColMap []int
	// numRows counts the number of rows we received when rows is nil.
	numRows int64
	err     error
	row     parser.DTuple
	alloc   sqlbase.DatumAlloc
	closed  bool
}

var _ distsql.RowReceiver = &distSQLReceiver{}

// PushRow is part of the RowReceiver interface.
func (r *distSQLReceiver) PushRow(row sqlbase.EncDatumRow) bool {
	if r.err != nil {
		return false
	}
	if r.rows == nil {
		r.numRows++
		return true
	}
	if r.row == nil {
		r.row = make(parser.DTuple, len(r.resultToStreamColMap))
	}
	for i, resIdx := range r.resultToStreamColMap {
		err := row[resIdx].Decode(&r.alloc)
		if err != nil {
			r.err = err
			return false
		}
		r.row[i] = row[resIdx].Datum
	}
	// Note that AddRow accounts for the memory used by the Datums.
	if _, err := r.rows.AddRow(r.row); err != nil {
		r.err = err
		return false
	}
	return true
}

// Close is part of the RowReceiver interface.
func (r *distSQLReceiver) Close(err error) {
	if r.closed {
		panic("double close")
	}
	r.closed = true
	if r.err == nil {
		r.err = err
	}
}
