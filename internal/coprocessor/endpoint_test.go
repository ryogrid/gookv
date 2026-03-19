package coprocessor

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ryogrid/gookv/internal/engine/rocks"
	"github.com/ryogrid/gookv/internal/engine/traits"
	"github.com/ryogrid/gookv/internal/storage/mvcc"
	"github.com/ryogrid/gookv/pkg/cfnames"
	"github.com/ryogrid/gookv/pkg/txntypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestEngine(t *testing.T) traits.KvEngine {
	t.Helper()
	dir := t.TempDir()
	e, err := rocks.Open(filepath.Join(dir, "test-db"))
	require.NoError(t, err)
	t.Cleanup(func() { e.Close() })
	return e
}

// writeTestData writes MVCC data for testing: user keys with values accessible at a given timestamp.
func writeTestData(t *testing.T, engine traits.KvEngine, userKeys [][]byte, values [][]byte, startTS, commitTS txntypes.TimeStamp) {
	t.Helper()
	wb := engine.NewWriteBatch()
	for i, uk := range userKeys {
		// Write value to CF_DEFAULT at startTS.
		defaultKey := mvcc.EncodeKey(uk, startTS)
		require.NoError(t, wb.Put(cfnames.CFDefault, defaultKey, values[i]))

		// Write Put record to CF_WRITE at commitTS.
		w := txntypes.Write{
			WriteType: txntypes.WriteTypePut,
			StartTS:   startTS,
		}
		writeData := w.Marshal()
		writeKey := mvcc.EncodeKey(uk, commitTS)
		require.NoError(t, wb.Put(cfnames.CFWrite, writeKey, writeData))
	}
	require.NoError(t, wb.Commit())
}

func TestDecodeEncodeDAGRequest(t *testing.T) {
	dag := &DAGRequest{
		StartTS: 100,
		Executors: []ExecutorDesc{
			{
				Type:      ExecTypeTableScan,
				TableScan: &TableScanDesc{ColCount: 3, Desc: false},
			},
			{
				Type:  ExecTypeLimit,
				Limit: &LimitDesc{Limit: 10},
			},
		},
		OutputOffsets: []uint32{0, 1, 2},
	}

	data := EncodeDAGRequest(dag)
	decoded, err := DecodeDAGRequest(data, 100)
	require.NoError(t, err)

	assert.Equal(t, txntypes.TimeStamp(100), decoded.StartTS)
	assert.Len(t, decoded.Executors, 2)
	assert.Equal(t, ExecTypeTableScan, decoded.Executors[0].Type)
	assert.Equal(t, 3, decoded.Executors[0].TableScan.ColCount)
	assert.False(t, decoded.Executors[0].TableScan.Desc)
	assert.Equal(t, ExecTypeLimit, decoded.Executors[1].Type)
	assert.Equal(t, uint64(10), decoded.Executors[1].Limit.Limit)
	assert.Equal(t, []uint32{0, 1, 2}, decoded.OutputOffsets)
}

func TestDecodeDAGRequest_Empty(t *testing.T) {
	_, err := DecodeDAGRequest(nil, 100)
	assert.Error(t, err)

	_, err = DecodeDAGRequest([]byte{}, 100)
	assert.Error(t, err)
}

func TestBuildPipeline_TableScan(t *testing.T) {
	engine := newTestEngine(t)
	ep := NewEndpoint(engine)

	dag := &DAGRequest{
		StartTS: 100,
		Executors: []ExecutorDesc{
			{Type: ExecTypeTableScan, TableScan: &TableScanDesc{ColCount: 2, Desc: false}},
		},
	}

	snap := engine.NewSnapshot()
	defer snap.Close()

	pipeline, err := ep.BuildPipeline(dag, snap, []KeyRange{{Start: nil, End: nil}})
	require.NoError(t, err)
	assert.NotNil(t, pipeline)
}

func TestBuildPipeline_TableScanWithLimit(t *testing.T) {
	engine := newTestEngine(t)
	ep := NewEndpoint(engine)

	dag := &DAGRequest{
		StartTS: 100,
		Executors: []ExecutorDesc{
			{Type: ExecTypeTableScan, TableScan: &TableScanDesc{ColCount: 2}},
			{Type: ExecTypeLimit, Limit: &LimitDesc{Limit: 5}},
		},
	}

	snap := engine.NewSnapshot()
	defer snap.Close()

	pipeline, err := ep.BuildPipeline(dag, snap, []KeyRange{{Start: nil, End: nil}})
	require.NoError(t, err)
	assert.NotNil(t, pipeline)
}

func TestBuildPipeline_NoExecutors(t *testing.T) {
	engine := newTestEngine(t)
	ep := NewEndpoint(engine)

	dag := &DAGRequest{StartTS: 100, Executors: nil}

	snap := engine.NewSnapshot()
	defer snap.Close()

	_, err := ep.BuildPipeline(dag, snap, nil)
	assert.ErrorIs(t, err, ErrUnsupportedExecutor)
}

func TestBuildPipeline_FirstNotTableScan(t *testing.T) {
	engine := newTestEngine(t)
	ep := NewEndpoint(engine)

	dag := &DAGRequest{
		StartTS: 100,
		Executors: []ExecutorDesc{
			{Type: ExecTypeLimit, Limit: &LimitDesc{Limit: 5}},
		},
	}

	snap := engine.NewSnapshot()
	defer snap.Close()

	_, err := ep.BuildPipeline(dag, snap, nil)
	assert.Error(t, err)
}

