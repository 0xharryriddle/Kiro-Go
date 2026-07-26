package proxy

import (
	"testing"
)

// callMcpAPI generates a fresh JSON-RPC request id per search (createMcpRequest)
// but decodeMcpResponse never compared the response's id back against it, and
// never checked the jsonrpc version. A JSON-RPC client that ignores both fields
// cannot tell the answer to THIS call from an unrelated or stale one: any
// response carrying a parseable results payload was accepted as the result of
// the current search.
//
// These tests pin the PREDICATE only. callMcpAPI deliberately logs a mismatch
// and continues rather than rejecting the response: nothing in this repo records
// a real MCP response, so "Kiro echoes our id back" is an unverified assumption,
// and failing closed on it would break every web search (burning one account per
// attempt) if the server picks its own id. The detection is still worth having —
// a real mismatch becomes discoverable from operator logs, and the check can be
// tightened to a hard rejection once a capture proves the id is mirrored.
// See validateMcpEnvelope's call site for the full reasoning.
func TestMcpResponseRejectsMismatchedRequestID(t *testing.T) {
	req := &McpRequest{ID: "web_search_tooluse_expected", JSONRPC: "2.0", Method: "tools/call"}
	resp := &McpResponse{
		ID:      "web_search_tooluse_SOMETHING_ELSE",
		JSONRPC: "2.0",
		Result:  &McpResult{Content: []McpContent{{Type: "text", Text: `{"results":[{"title":"other","url":"https://other.test"}]}`}}},
	}

	if err := validateMcpEnvelope(req, resp); err == nil {
		t.Fatal("a response whose id does not match the request was accepted")
	}
}

func TestMcpResponseRejectsWrongJSONRPCVersion(t *testing.T) {
	req := &McpRequest{ID: "id-1", JSONRPC: "2.0", Method: "tools/call"}
	resp := &McpResponse{
		ID:      "id-1",
		JSONRPC: "1.0",
		Result:  &McpResult{Content: []McpContent{{Type: "text", Text: `{"results":[]}`}}},
	}

	if err := validateMcpEnvelope(req, resp); err == nil {
		t.Fatal("a response with a non-2.0 jsonrpc version was accepted")
	}
}

// The matching envelope must pass, or every search breaks.
func TestMcpResponseAcceptsMatchingEnvelope(t *testing.T) {
	req := &McpRequest{ID: "id-1", JSONRPC: "2.0", Method: "tools/call"}
	resp := &McpResponse{
		ID:      "id-1",
		JSONRPC: "2.0",
		Result:  &McpResult{Content: []McpContent{{Type: "text", Text: `{"results":[]}`}}},
	}

	if err := validateMcpEnvelope(req, resp); err != nil {
		t.Fatalf("a matching envelope was rejected: %v", err)
	}
}

// Upstream is not required to echo the fields. A blank id or blank jsonrpc is
// "not stated", not "mismatched" — rejecting those would break every search
// against a server that simply omits them, which is a far worse outcome than the
// replay case this check exists to catch. Only a value that is present AND
// different is evidence of a mismatch.
func TestMcpResponseToleratesOmittedEnvelopeFields(t *testing.T) {
	req := &McpRequest{ID: "id-1", JSONRPC: "2.0", Method: "tools/call"}
	resp := &McpResponse{
		Result: &McpResult{Content: []McpContent{{Type: "text", Text: `{"results":[]}`}}},
	}

	if err := validateMcpEnvelope(req, resp); err != nil {
		t.Fatalf("an envelope that omits id/jsonrpc was rejected: %v", err)
	}
}
