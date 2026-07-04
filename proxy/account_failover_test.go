package proxy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAccountFailureClassifiers(t *testing.T) {
	tests := []struct {
		name string
		fn   func(string) bool
		msg  string
	}{
		{name: "quota", fn: isQuotaErrorMessage, msg: "HTTP 429: quota exhausted"},
		{name: "overage", fn: isOverageErrorMessage, msg: "HTTP 402 from Kiro IDE: OVERAGE limit exceeded"},
		{name: "suspension", fn: isSuspensionErrorMessage, msg: "Your User ID temporarily is suspended"},
		{name: "profile", fn: isProfileUnavailableErrorMessage, msg: "no available Kiro profile"},
		{name: "auth", fn: isAuthErrorMessage, msg: "Authentication failed - token invalid or expired"},
	}

	for _, tc := range tests {
		if !tc.fn(tc.msg) {
			t.Fatalf("%s classifier did not match %q", tc.name, tc.msg)
		}
	}
}

func TestStatusForUpstreamErrorMapsQuotaTo429(t *testing.T) {
	status := statusForUpstreamError(errors.New("HTTP 429 from Kiro IDE: quota exhausted"))
	if status != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", status)
	}
}

func TestApplyRetryAfterHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	applyRetryAfterHeader(rec, errors.New("HTTP 429 from Kiro IDE: quota exhausted; retry after 120"))
	if got := rec.Header().Get("Retry-After"); got != "120" {
		t.Fatalf("expected Retry-After 120, got %q", got)
	}
}
