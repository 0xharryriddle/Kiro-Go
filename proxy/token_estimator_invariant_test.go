package proxy

// The token estimator has two weight sets with a load-bearing relationship:
// reportingTokenWeights (optimistic, used for the usage numbers a customer sees)
// and wireTokenWeights (pessimistic, used to BOUND what is put on the wire).
//
// The invariant that makes the design safe is:
//
//\twireTokens(s) >= reportTokens(s)   for every input s
//
// If it ever inverts, the proxy under-counts what it is about to send, an
// oversized request reaches upstream, and the WHOLE call is rejected — the
// failure mode the pessimistic weights exist to prevent. Nothing else in the
// suite pinned it, and the file had no tests at all.
//
// Fuzzed rather than spot-checked on purpose: this is a numeric property over
// four character classes plus a `length < 5` shortcut that ignores the weights
// entirely and divides by a hardcoded 3.0. Hand-picked cases cannot establish
// it; a randomized sweep concentrated on that boundary can falsify it.
//
// Observed: 250k cases, zero violations, tightest margin exactly 0 (equality is
// fine — the bound is >=, not >).

import (
	"math/rand"
	"testing"
)

// Fuzz the ONE invariant the design depends on: wire >= report, always.
// Hand-picked cases are weak evidence for a numeric property; a randomized
// sweep over mixed character classes is what would actually find a hole,
// especially around the length<5 shortcut which ignores the weights entirely.
func TestWireEstimateNeverUnderReportsShortStrings(t *testing.T) {
	alphabets := []([]rune){
		[]rune("abcdefghij klmnop"),
		[]rune("0123456789"),
		[]rune("!@#$%^&*(){}[]:;<>?/"),
		[]rune("\u4f60\u597d\u4e16\u754c\u3053\u3093\u306b\u3061\u306f\ud55c\uad00"),
		[]rune("a1!\u4f60 b2@\u597d"),
	}
	rng := rand.New(rand.NewSource(20260727))
	worstDelta := 1 << 30
	worstCase := ""
	violations := 0
	for i := 0; i < 200000; i++ {
		ab := alphabets[rng.Intn(len(alphabets))]
		n := rng.Intn(12) // concentrate on the short-string boundary
		b := make([]rune, n)
		for j := range b {
			b[j] = ab[rng.Intn(len(ab))]
		}
		s := string(b)
		rep := estimateApproxTokens(s)
		wire := estimateWireTokens(s)
		if wire < rep {
			violations++
			if violations <= 5 {
				t.Errorf("VIOLATION runes=%d %q: wire=%d < report=%d", n, s, wire, rep)
			}
		}
		if d := wire - rep; d < worstDelta {
			worstDelta = d
			worstCase = s
		}
	}
	t.Logf("violations=%d  tightest margin (wire-report)=%d on %q", violations, worstDelta, worstCase)
}

// And the same over LONG strings, where the weighted path runs.
func TestWireEstimateNeverUnderReportsLongStrings(t *testing.T) {
	ab := []rune("abc XYZ 019 !@# \u4f60\u597d\u3053\u3093")
	rng := rand.New(rand.NewSource(7))
	violations := 0
	tightest := 1 << 30
	for i := 0; i < 50000; i++ {
		n := 5 + rng.Intn(400)
		b := make([]rune, n)
		for j := range b {
			b[j] = ab[rng.Intn(len(ab))]
		}
		s := string(b)
		rep := estimateApproxTokens(s)
		wire := estimateWireTokens(s)
		if wire < rep {
			violations++
			if violations <= 3 {
				t.Errorf("LONG VIOLATION runes=%d: wire=%d < report=%d", n, wire, rep)
			}
		}
		if d := wire - rep; d < tightest {
			tightest = d
		}
	}
	t.Logf("long violations=%d tightest margin=%d", violations, tightest)
}
