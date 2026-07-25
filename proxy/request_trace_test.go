package proxy

import (
	"strings"
	"testing"
)

func TestNewTraceIDIsUniqueAndPrefixed(t *testing.T) {
	a := newTraceID()
	b := newTraceID()
	if a == b {
		t.Fatal("trace IDs must be unique")
	}
	if !strings.HasPrefix(a, "trc_") {
		t.Fatalf("unexpected trace id shape: %q", a)
	}
	if len(a) < 8 {
		t.Fatalf("trace id too short: %q", a)
	}
}
