package coprocessor

import (
	"context"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/ryogrid/gookv/internal/engine/traits"
	"github.com/ryogrid/gookv/pkg/txntypes"
)

// Request type constants matching TiKV/TiDB protobuf.
const (
	ReqTypeDAG      int64 = 103
	ReqTypeAnalyze  int64 = 104
	ReqTypeChecksum int64 = 105
)

// ExecType identifies a coprocessor executor.
type ExecType int

const (
	ExecTypeTableScan   ExecType = 1
	ExecTypeSelection   ExecType = 2
	ExecTypeLimit       ExecType = 3
	ExecTypeAggregation ExecType = 4
)

// DAGRequest represents a decoded push-down query plan.
type DAGRequest struct {
	Executors     []ExecutorDesc
	OutputOffsets []uint32
	StartTS       txntypes.TimeStamp
	PagingSize    uint64
}

// ExecutorDesc describes a single executor in the pipeline.
type ExecutorDesc struct {
	Type        ExecType
	TableScan   *TableScanDesc
	Selection   *SelectionDesc
	Limit       *LimitDesc
	Aggregation *AggregationDesc
}

// TableScanDesc describes a table scan.
type TableScanDesc struct {
	ColCount int
	Desc     bool
}

// SelectionDesc describes a selection (filter).
type SelectionDesc struct {
	Predicates []*RPNExpression
}

// LimitDesc describes a limit.
type LimitDesc struct {
	Limit uint64
}

// AggregationDesc describes an aggregation.
type AggregationDesc struct {
	AggrDefs  []AggrDef
	GroupCols []int
}

// CopKeyRange represents a key range from a coprocessor request.
type CopKeyRange struct {
	Start []byte
	End   []byte
}

// Handle processes a coprocessor request and returns a response.
// The response data contains encoded rows from the executor pipeline.
func (ep *Endpoint) Handle(ctx context.Context, tp int64, data []byte, startTS uint64, ranges []CopKeyRange) (*CopResponse, error) {
	resp := &CopResponse{}

	if tp != ReqTypeDAG {
		resp.OtherError = fmt.Sprintf("unsupported request type: %d", tp)
		return resp, nil
	}

	dag, err := DecodeDAGRequest(data, txntypes.TimeStamp(startTS))
	if err != nil {
		resp.OtherError = err.Error()
		return resp, nil
	}

	// Convert key ranges.
	krs := make([]KeyRange, len(ranges))
	for i, r := range ranges {
		krs[i] = KeyRange{Start: r.Start, End: r.End}
	}

	snap := ep.engine.NewSnapshot()
	defer snap.Close()

	pipeline, err := ep.BuildPipeline(dag, snap, krs)
	if err != nil {
		resp.OtherError = err.Error()
		return resp, nil
	}

	deadline := time.Time{}
	if dl, ok := ctx.Deadline(); ok {
		deadline = dl
	}

	runner := NewExecutorsRunner(pipeline, deadline)
	rows, err := runner.Run(ctx)
	if err != nil {
		resp.OtherError = err.Error()
		return resp, nil
	}

	resp.Data = EncodeSelectResponse(rows, dag.OutputOffsets)
	return resp, nil
}

// HandleStream processes a coprocessor request and sends results incrementally.
func (ep *Endpoint) HandleStream(
	ctx context.Context,
	tp int64,
	data []byte,
	startTS uint64,
	ranges []CopKeyRange,
	sender func(*CopResponse) error,
) error {
	if tp != ReqTypeDAG {
		return sender(&CopResponse{OtherError: fmt.Sprintf("unsupported request type: %d", tp)})
	}

	dag, err := DecodeDAGRequest(data, txntypes.TimeStamp(startTS))
	if err != nil {
		return sender(&CopResponse{OtherError: err.Error()})
	}

	krs := make([]KeyRange, len(ranges))
	for i, r := range ranges {
		krs[i] = KeyRange{Start: r.Start, End: r.End}
	}

	snap := ep.engine.NewSnapshot()
	defer snap.Close()

	pipeline, err := ep.BuildPipeline(dag, snap, krs)
	if err != nil {
		return sender(&CopResponse{OtherError: err.Error()})
	}

	batchSize := 32
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		result, err := pipeline.NextBatch(ctx, batchSize)
		if err != nil {
			return err
		}

		if len(result.Rows) > 0 {
			resp := &CopResponse{
				Data: EncodeSelectResponse(result.Rows, dag.OutputOffsets),
			}
			if err := sender(resp); err != nil {
				return err
			}
		}

		if result.IsDrained {
			break
		}

		batchSize *= 2
		if batchSize > 1024 {
			batchSize = 1024
		}
	}

	return nil
}

