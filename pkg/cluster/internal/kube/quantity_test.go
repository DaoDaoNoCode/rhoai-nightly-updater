package kube

import (
	"encoding/json"
	"testing"
)

// Unmarshaling supports Quantity values encoded as JSON strings or numbers,
// reflecting how CR authors write them. SameQuantity treats matching amounts as
// equal no matter which encoding was used.
func TestQuantity(t *testing.T) {
	var got struct {
		CPU    Quantity `json:"cpu"`
		Memory Quantity `json:"memory"`
		None   Quantity `json:"none"`
	}
	if err := json.Unmarshal([]byte(`{"cpu":1,"memory":"5Gi","none":null}`), &got); err != nil || got.CPU != "1" || got.Memory != "5Gi" || got.None != "" {
		t.Fatalf("decoded %+v, %v", got, err)
	}
	if err := json.Unmarshal([]byte(`{"cpu":1.5}`), &got); err != nil || got.CPU != "1.5" {
		t.Fatalf("decoded %+v, %v", got, err)
	}
	for _, tc := range []struct {
		a, b        string
		same, known bool
	}{
		{"1000m", "1", true, true},
		{"2000m", "2", true, true},
		{"500m", "0.5", true, true},
		{"19456Mi", "19Gi", true, true},
		{"5368709120", "5Gi", true, true},
		{"1e3", "1k", true, true},
		{"5G", "5Gi", false, true},
		{"11Gi", "19Gi", false, true},
		{"6", "6", true, true},
		{"abc", "1", false, false},
		{"", "1", false, false},
		{"-1", "1", false, false},
	} {
		same, known := SameQuantity(tc.a, tc.b)
		if same != tc.same || known != tc.known {
			t.Errorf("SameQuantity(%q, %q) = %v, %v; want %v, %v", tc.a, tc.b, same, known, tc.same, tc.known)
		}
	}
}
