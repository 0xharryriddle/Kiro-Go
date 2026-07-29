package proxy

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kiro-go/config"
)

// The four customer-facing handlers each buffered the whole request body with a
// bare io.ReadAll(r.Body) and no ceiling:
//
//	handler.go:1519  handleCountTokens             (/v1/messages/count_tokens)
//	handler.go:1561  handleClaudeMessagesInternal  (/v1/messages)
//	handler.go:2771  handleOpenAIChat              (/v1/chat/completions)
//	responses_handler.go:22 handleOpenAIResponses   (/v1/responses)
//
// Every ADMIN surface in this repo had already decided this question — 7 sites in
// handler.go plus admin_bot_api.go, kiro_apikey_admin.go and
// kiro_profiles_admin.go all wrap http.MaxBytesReader. The customer hot path was
// the one place that did not, and it is the path reachable WITHOUT a credential:
// authenticate() returns (nil, nil) when requireApiKey is off, which is the live
// posture of this deployment.
//
// These tests configure a deliberately small ceiling instead of exceeding the
// 32 MiB default, so the suite proves the bound without allocating 32 MiB four
// times over. The default itself is pinned in config's own test.
func bodyLimitHandler(t *testing.T, limitBytes int) *Handler {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	// Written BEFORE Init so the loader picks the value up: there is no
	// UpdateMaxRequestBodyBytes, by design — this is an operator knob, not
	// something the admin API mutates at runtime.
	seed := `{"port":8080,"maxRequestBodyBytes":` + itoa(limitBytes) + `}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	if err := config.Init(path); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if got := config.GetMaxRequestBodyBytes(); got != limitBytes {
		t.Fatalf("seeded limit did not load: GetMaxRequestBodyBytes() = %d, want %d", got, limitBytes)
	}
	return &Handler{}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// oversizedJSONBody builds a syntactically VALID JSON request that exceeds the
// ceiling. Valid on purpose: a malformed body would earn a 400 from the JSON
// decoder and prove nothing about the size bound.
func oversizedJSONBody(padBytes int) string {
	return `{"model":"claude-sonnet-4.5","max_tokens":16,"messages":[{"role":"user","content":"` +
		strings.Repeat("A", padBytes) + `"}]}`
}

// customerBodySurfaces enumerates the four handlers under test. Kept as one
// table so a newly added customer surface that forgets the ceiling shows up here
// as an obvious omission rather than silently going unbounded.
func customerBodySurfaces(h *Handler) []struct {
	name    string
	path    string
	handler func(http.ResponseWriter, *http.Request)
} {
	return []struct {
		name    string
		path    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"claude_messages", "/v1/messages", h.handleClaudeMessages},
		{"claude_count_tokens", "/v1/messages/count_tokens", h.handleCountTokens},
		{"openai_chat", "/v1/chat/completions", h.handleOpenAIChat},
		{"openai_responses", "/v1/responses", h.handleOpenAIResponses},
	}
}

// The behavioural assertion: an over-ceiling body is refused with 413, on every
// customer surface. Pre-fix each of these buffered the whole body and answered
// on the CONTENT (400 for a shape/JSON complaint, or a pool error), never 413.
func TestCustomerSurfacesRefuseOversizedBody(t *testing.T) {
	const limit = 64 << 10
	h := bodyLimitHandler(t, limit)

	// 4x the ceiling: unambiguously over, and large enough that a handler
	// which only checked Content-Length would still be caught by the reader.
	body := oversizedJSONBody(limit * 4)

	for _, c := range customerBodySurfaces(h) {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(body))
			rec := httptest.NewRecorder()

			// The handler must reject at the READ step, before dispatch. If it
			// instead buffers the whole body and proceeds, this bare Handler
			// (nil pool) panics somewhere downstream — which is exactly the
			// defect, so it is recorded as a failure rather than allowed to
			// abort the test binary and hide the sibling subtests. Without this
			// guard the pre-fix run dies on a SIGSEGV and the remaining cases
			// never execute, so the neutralization result would be unreadable.
			func() {
				defer func() {
					if p := recover(); p != nil {
						t.Errorf("%s buffered a %d-byte body against a %d-byte ceiling and proceeded to dispatch (panicked: %v); want 413 at the read step",
							c.path, len(body), limit, p)
					}
				}()
				c.handler(rec, req)
			}()

			if t.Failed() {
				return
			}
			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Errorf("%s answered %d for a %d-byte body against a %d-byte ceiling; want 413. body=%s",
					c.path, rec.Code, len(body), limit, truncateForLog(rec.Body.String(), 200))
			}
		})
	}
}

// Control: the ceiling must not fire on a normal request. A small body has to
// get PAST the read step and be answered on its content — otherwise the "fix" is
// an outage dressed up as a safety feature.
//
// Deliberately invalid JSON so the assertion needs no account pool: reaching the
// decoder's complaint proves the body was read in full, which is the only thing
// under test here.
func TestCustomerSurfacesAcceptNormalBody(t *testing.T) {
	h := bodyLimitHandler(t, 64<<10)

	for _, c := range customerBodySurfaces(h) {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(`{not-json`))
			rec := httptest.NewRecorder()
			c.handler(rec, req)

			if rec.Code == http.StatusRequestEntityTooLarge {
				t.Errorf("%s rejected a 9-byte body as too large; the ceiling is firing on normal traffic", c.path)
			}
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s answered %d for malformed JSON; want 400 (proves the body was read, then parsed)",
					c.path, rec.Code)
			}
		})
	}
}

// errReader fails mid-read. It separates "body too large" from "transport broke",
// which readLimitedRequestBody must not conflate: the first is the client's
// fault and deserves 413, the second is a read error and stays 400. Without the
// errors.As check in the helper, any read failure would be mislabelled 413.
type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func TestGenuineReadErrorIsNotReportedAsTooLarge(t *testing.T) {
	h := bodyLimitHandler(t, 64<<10)

	for _, c := range customerBodySurfaces(h) {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, c.path, io.NopCloser(errReader{err: errors.New("connection reset")}))
			rec := httptest.NewRecorder()
			c.handler(rec, req)

			if rec.Code == http.StatusRequestEntityTooLarge {
				t.Errorf("%s reported a transport read error as 413; only an over-ceiling body may be 413", c.path)
			}
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s answered %d for a failed read; want 400", c.path, rec.Code)
			}
		})
	}
}

// Unit-level check on the helper itself, independent of any handler: the
// sentinel must be distinguishable, and an unrelated error must not match it.
func TestIsRequestBodyTooLargeDiscriminates(t *testing.T) {
	if !isRequestBodyTooLarge(errRequestBodyTooLarge) {
		t.Error("isRequestBodyTooLarge did not recognise its own sentinel")
	}
	if isRequestBodyTooLarge(errors.New("connection reset")) {
		t.Error("isRequestBodyTooLarge matched an unrelated error")
	}
	if isRequestBodyTooLarge(nil) {
		t.Error("isRequestBodyTooLarge matched nil")
	}
}

// A nil body must not panic. Several admin/test call paths construct requests
// without one, and the helper is now on the hot path for four handlers.
func TestReadLimitedRequestBodyToleratesNilBody(t *testing.T) {
	bodyLimitHandler(t, 64<<10)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Body = nil
	rec := httptest.NewRecorder()

	body, err := readLimitedRequestBody(rec, req)
	if err != nil {
		t.Fatalf("nil body returned an error: %v", err)
	}
	if len(body) != 0 {
		t.Fatalf("nil body returned %d bytes", len(body))
	}
}