// CopResponse is our internal representation of a coprocessor response.
type CopResponse struct {
	Data       []byte
	OtherError string
}

// BuildPipeline constructs the executor chain from DAGRequest descriptors.
func (ep *Endpoint) BuildPipeline(
	dag *DAGRequest,
	snap traits.Snapshot,
	ranges []KeyRange,
) (BatchExecutor, error) {
	if len(dag.Executors) == 0 {
		return nil, ErrUnsupportedExecutor
	}

	// First executor must be a scan (leaf).
	first := dag.Executors[0]
	var exec BatchExecutor

	switch first.Type {
	case ExecTypeTableScan:
		if first.TableScan == nil {
			return nil, fmt.Errorf("%w: TableScan descriptor is nil", ErrUnsupportedExecutor)
		}
		exec = NewTableScanExecutor(snap, ranges, dag.StartTS,
			first.TableScan.ColCount, first.TableScan.Desc)
	default:
		return nil, fmt.Errorf("%w: first executor must be TableScan, got %d", ErrUnsupportedExecutor, first.Type)
	}

	// Wrap with subsequent executors.
	for _, desc := range dag.Executors[1:] {
		switch desc.Type {
		case ExecTypeSelection:
			if desc.Selection == nil {
				return nil, fmt.Errorf("%w: Selection descriptor is nil", ErrUnsupportedExecutor)
			}
			exec = NewSelectionExecutor(exec, desc.Selection.Predicates)
		case ExecTypeLimit:
			if desc.Limit == nil {
				return nil, fmt.Errorf("%w: Limit descriptor is nil", ErrUnsupportedExecutor)
			}
			exec = NewLimitExecutor(exec, int(desc.Limit.Limit))
		case ExecTypeAggregation:
			if desc.Aggregation == nil {
				return nil, fmt.Errorf("%w: Aggregation descriptor is nil", ErrUnsupportedExecutor)
			}
			if len(desc.Aggregation.GroupCols) > 0 {
				exec = NewHashAggrExecutor(exec, desc.Aggregation.GroupCols, desc.Aggregation.AggrDefs)
			} else {
				exec = NewSimpleAggrExecutor(exec, desc.Aggregation.AggrDefs)
			}
		default:
			return nil, fmt.Errorf("%w: executor type %d", ErrUnsupportedExecutor, desc.Type)
		}
	}

	return exec, nil
}

