package proxy

import (
	"testing"
)

func TestKiroCallDiagnosticsRecordsStatusAndUpstreamRequestID(t *testing.T) {
	var d KiroCallDiagnostics
	d.record(TraceAttempt{
		UpstreamEndpoint:  "primary",
		UpstreamHost:      "codewhisperer.eu-central-1.amazonaws.com",
		HTTPStatus:        500,
		UpstreamRequestID: "abc-123",
	})

	last := d.Last()
	if last == nil {
		t.Fatal("expected a recorded endpoint attempt")
	}
	if last.HTTPStatus != 500 {
		t.Fatalf("HTTPStatus = %d, want 500", last.HTTPStatus)
	}
	if last.UpstreamRequestID != "abc-123" {
		t.Fatalf("UpstreamRequestID = %q, want abc-123", last.UpstreamRequestID)
	}
	if last.UpstreamHost == "" {
		t.Fatal("expected upstream host to be captured")
	}
}

func TestKiroCallDiagnosticsLastReturnsMostRecent(t *testing.T) {
	var d KiroCallDiagnostics
	d.record(TraceAttempt{UpstreamEndpoint: "first", HTTPStatus: 500})
	d.record(TraceAttempt{UpstreamEndpoint: "second", HTTPStatus: 200})

	if got := d.Last(); got == nil || got.UpstreamEndpoint != "second" {
		t.Fatalf("Last() = %#v, want the second attempt", got)
	}
	if len(d.Endpoints) != 2 {
		t.Fatalf("len(Endpoints) = %d, want 2", len(d.Endpoints))
	}
}

func TestKiroCallDiagnosticsNilSafe(t *testing.T) {
	var d *KiroCallDiagnostics
	// A nil diagnostics sink must be a no-op so existing call sites can pass nil.
	d.record(TraceAttempt{HTTPStatus: 500})
	if d.Last() != nil {
		t.Fatal("nil diagnostics must report no attempts")
	}
}

func TestUpstreamRequestIDFromHeaderIsCaseInsensitive(t *testing.T) {
	// AWS services are inconsistent about the casing of this header, and Go's
	// Header.Get only canonicalises the exact key it is given. Probe the known
	// variants rather than betting on one spelling.
	cases := []struct {
		key  string
		want string
	}{
		{"x-amzn-RequestId", "id-1"},
		{"x-amzn-requestid", "id-2"},
		{"X-Amzn-Trace-Id", "id-3"},
		{"x-request-id", "id-4"},
	}
	for _, tc := range cases {
		h := map[string][]string{}
		h[canonicalHeaderKey(tc.key)] = []string{tc.want}
		if got := upstreamRequestIDFromHeader(h); got != tc.want {
			t.Fatalf("header %s: got %q, want %q", tc.key, got, tc.want)
		}
	}
}
