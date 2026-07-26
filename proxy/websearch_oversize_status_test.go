package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// The MCP size bound must not swallow the upstream HTTP status.
//
// decodeMcpResponse checked the byte bound BEFORE the status, so an oversized
// error response was reported only as "MCP response exceeds N bytes". That
// string carries no status, and every downstream classifier keys off the status:
//
//   - isQuotaErrorMessage (proxy/account_failover.go) recognises 429 / quota
//     markers, so an oversized 429 was filed as a generic transient failure
//     instead of a quota exhaustion — the account is not cooled for the quota
//     window and the client is told 502 instead of 429, so it retries
//     immediately into the same exhausted account.
//   - the same loss applies to an oversized 401/403, which then never reaches
//     the auth classification that would flag the credential.
//
// A quota or auth response is exactly the kind that can carry a large HTML
// error page from a gateway, so this is not a hypothetical shape.
//
// The bound itself must still hold: the body must never be buffered beyond the
// limit, and an oversized 200 must still be rejected rather than parsed.
func TestOversizedMcpResponseKeepsUpstreamStatus(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   string
	}{
		{"oversized 429 must remain classifiable as quota", http.StatusTooManyRequests, "429"},
		{"oversized 401 must remain classifiable as auth", http.StatusUnauthorized, "401"},
		{"oversized 403 must remain classifiable as auth", http.StatusForbidden, "403"},
		{"oversized 500 must remain classifiable as upstream", http.StatusInternalServerError, "500"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			oversize := strings.Repeat("x", maxMcpResponseBytes+64)
			resp := &http.Response{
				StatusCode: c.status,
				Body:       io.NopCloser(strings.NewReader(oversize)),
			}
			_, err := decodeMcpResponse(resp)
			if err == nil {
				t.Fatal("expected an error for an oversized response")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("oversized response lost its HTTP status %s: %v", c.want, err)
			}
		})
	}
}

// A 429 whose oversized body is dropped must still be recognised by the quota
// classifier — that is the consequence the test above exists to protect.
func TestOversizedQuotaResponseIsStillClassifiedAsQuota(t *testing.T) {
	oversize := strings.Repeat("x", maxMcpResponseBytes+64)
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       io.NopCloser(strings.NewReader(oversize)),
	}
	_, err := decodeMcpResponse(resp)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !isQuotaErrorMessage(err.Error()) {
		t.Fatalf("oversized 429 not classified as quota, so the account is not cooled: %v", err)
	}
}

// The bound must still do its job: the oversized body must NOT be echoed back
// in the error (that would reintroduce the memory amplification the bound
// exists to prevent), and an oversized 200 must be refused rather than parsed.
func TestOversizedMcpResponseStillBoundedAndNotEchoed(t *testing.T) {
	oversize := strings.Repeat("y", maxMcpResponseBytes+64)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(oversize)),
	}
	_, err := decodeMcpResponse(resp)
	if err == nil {
		t.Fatal("expected an oversized 200 to be refused")
	}
	msg := err.Error()
	if len(msg) > 4096 {
		t.Fatalf("error echoed the oversized body (%d chars): the size bound is defeated", len(msg))
	}
	if strings.Contains(msg, strings.Repeat("y", 512)) {
		t.Fatal("error echoed a large run of the oversized body")
	}
	if !strings.Contains(msg, "exceeds") {
		t.Fatalf("oversized 200 should report the size violation: %v", msg)
	}
}

// A normal-sized error response must keep the existing behaviour: status plus
// the (bounded) body, which is what makes upstream errors diagnosable.
func TestNormalSizedErrorResponseStillCarriesStatusAndBody(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       io.NopCloser(strings.NewReader(`{"message":"quota exhausted"}`)),
	}
	_, err := decodeMcpResponse(resp)
	if err == nil {
		t.Fatal("expected an error for a 429")
	}
	msg := err.Error()
	if !strings.Contains(msg, "429") || !strings.Contains(msg, "quota exhausted") {
		t.Fatalf("normal-sized error lost status or body: %v", msg)
	}
}
