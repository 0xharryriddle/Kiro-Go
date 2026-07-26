package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mcpResponseFromServer drives the REAL decode path (decodeMcpResponse, which
// callMcpAPI delegates to) against a live HTTP server, so the assertions below
// are about production behaviour rather than a reimplementation of it.
func mcpResponseFromServer(t *testing.T, status int, body string) (*McpResponse, error) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	resp, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatalf("GET test server: %v", err)
	}
	defer resp.Body.Close()
	return decodeMcpResponse(resp)
}

// The MCP response body is upstream/attacker influenced: its contents are search
// results harvested from arbitrary websites, and the proxy amplifies that body
// twice (into web_search_result blocks AND into the model-facing summary string).
// Buffering it with an unbounded io.ReadAll therefore lets one oversized upstream
// reply cost several multiples of its size in resident memory, which on a small
// container is an OOM from a single request.
//
// Sibling upstream reads in this package already bound themselves with
// io.LimitReader (proxy/bedrock.go:222, proxy/custom_api_forward.go:155); this
// asserts the web_search path does too.
func TestMcpResponseIsSizeBounded(t *testing.T) {
	// Just over the limit, and deliberately valid JSON: if the bound were absent
	// this would parse successfully and the test would see no error at all, so a
	// failure here cannot be confused with a JSON-parse rejection.
	oversized := `{"jsonrpc":"2.0","id":"x","padding":"` +
		strings.Repeat("A", maxMcpResponseBytes) + `"}`

	_, err := mcpResponseFromServer(t, http.StatusOK, oversized)
	if err == nil {
		t.Fatal("an over-limit MCP response was accepted; the body read is unbounded")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("over-limit body was rejected for the wrong reason (want a size error, so the bound is what refused it): %v", err)
	}
}

// The bound must not reject legitimate traffic: a normal search payload is
// kilobytes, so a well-formed response well under the limit must still decode.
// Without this, "fixing" the DoS by lowering the cap until everything fails
// would look like success.
func TestMcpResponseUnderLimitStillDecodes(t *testing.T) {
	resp, err := mcpResponseFromServer(t, http.StatusOK,
		`{"jsonrpc":"2.0","id":"req-1","result":{"content":[{"type":"text","text":"{\"results\":[{\"title\":\"ok\",\"url\":\"https://example.test\"}]}"}]}}`)
	if err != nil {
		t.Fatalf("a normal-sized MCP response was rejected: %v", err)
	}
	results := parseSearchResults(resp)
	if results == nil || len(results.Results) != 1 || results.Results[0].Title != "ok" {
		t.Fatalf("normal payload did not survive decode: %#v", results)
	}
}