// DecodeDAGRequest parses a binary-encoded DAGRequest.
// Format (simplified, not TiDB tipb proto):
//
//	[1 byte: executor count]
//	for each executor:
//	  [1 byte: ExecType]
//	  [type-specific data]
//	[2 bytes: output offset count]
//	for each output offset:
//	  [4 bytes: offset uint32]
func DecodeDAGRequest(data []byte, startTS txntypes.TimeStamp) (*DAGRequest, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("coprocessor: empty DAGRequest data")
	}

	dag := &DAGRequest{StartTS: startTS}
	pos := 0

	// Executor count.
	if pos >= len(data) {
		return nil, fmt.Errorf("coprocessor: truncated DAGRequest")
	}
	execCount := int(data[pos])
	pos++

	for i := 0; i < execCount; i++ {
		if pos >= len(data) {
			return nil, fmt.Errorf("coprocessor: truncated executor %d", i)
		}
		execType := ExecType(data[pos])
		pos++

		desc := ExecutorDesc{Type: execType}
		switch execType {
		case ExecTypeTableScan:
			if pos+5 > len(data) {
				return nil, fmt.Errorf("coprocessor: truncated TableScan")
			}
			colCount := int(binary.BigEndian.Uint32(data[pos : pos+4]))
			pos += 4
			descending := data[pos] != 0
			pos++
			desc.TableScan = &TableScanDesc{ColCount: colCount, Desc: descending}

		case ExecTypeSelection:
			// Selection: [1 byte: predicate count][predicates...]
			// Each predicate: [2 bytes: data len][RPN data]
			if pos >= len(data) {
				return nil, fmt.Errorf("coprocessor: truncated Selection")
			}
			predCount := int(data[pos])
			pos++
			var predicates []*RPNExpression
			for j := 0; j < predCount; j++ {
				if pos+2 > len(data) {
					return nil, fmt.Errorf("coprocessor: truncated Selection predicate %d", j)
				}
				dataLen := int(binary.BigEndian.Uint16(data[pos : pos+2]))
				pos += 2
				if pos+dataLen > len(data) {
					return nil, fmt.Errorf("coprocessor: truncated Selection predicate data %d", j)
				}
				expr, err := DecodeRPNExpression(data[pos : pos+dataLen])
				if err != nil {
					return nil, fmt.Errorf("coprocessor: decode predicate %d: %w", j, err)
				}
				predicates = append(predicates, expr)
				pos += dataLen
			}
			desc.Selection = &SelectionDesc{Predicates: predicates}

		case ExecTypeLimit:
			if pos+8 > len(data) {
				return nil, fmt.Errorf("coprocessor: truncated Limit")
			}
			limit := binary.BigEndian.Uint64(data[pos : pos+8])
			pos += 8
			desc.Limit = &LimitDesc{Limit: limit}

		case ExecTypeAggregation:
			// [1 byte: aggr count][aggr defs...][1 byte: group col count][group cols...]
			if pos >= len(data) {
				return nil, fmt.Errorf("coprocessor: truncated Aggregation")
			}
			aggrCount := int(data[pos])
			pos++
			var aggrDefs []AggrDef
			for j := 0; j < aggrCount; j++ {
				if pos+2 > len(data) {
					return nil, fmt.Errorf("coprocessor: truncated AggrDef %d", j)
				}
				funcType := AggrFuncType(data[pos])
				colIdx := int(data[pos+1])
				pos += 2
				aggrDefs = append(aggrDefs, AggrDef{FuncType: funcType, ColIdx: colIdx})
			}
			if pos >= len(data) {
				return nil, fmt.Errorf("coprocessor: truncated Aggregation group cols")
			}
			groupCount := int(data[pos])
			pos++
			var groupCols []int
			for j := 0; j < groupCount; j++ {
				if pos >= len(data) {
					return nil, fmt.Errorf("coprocessor: truncated group col %d", j)
				}
				groupCols = append(groupCols, int(data[pos]))
				pos++
			}
			desc.Aggregation = &AggregationDesc{AggrDefs: aggrDefs, GroupCols: groupCols}

		default:
			return nil, fmt.Errorf("%w: unknown executor type %d", ErrUnsupportedExecutor, execType)
		}

		dag.Executors = append(dag.Executors, desc)
	}

	// Output offsets.
	if pos+2 <= len(data) {
		offsetCount := int(binary.BigEndian.Uint16(data[pos : pos+2]))
		pos += 2
		for i := 0; i < offsetCount; i++ {
			if pos+4 > len(data) {
				break
			}
			dag.OutputOffsets = append(dag.OutputOffsets, binary.BigEndian.Uint32(data[pos:pos+4]))
			pos += 4
		}
	}

	return dag, nil
}

// EncodeDAGRequest serializes a DAGRequest for testing purposes.
func EncodeDAGRequest(dag *DAGRequest) []byte {
	var buf []byte
	buf = append(buf, byte(len(dag.Executors)))

	for _, desc := range dag.Executors {
		buf = append(buf, byte(desc.Type))
		switch desc.Type {
		case ExecTypeTableScan:
			b := make([]byte, 4)
			binary.BigEndian.PutUint32(b, uint32(desc.TableScan.ColCount))
			buf = append(buf, b...)
			if desc.TableScan.Desc {
				buf = append(buf, 1)
			} else {
				buf = append(buf, 0)
			}
		case ExecTypeLimit:
			b := make([]byte, 8)
			binary.BigEndian.PutUint64(b, desc.Limit.Limit)
			buf = append(buf, b...)
		case ExecTypeSelection:
			buf = append(buf, byte(len(desc.Selection.Predicates)))
			for _, pred := range desc.Selection.Predicates {
				encoded := EncodeRPNExpression(pred)
				b := make([]byte, 2)
				binary.BigEndian.PutUint16(b, uint16(len(encoded)))
				buf = append(buf, b...)
				buf = append(buf, encoded...)
			}
		case ExecTypeAggregation:
			buf = append(buf, byte(len(desc.Aggregation.AggrDefs)))
			for _, ad := range desc.Aggregation.AggrDefs {
				buf = append(buf, byte(ad.FuncType), byte(ad.ColIdx))
			}
			buf = append(buf, byte(len(desc.Aggregation.GroupCols)))
			for _, gc := range desc.Aggregation.GroupCols {
				buf = append(buf, byte(gc))
			}
		}
	}

	// Output offsets.
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, uint16(len(dag.OutputOffsets)))
	buf = append(buf, b...)
	for _, off := range dag.OutputOffsets {
		b := make([]byte, 4)
		binary.BigEndian.PutUint32(b, off)
		buf = append(buf, b...)
	}

	return buf
}

