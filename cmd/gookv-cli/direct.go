package main

import (
	"context"
	"fmt"

	"github.com/pingcap/kvproto/pkg/kvrpcpb"
	"github.com/pingcap/kvproto/pkg/tikvpb"
	"github.com/ryogrid/gookv/pkg/client"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// directRawKV implements rawKVAPI by making direct gRPC calls to a single
// KV node, bypassing PD-based region routing. Used with the --addr flag.
type directRawKV struct {
	tikv tikvpb.TikvClient
	conn *grpc.ClientConn
}

func newDirectRawKV(addr string) (*directRawKV, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", addr, err)
	}
	return &directRawKV{
		tikv: tikvpb.NewTikvClient(conn),
		conn: conn,
	}, nil
}

func (d *directRawKV) Close() error {
	return d.conn.Close()
}

func (d *directRawKV) Get(ctx context.Context, key []byte) ([]byte, bool, error) {
	resp, err := d.tikv.RawGet(ctx, &kvrpcpb.RawGetRequest{Key: key})
	if err != nil {
		return nil, false, err
	}
	if resp.GetError() != "" {
		return nil, false, fmt.Errorf("RawGet: %s", resp.GetError())
	}
	if resp.GetNotFound() {
		return nil, true, nil
	}
	return resp.GetValue(), false, nil
}

func (d *directRawKV) Put(ctx context.Context, key, value []byte) error {
	resp, err := d.tikv.RawPut(ctx, &kvrpcpb.RawPutRequest{Key: key, Value: value})
	if err != nil {
		return err
	}
	if resp.GetError() != "" {
		return fmt.Errorf("RawPut: %s", resp.GetError())
	}
	return nil
}

func (d *directRawKV) PutWithTTL(ctx context.Context, key, value []byte, ttl uint64) error {
	resp, err := d.tikv.RawPut(ctx, &kvrpcpb.RawPutRequest{Key: key, Value: value, Ttl: ttl})
	if err != nil {
		return err
	}
	if resp.GetError() != "" {
		return fmt.Errorf("RawPut: %s", resp.GetError())
	}
	return nil
}

func (d *directRawKV) Delete(ctx context.Context, key []byte) error {
	resp, err := d.tikv.RawDelete(ctx, &kvrpcpb.RawDeleteRequest{Key: key})
	if err != nil {
		return err
	}
	if resp.GetError() != "" {
		return fmt.Errorf("RawDelete: %s", resp.GetError())
	}
	return nil
}

func (d *directRawKV) GetKeyTTL(ctx context.Context, key []byte) (uint64, error) {
	resp, err := d.tikv.RawGetKeyTTL(ctx, &kvrpcpb.RawGetKeyTTLRequest{Key: key})
	if err != nil {
		return 0, err
	}
	if resp.GetNotFound() {
		return 0, fmt.Errorf("key not found")
	}
	return resp.GetTtl(), nil
}

func (d *directRawKV) BatchGet(ctx context.Context, keys [][]byte) ([]client.KvPair, error) {
	resp, err := d.tikv.RawBatchGet(ctx, &kvrpcpb.RawBatchGetRequest{Keys: keys})
	if err != nil {
		return nil, err
	}
	pairs := make([]client.KvPair, 0, len(resp.GetPairs()))
	for _, p := range resp.GetPairs() {
		if p.GetError() != nil {
			continue
		}
		pairs = append(pairs, client.KvPair{Key: p.GetKey(), Value: p.GetValue()})
	}
	return pairs, nil
}

func (d *directRawKV) BatchPut(ctx context.Context, pairs []client.KvPair) error {
	kvPairs := make([]*kvrpcpb.KvPair, len(pairs))
	for i, p := range pairs {
		kvPairs[i] = &kvrpcpb.KvPair{Key: p.Key, Value: p.Value}
	}
	resp, err := d.tikv.RawBatchPut(ctx, &kvrpcpb.RawBatchPutRequest{Pairs: kvPairs})
	if err != nil {
		return err
	}
	if resp.GetError() != "" {
		return fmt.Errorf("RawBatchPut: %s", resp.GetError())
	}
	return nil
}

func (d *directRawKV) BatchDelete(ctx context.Context, keys [][]byte) error {
	resp, err := d.tikv.RawBatchDelete(ctx, &kvrpcpb.RawBatchDeleteRequest{Keys: keys})
	if err != nil {
		return err
	}
	if resp.GetError() != "" {
		return fmt.Errorf("RawBatchDelete: %s", resp.GetError())
	}
	return nil
}

func (d *directRawKV) Scan(ctx context.Context, startKey, endKey []byte, limit int) ([]client.KvPair, error) {
	resp, err := d.tikv.RawScan(ctx, &kvrpcpb.RawScanRequest{
		StartKey: startKey,
		EndKey:   endKey,
		Limit:    uint32(limit),
	})
	if err != nil {
		return nil, err
	}
	pairs := make([]client.KvPair, 0, len(resp.GetKvs()))
	for _, p := range resp.GetKvs() {
		if p.GetError() != nil {
			continue
		}
		pairs = append(pairs, client.KvPair{Key: p.GetKey(), Value: p.GetValue()})
	}
	return pairs, nil
}

func (d *directRawKV) DeleteRange(ctx context.Context, startKey, endKey []byte) error {
	resp, err := d.tikv.RawDeleteRange(ctx, &kvrpcpb.RawDeleteRangeRequest{
		StartKey: startKey,
		EndKey:   endKey,
	})
	if err != nil {
		return err
	}
	if resp.GetError() != "" {
		return fmt.Errorf("RawDeleteRange: %s", resp.GetError())
	}
	return nil
}

func (d *directRawKV) CompareAndSwap(ctx context.Context, key, value, prevValue []byte, prevNotExist bool) (bool, []byte, error) {
	resp, err := d.tikv.RawCompareAndSwap(ctx, &kvrpcpb.RawCASRequest{
		Key:            key,
		Value:          value,
		PreviousValue:  prevValue,
		PreviousNotExist: prevNotExist,
	})
	if err != nil {
		return false, nil, err
	}
	if resp.GetError() != "" {
		return false, nil, fmt.Errorf("RawCAS: %s", resp.GetError())
	}
	return resp.GetSucceed(), resp.GetPreviousValue(), nil
}

func (d *directRawKV) Checksum(ctx context.Context, startKey, endKey []byte) (uint64, uint64, uint64, error) {
	resp, err := d.tikv.RawChecksum(ctx, &kvrpcpb.RawChecksumRequest{
		Ranges: []*kvrpcpb.KeyRange{{StartKey: startKey, EndKey: endKey}},
	})
	if err != nil {
		return 0, 0, 0, err
	}
	return resp.GetChecksum(), resp.GetTotalKvs(), resp.GetTotalBytes(), nil
}
