package config

import "testing"

// Body capture is the one feature in the tracing work that can persist user
// prompts, so the default must be the privacy-preserving one and "full" must
// require a separate, explicit acknowledgement.
func TestGetTraceCaptureModeDefaultsToMeta(t *testing.T) {
	cfgLock.Lock()
	cfg = &Config{}
	cfgLock.Unlock()

	if got := GetTraceCaptureMode(); got != TraceCaptureMeta {
		t.Fatalf("default capture mode = %q, want %q (never default to storing prompts)", got, TraceCaptureMeta)
	}
}

func TestGetTraceCaptureModeFullDegradesWithoutAcknowledgement(t *testing.T) {
	cfgLock.Lock()
	cfg = &Config{TraceCaptureMode: "full"}
	cfgLock.Unlock()

	// "full" without the risk acknowledgement must NOT retain verbatim prompts.
	if got := GetTraceCaptureMode(); got != TraceCaptureRedacted {
		t.Fatalf("capture mode = %q, want %q: full requires an explicit acknowledgement", got, TraceCaptureRedacted)
	}

	cfgLock.Lock()
	cfg = &Config{TraceCaptureMode: "full", TraceCaptureAcknowledgeRisk: true}
	cfgLock.Unlock()

	if got := GetTraceCaptureMode(); got != TraceCaptureFull {
		t.Fatalf("capture mode = %q, want %q once acknowledged", got, TraceCaptureFull)
	}
}

func TestGetTraceCaptureModeRejectsUnknownValues(t *testing.T) {
	for _, raw := range []string{"verbose", "yes", "1", "  ", "prompts"} {
		cfgLock.Lock()
		cfg = &Config{TraceCaptureMode: raw}
		cfgLock.Unlock()
		// An unrecognised value must fail SAFE, not fail open.
		if got := GetTraceCaptureMode(); got != TraceCaptureMeta {
			t.Fatalf("capture mode for %q = %q, want %q (unknown values must fail safe)", raw, got, TraceCaptureMeta)
		}
	}
}

func TestGetTraceCaptureModeAcceptsOffAndRedacted(t *testing.T) {
	cases := map[string]string{
		"off":      TraceCaptureOff,
		"OFF":      TraceCaptureOff,
		" meta ":   TraceCaptureMeta,
		"redacted": TraceCaptureRedacted,
	}
	for raw, want := range cases {
		cfgLock.Lock()
		cfg = &Config{TraceCaptureMode: raw}
		cfgLock.Unlock()
		if got := GetTraceCaptureMode(); got != want {
			t.Fatalf("capture mode for %q = %q, want %q", raw, got, want)
		}
	}
}

func TestTraceRetentionAndBodyCapsHaveSaneDefaults(t *testing.T) {
	cfgLock.Lock()
	cfg = &Config{}
	cfgLock.Unlock()

	if got := GetTraceRetentionHours(); got != 168 {
		t.Fatalf("GetTraceRetentionHours() = %d, want 168 (7 days)", got)
	}
	if got := GetTraceMaxBodyBytes(); got != 256*1024 {
		t.Fatalf("GetTraceMaxBodyBytes() = %d, want 262144", got)
	}

	// Explicit values win; nonsense values fall back to the default rather than
	// producing unbounded retention or zero-length bodies.
	cfgLock.Lock()
	cfg = &Config{TraceRetentionHours: 24, TraceMaxBodyBytes: 4096}
	cfgLock.Unlock()
	if got := GetTraceRetentionHours(); got != 24 {
		t.Fatalf("GetTraceRetentionHours() = %d, want 24", got)
	}
	if got := GetTraceMaxBodyBytes(); got != 4096 {
		t.Fatalf("GetTraceMaxBodyBytes() = %d, want 4096", got)
	}

	cfgLock.Lock()
	cfg = &Config{TraceRetentionHours: -5, TraceMaxBodyBytes: -1}
	cfgLock.Unlock()
	if got := GetTraceRetentionHours(); got != 168 {
		t.Fatalf("negative retention = %d, want the 168 default", got)
	}
	if got := GetTraceMaxBodyBytes(); got != 256*1024 {
		t.Fatalf("negative body cap = %d, want the 262144 default", got)
	}
}

// Capture must be resolvable without a loaded config (nil cfg) — a nil
// dereference here would take down the request path.
func TestTraceCaptureGettersAreNilSafe(t *testing.T) {
	cfgLock.Lock()
	cfg = nil
	cfgLock.Unlock()

	if got := GetTraceCaptureMode(); got != TraceCaptureMeta {
		t.Fatalf("nil cfg capture mode = %q, want %q", got, TraceCaptureMeta)
	}
	if got := GetTraceRetentionHours(); got != 168 {
		t.Fatalf("nil cfg retention = %d, want 168", got)
	}
	if got := GetTraceMaxBodyBytes(); got != 256*1024 {
		t.Fatalf("nil cfg body cap = %d, want 262144", got)
	}
}
