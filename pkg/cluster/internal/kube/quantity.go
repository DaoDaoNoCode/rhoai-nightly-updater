package kube

import (
	"bytes"
	"encoding/json"
	"math/big"
	"strings"
)

// Quantity mirrors how a Kubernetes resource amount appears in serialized JSON.
// For built-in types the API tends to emit string forms, whereas custom
// resources (RayCluster, ClusterQueue, and the like) may leave numeric fields
// as JSON numbers ("cpu": 1). This type unmarshals either representation;
// numbers become their decimal string form.
type Quantity string

// UnmarshalJSON handles JSON strings, numeric tokens, and null (mapped to "").
func (q *Quantity) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case bytes.Equal(b, []byte("null")):
		*q = ""
		return nil
	case len(b) > 0 && b[0] == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*q = Quantity(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*q = Quantity(n.String())
	return nil
}

// quantitySuffixes lists Kubernetes unit suffixes alongside scaling factors:
// binary prefixes (Ki through Ei) and decimal ones (m, k, M through E).
// Binary entries are matched before decimal so "Mi" is not parsed as "M";
// see k8s.io/apimachinery pkg/api/resource suffix.go.
var quantitySuffixes = []struct {
	suffix string
	factor *big.Rat
}{
	{"Ki", ratPow(2, 10)}, {"Mi", ratPow(2, 20)}, {"Gi", ratPow(2, 30)}, {"Ti", ratPow(2, 40)}, {"Pi", ratPow(2, 50)}, {"Ei", ratPow(2, 60)},
	{"m", new(big.Rat).SetFrac64(1, 1000)}, {"k", ratPow(10, 3)}, {"M", ratPow(10, 6)}, {"G", ratPow(10, 9)}, {"T", ratPow(10, 12)}, {"P", ratPow(10, 15)}, {"E", ratPow(10, 18)},
}

func ratPow(base, exp int64) *big.Rat {
	return new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(base), big.NewInt(exp), nil))
}

// parseQuantity interprets literals such as "1000m", "1", "19Gi", "5368709120",
// or "1e3" into an exact rational count of base units (cores or bytes).
func parseQuantity(s string) (*big.Rat, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}
	factor := big.NewRat(1, 1)
	for _, suf := range quantitySuffixes {
		if before, ok := strings.CutSuffix(s, suf.suffix); ok && before != "" {
			s, factor = before, suf.factor
			break
		}
	}
	v, ok := new(big.Rat).SetString(s)
	if !ok || v.Sign() < 0 {
		return nil, false
	}
	return v.Mul(v, factor), true
}

// SameQuantity reports whether a and b denote equal amounts—for example
// "1000m" versus "1", "19456Mi" versus "19Gi", or "5368709120" versus "5Gi".
// If either operand fails to parse, known is false and same must be ignored.
func SameQuantity(a, b string) (same, known bool) {
	x, okA := parseQuantity(a)
	y, okB := parseQuantity(b)
	if !okA || !okB {
		return false, false
	}
	return x.Cmp(y) == 0, true
}
