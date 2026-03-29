package e2elib

import (
	"testing"
)

func TestCountTableRows(t *testing.T) {
	// STORE LIST output example
	output := `+---------+------------------+-------+
| StoreID | Address          | State |
+---------+------------------+-------+
|       1 | 127.0.0.1:20160  | Up    |
|       2 | 127.0.0.1:20161  | Up    |
|       3 | 127.0.0.1:20162  | Up    |
+---------+------------------+-------+
(3 stores)`
	count := countTableRows(output)
	if count != 3 {
		t.Errorf("countTableRows = %d, want 3", count)
	}
}

func TestCountTableRowsEmpty(t *testing.T) {
	output := `+---------+
| StoreID |
+---------+
+---------+
(0 stores)`
	count := countTableRows(output)
	if count != 0 {
		t.Errorf("countTableRows = %d, want 0", count)
	}
}

func TestParseScalar(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"123", "123"},
		{"true", "true"},
		{"false", "false"},
		{`"hello"`, "hello"},
		{"TTL: 0 (no expiration)", "TTL: 0 (no expiration)"},
	}
	for _, tt := range tests {
		got := parseScalar(tt.input)
		if got != tt.want {
			t.Errorf("parseScalar(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestParseLeaderStoreID(t *testing.T) {
	output := `Region ID:  1
  StartKey:  (empty)
  EndKey:    (empty)
  Epoch:     conf_ver:1 version:1
  Peers:     [store:1, store:2, store:3]
  Leader:    store:2`

	id, ok := parseLeaderStoreID(output)
	if !ok {
		t.Fatal("parseLeaderStoreID returned ok=false")
	}
	if id != 2 {
		t.Errorf("parseLeaderStoreID = %d, want 2", id)
	}
}

func TestParseLeaderStoreIDNone(t *testing.T) {
	output := `Region ID:  1
  Leader:    (none)`

	_, ok := parseLeaderStoreID(output)
	if ok {
		t.Error("parseLeaderStoreID should return ok=false for (none)")
	}
}