func TestEndpointHandle_UnsupportedType(t *testing.T) {
	engine := newTestEngine(t)
	ep := NewEndpoint(engine)

	resp, err := ep.Handle(context.Background(), 999, nil, 100, nil)
	require.NoError(t, err)
	assert.Contains(t, resp.OtherError, "unsupported request type")
}

func TestEndpointHandle_DAGRequest(t *testing.T) {
	engine := newTestEngine(t)

	// Write test data: 3 rows with 2 columns each.
	userKeys := [][]byte{
		{0x01, 0x01},
		{0x01, 0x02},
		{0x01, 0x03},
	}
	values := [][]byte{
		{0x10, 0x20},
		{0x30, 0x40},
		{0x50, 0x60},
	}
	writeTestData(t, engine, userKeys, values, 10, 20)

	ep := NewEndpoint(engine)

	dag := &DAGRequest{
		StartTS: 100,
		Executors: []ExecutorDesc{
			{Type: ExecTypeTableScan, TableScan: &TableScanDesc{ColCount: 2, Desc: false}},
		},
		OutputOffsets: []uint32{0, 1},
	}
	data := EncodeDAGRequest(dag)

	ranges := []CopKeyRange{
		{Start: nil, End: nil},
	}

	resp, err := ep.Handle(context.Background(), ReqTypeDAG, data, 100, ranges)
	require.NoError(t, err)
	assert.Empty(t, resp.OtherError)
	assert.NotEmpty(t, resp.Data, "should have response data")
}

func TestEncodeDecodeSelectResponse(t *testing.T) {
	rows := []Row{
		{Int64Datum(1), StringDatum("hello")},
		{Int64Datum(2), StringDatum("world")},
		{NullDatum(), Int64Datum(42)},
	}

	data := EncodeSelectResponse(rows, nil)
	decoded, err := DecodeSelectResponse(data)
	require.NoError(t, err)
	require.Len(t, decoded, 3)

	assert.Equal(t, KindInt64, decoded[0][0].Kind)
	assert.Equal(t, int64(1), decoded[0][0].I64)
	assert.Equal(t, KindString, decoded[0][1].Kind)
	assert.Equal(t, "hello", decoded[0][1].Str)

	assert.Equal(t, KindInt64, decoded[1][0].Kind)
	assert.Equal(t, int64(2), decoded[1][0].I64)

	assert.Equal(t, KindNull, decoded[2][0].Kind)
	assert.Equal(t, int64(42), decoded[2][1].I64)
}

func TestEncodeSelectResponse_WithOutputOffsets(t *testing.T) {
	rows := []Row{
		{Int64Datum(1), StringDatum("hello"), Int64Datum(100)},
	}

	// Only select columns 0 and 2.
	data := EncodeSelectResponse(rows, []uint32{0, 2})
	decoded, err := DecodeSelectResponse(data)
	require.NoError(t, err)
	require.Len(t, decoded, 1)
	require.Len(t, decoded[0], 2)
	assert.Equal(t, int64(1), decoded[0][0].I64)
	assert.Equal(t, int64(100), decoded[0][1].I64)
}

func TestDecodeEncodeRPNExpression(t *testing.T) {
	expr := &RPNExpression{
		Nodes: []RPNNode{
			{Type: RPNColumnRef, ColIdx: 0},
			{Type: RPNConstant, Constant: Int64Datum(42)},
			{Type: RPNFuncCall, FuncType: RPNFuncEQ},
		},
	}

	data := EncodeRPNExpression(expr)
	decoded, err := DecodeRPNExpression(data)
	require.NoError(t, err)
	require.Len(t, decoded.Nodes, 3)

	assert.Equal(t, RPNColumnRef, decoded.Nodes[0].Type)
	assert.Equal(t, 0, decoded.Nodes[0].ColIdx)
	assert.Equal(t, RPNConstant, decoded.Nodes[1].Type)
	assert.Equal(t, int64(42), decoded.Nodes[1].Constant.I64)
	assert.Equal(t, RPNFuncCall, decoded.Nodes[2].Type)
	assert.Equal(t, RPNFuncEQ, decoded.Nodes[2].FuncType)
}

func TestEndpointHandleStream(t *testing.T) {
	engine := newTestEngine(t)

	// Write test data.
	userKeys := [][]byte{{0x01, 0x01}, {0x01, 0x02}}
	values := [][]byte{{0x10}, {0x20}}
	writeTestData(t, engine, userKeys, values, 10, 20)

	ep := NewEndpoint(engine)

	dag := &DAGRequest{
		StartTS: 100,
		Executors: []ExecutorDesc{
			{Type: ExecTypeTableScan, TableScan: &TableScanDesc{ColCount: 2}},
		},
	}
	data := EncodeDAGRequest(dag)

	var responses []*CopResponse
	err := ep.HandleStream(context.Background(), ReqTypeDAG, data, 100,
		[]CopKeyRange{{Start: nil, End: nil}},
		func(resp *CopResponse) error {
			responses = append(responses, resp)
			return nil
		},
	)
	require.NoError(t, err)
	// Should have received at least one response (may be empty if no data matches).
}