// DecodeRPNExpression decodes an RPN expression from binary.
// Format: [1 byte: node count] then for each node: [1 byte: type][type-specific data].
func DecodeRPNExpression(data []byte) (*RPNExpression, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty RPN expression")
	}
	nodeCount := int(data[0])
	pos := 1
	var nodes []RPNNode

	for i := 0; i < nodeCount; i++ {
		if pos >= len(data) {
			return nil, fmt.Errorf("truncated RPN node %d", i)
		}
		nodeType := data[pos]
		pos++

		switch nodeType {
		case 0x01: // ColumnRef
			if pos >= len(data) {
				return nil, fmt.Errorf("truncated ColumnRef")
			}
			colIdx := int(data[pos])
			pos++
			nodes = append(nodes, RPNNode{Type: RPNColumnRef, ColIdx: colIdx})

		case 0x02: // Constant Int64
			if pos+8 > len(data) {
				return nil, fmt.Errorf("truncated Int64 constant")
			}
			v := int64(binary.BigEndian.Uint64(data[pos : pos+8]))
			pos += 8
			nodes = append(nodes, RPNNode{Type: RPNConstant, Constant: Int64Datum(v)})

		case 0x03: // Constant String
			if pos+2 > len(data) {
				return nil, fmt.Errorf("truncated String constant length")
			}
			slen := int(binary.BigEndian.Uint16(data[pos : pos+2]))
			pos += 2
			if pos+slen > len(data) {
				return nil, fmt.Errorf("truncated String constant data")
			}
			s := string(data[pos : pos+slen])
			pos += slen
			nodes = append(nodes, RPNNode{Type: RPNConstant, Constant: StringDatum(s)})

		case 0x10: // FuncCall
			if pos >= len(data) {
				return nil, fmt.Errorf("truncated FuncCall")
			}
			funcType := RPNFuncType(data[pos])
			pos++
			nodes = append(nodes, RPNNode{Type: RPNFuncCall, FuncType: funcType})

		default:
			return nil, fmt.Errorf("unknown RPN node type: %d", nodeType)
		}
	}

	return &RPNExpression{Nodes: nodes}, nil
}

// EncodeRPNExpression serializes an RPN expression for testing.
func EncodeRPNExpression(expr *RPNExpression) []byte {
	var buf []byte
	buf = append(buf, byte(len(expr.Nodes)))

	for _, node := range expr.Nodes {
		switch node.Type {
		case RPNColumnRef:
			buf = append(buf, 0x01, byte(node.ColIdx))
		case RPNConstant:
			switch node.Constant.Kind {
			case KindInt64:
				buf = append(buf, 0x02)
				b := make([]byte, 8)
				binary.BigEndian.PutUint64(b, uint64(node.Constant.I64))
				buf = append(buf, b...)
			case KindString:
				buf = append(buf, 0x03)
				b := make([]byte, 2)
				binary.BigEndian.PutUint16(b, uint16(len(node.Constant.Str)))
				buf = append(buf, b...)
				buf = append(buf, []byte(node.Constant.Str)...)
			}
		case RPNFuncCall:
			buf = append(buf, 0x10, byte(node.FuncType))
		}
	}

	return buf
}

// EncodeSelectResponse encodes rows into binary response data.
// Format: [4 bytes: row count] then for each row: [encoded datums].
func EncodeSelectResponse(rows []Row, outputOffsets []uint32) []byte {
	var buf []byte
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(len(rows)))
	buf = append(buf, b...)

	for _, row := range rows {
		// Select only the output columns.
		var cols []Datum
		if len(outputOffsets) > 0 {
			for _, off := range outputOffsets {
				if int(off) < len(row) {
					cols = append(cols, row[off])
				}
			}
		} else {
			cols = row
		}

		// Encode column count.
		buf = append(buf, byte(len(cols)))
		for _, d := range cols {
			buf = append(buf, encodeDatum(d)...)
		}
	}

	return buf
}

