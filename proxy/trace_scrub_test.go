package proxy

import (
	"strings"
	"testing"
)

// Credential material must be stripped in EVERY capture mode. "full" controls
// how much PROMPT text is retained -- it never means "retain credentials".
func TestScrubTraceBodyRemovesSecretsInAllModes(t *testing.T) {
	body := `{
	  "authorization": "Bearer abc.def.ghijklmnop",
	  "kiroApiKey": "ksk_live_supersecret123",
	  "refreshToken": "eyJhbGciOi.payload.signature",
	  "clientSecret": "s3cr3t-value-here",
	  "x-api-key": "raw-key-value-1234",
	  "prompt": "hello world"
	}`

	for _, mode := range []string{"meta", "redacted", "full"} {
		t.Run(mode, func(t *testing.T) {
			got := string(scrubTraceBody([]byte(body), mode))
			for _, forbidden := range []string{
				"ksk_live_supersecret123",
				"abc.def.ghijklmnop",
				"eyJhbGciOi.payload.signature",
				"s3cr3t-value-here",
				"raw-key-value-1234",
			} {
				if strings.Contains(got, forbidden) {
					t.Fatalf("mode %s leaked %q:\n%s", mode, forbidden, got)
				}
			}
		})
	}
}

// In "redacted" mode PII is additionally removed, while "full" keeps prompt text
// verbatim (minus credentials).
func TestScrubTraceBodyRedactsPIIOnlyInRedactedMode(t *testing.T) {
	body := []byte(`{"prompt":"contact me at alice@example.com from 10.1.2.3"}`)

	redacted := string(scrubTraceBody(body, "redacted"))
	if strings.Contains(redacted, "alice@example.com") {
		t.Fatalf("redacted mode must remove email addresses: %s", redacted)
	}
	if strings.Contains(redacted, "10.1.2.3") {
		t.Fatalf("redacted mode must remove IP addresses: %s", redacted)
	}

	full := string(scrubTraceBody(body, "full"))
	if !strings.Contains(full, "alice@example.com") {
		t.Fatalf("full mode should retain prompt text verbatim: %s", full)
	}
}

// "off" and "meta" must never yield body content at all.
func TestScrubTraceBodyReturnsNothingWhenCaptureDisabled(t *testing.T) {
	body := []byte(`{"prompt":"sensitive user question"}`)
	for _, mode := range []string{"off", "meta", "", "bogus"} {
		if got := scrubTraceBody(body, mode); got != nil {
			t.Fatalf("mode %q must not produce a body, got %q", mode, got)
		}
	}
}

func TestScrubTraceBodyHandlesEmptyInput(t *testing.T) {
	if got := scrubTraceBody(nil, "full"); got != nil {
		t.Fatalf("nil input must stay nil, got %q", got)
	}
	if got := scrubTraceBody([]byte(""), "full"); got != nil {
		t.Fatalf("empty input must stay nil, got %q", got)
	}
}

// Truncation must be explicit so a reader never mistakes a cut body for the
// whole thing.
func TestTruncateTraceBodyFlagsTruncation(t *testing.T) {
	body := []byte(strings.Repeat("a", 100))

	got, truncated := truncateTraceBody(body, 40)
	if !truncated {
		t.Fatal("expected truncated=true when the body exceeds the cap")
	}
	if len(got) != 40 {
		t.Fatalf("len = %d, want 40", len(got))
	}

	got, truncated = truncateTraceBody(body, 1000)
	if truncated {
		t.Fatal("expected truncated=false when the body fits")
	}
	if len(got) != 100 {
		t.Fatalf("len = %d, want 100", len(got))
	}

	// A non-positive cap means "use the default", never "store nothing".
	if got, _ = truncateTraceBody(body, 0); len(got) != 100 {
		t.Fatalf("cap 0 should fall back to the default, got len %d", len(got))
	}
}
