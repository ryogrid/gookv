package mvcc

import (
	"fmt"
	"testing"
)

func TestKeyDecodeForRegionRouting(t *testing.T) {
	key := []byte("fill:aajbalance")
	lockKey := EncodeLockKey(key)
	decoded, ts, err := DecodeKey(lockKey)
	fmt.Printf("original: %q\n", key)
	fmt.Printf("lockKey:  %x (len=%d)\n", lockKey, len(lockKey))
	fmt.Printf("decoded:  %q ts=%d err=%v\n", decoded, ts, err)

	if string(decoded) != string(key) {
		t.Errorf("DecodeKey(EncodeLockKey(%q)) = %q, want %q", key, decoded, key)
	}

	encodedKey := EncodeKey(key, 12345)
	decoded2, ts2, err2 := DecodeKey(encodedKey)
	fmt.Printf("encodedKey: %x (len=%d)\n", encodedKey, len(encodedKey))
	fmt.Printf("decoded2:   %q ts=%d err=%v\n", decoded2, ts2, err2)

	if string(decoded2) != string(key) {
		t.Errorf("DecodeKey(EncodeKey(%q, 12345)) = %q, want %q", key, decoded2, key)
	}
}