// DecodeSelectResponse decodes binary response data into rows.
func DecodeSelectResponse(data []byte) ([]Row, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("truncated select response")
	}
	rowCount := int(binary.BigEndian.Uint32(data[0:4]))
	pos := 4

	var rows []Row
	for i := 0; i < rowCount; i++ {
		if pos >= len(data) {
			return nil, fmt.Errorf("truncated row %d", i)
		}
		colCount := int(data[pos])
		pos++

		var row Row
		for j := 0; j < colCount; j++ {
			d, n, err := decodeDatum(data[pos:])
			if err != nil {
				return nil, fmt.Errorf("decode datum row %d col %d: %w", i, j, err)
			}
			row = append(row, d)
			pos += n
		}
		rows = append(rows, row)
	}

	return rows, nil
}

func encodeDatum(d Datum) []byte {
	switch d.Kind {
	case KindNull:
		return []byte{0x00}
	case KindInt64:
		buf := make([]byte, 9)
		buf[0] = 0x01
		binary.BigEndian.PutUint64(buf[1:], uint64(d.I64))
		return buf
	case KindUint64:
		buf := make([]byte, 9)
		buf[0] = 0x02
		binary.BigEndian.PutUint64(buf[1:], d.U64)
		return buf
	case KindFloat64:
		buf := make([]byte, 9)
		buf[0] = 0x03
		binary.BigEndian.PutUint64(buf[1:], uint64(d.I64)) // float bits via I64
		return buf
	case KindString:
		buf := make([]byte, 3+len(d.Str))
		buf[0] = 0x04
		binary.BigEndian.PutUint16(buf[1:3], uint16(len(d.Str)))
		copy(buf[3:], d.Str)
		return buf
	case KindBytes:
		buf := make([]byte, 3+len(d.Buf))
		buf[0] = 0x05
		binary.BigEndian.PutUint16(buf[1:3], uint16(len(d.Buf)))
		copy(buf[3:], d.Buf)
		return buf
	default:
		return []byte{0x00}
	}
}

func decodeDatum(data []byte) (Datum, int, error) {
	if len(data) == 0 {
		return Datum{}, 0, fmt.Errorf("empty datum")
	}
	switch data[0] {
	case 0x00: // Null
		return NullDatum(), 1, nil
	case 0x01: // Int64
		if len(data) < 9 {
			return Datum{}, 0, fmt.Errorf("truncated int64")
		}
		v := int64(binary.BigEndian.Uint64(data[1:9]))
		return Int64Datum(v), 9, nil
	case 0x02: // Uint64
		if len(data) < 9 {
			return Datum{}, 0, fmt.Errorf("truncated uint64")
		}
		v := binary.BigEndian.Uint64(data[1:9])
		return Uint64Datum(v), 9, nil
	case 0x03: // Float64
		if len(data) < 9 {
			return Datum{}, 0, fmt.Errorf("truncated float64")
		}
		v := int64(binary.BigEndian.Uint64(data[1:9]))
		return Datum{Kind: KindFloat64, I64: v}, 9, nil
	case 0x04: // String
		if len(data) < 3 {
			return Datum{}, 0, fmt.Errorf("truncated string length")
		}
		slen := int(binary.BigEndian.Uint16(data[1:3]))
		if len(data) < 3+slen {
			return Datum{}, 0, fmt.Errorf("truncated string data")
		}
		return StringDatum(string(data[3 : 3+slen])), 3 + slen, nil
	case 0x05: // Bytes
		if len(data) < 3 {
			return Datum{}, 0, fmt.Errorf("truncated bytes length")
		}
		blen := int(binary.BigEndian.Uint16(data[1:3]))
		if len(data) < 3+blen {
			return Datum{}, 0, fmt.Errorf("truncated bytes data")
		}
		return BytesDatum(append([]byte(nil), data[3:3+blen]...)), 3 + blen, nil
	default:
		return Datum{}, 0, fmt.Errorf("unknown datum type: %d", data[0])
	}
}
