package proxy

import (
	"errors"
	"net/http"
	"testing"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

// An upstream length rejection must be recognised. Before this classifier
// existed the condition was named only in comments, so such a failure fell into
// the default branch: it recorded an error against a perfectly healthy account
// and surfaced to the client as a 500.
func TestInputTooLongIsClassified(t *testing.T) {
	for _, msg := range []string{
		`HTTP 400 from kiro: {"reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD"}`,
		`HTTP 400 from kiro: {"message":"Input is too long."}`,
		"upstream error (status 400): prompt is too long",
		"HTTP 400: too many tokens in request",
		"context length exceeded",
		"context_length_exceeded",
		"input exceeds the maximum supported size",
		// Case must not matter: upstream bodies vary.
		`HTTP 400 from kiro: {"reason":"content_length_exceeds_threshold"}`,
		"HTTP 400: INPUT IS TOO LONG.",
	} {
		if !isInputTooLongErrorMessage(msg) {
			t.Errorf("length rejection not classified: %q", msg)
		}
	}
}

// The classifier must not swallow unrelated failures. It runs FIRST in
// handleAccountFailure's switch, so a false positive here would skip the
// account's real remediation (ban, overage refresh, quota cooldown) entirely.
//
// The quota and overage strings below are the exact shapes observed in this
// deployment's own trace log, not invented examples.
func TestInputTooLongDoesNotMatchUnrelatedErrors(t *testing.T) {
	for _, msg := range []string{
		`HTTP 402 from Kiro IDE: {"message":"You have reached the limit.","reason":"MONTHLY_REQUEST_COUNT"}`,
		`HTTP 429 from kiro: quota exhausted`,
		`HTTP 402 from Kiro IDE: OVERAGE limit exceeded`,
		"HTTP 401: unauthorized",
		"HTTP 403: User is not authorized to make this call",
		"Your User ID temporarily is suspended",
		"no available Kiro profile",
		"HTTP 500 from kiro: internal error",
		"refresh failed: 400 invalid_grant",
		"dial tcp: connection refused",
	} {
		if isInputTooLongErrorMessage(msg) {
			t.Errorf("false positive on unrelated error: %q", msg)
		}
	}
}

// A length rejection is a property of the request, so the client must see 400.
// A 500 told the client the service had failed and that retrying the identical
// oversized payload was reasonable.
func TestInputTooLongMapsToBadRequest(t *testing.T) {
	err := errors.New(`HTTP 400 from kiro: {"reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD"}`)
	if got := statusForUpstreamError(err); got != http.StatusBadRequest {
		t.Fatalf("statusForUpstreamError = %d, want %d", got, http.StatusBadRequest)
	}
}

// The load-bearing behaviour: a length rejection must NOT count against the
// account. Every account in the pool would reject the same oversized payload
// identically, so recording an error walks healthy accounts into cooldown for a
// fault that lives entirely in the request we built.
func TestInputTooLongDoesNotPenaliseAccount(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "acct", Enabled: true, Email: "a@b.c"}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	acc, ok := config.GetAccountByID("acct")
	if !ok {
		t.Fatal("seeded account missing")
	}

	p := accountpool.GetPool()
	p.Reload()
	t.Cleanup(p.WaitForPendingWrites)
	h := &Handler{pool: p}

	errCountFor := func(id string) int {
		for _, d := range p.Diagnostics() {
			if d.ID == id {
				return d.ErrorCount
			}
		}
		return -1
	}
	before := errCountFor("acct")

	h.handleAccountFailure(&acc, errors.New(`HTTP 400 from kiro: {"reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD"}`))

	if after := errCountFor("acct"); after != before {
		t.Fatalf("length rejection penalised the account: errorCount %d -> %d", before, after)
	}
	// It must also never disable the account.
	if got, _ := config.GetAccountByID("acct"); !got.Enabled || got.BanStatus == "BANNED" {
		t.Fatalf("length rejection disabled a healthy account: enabled=%v banStatus=%q",
			got.Enabled, got.BanStatus)
	}
}

// Contrast case: a genuine quota error MUST still be recorded, proving the new
// branch did not simply swallow every failure ahead of the others.
func TestQuotaErrorStillPenalisesAccount(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "quota-acct", Enabled: true, Email: "q@b.c"}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	acc, ok := config.GetAccountByID("quota-acct")
	if !ok {
		t.Fatal("seeded account missing")
	}

	p := accountpool.GetPool()
	p.Reload()
	t.Cleanup(p.WaitForPendingWrites)
	h := &Handler{pool: p}

	errCountFor := func(id string) int {
		for _, d := range p.Diagnostics() {
			if d.ID == id {
				return d.ErrorCount
			}
		}
		return -1
	}
	before := errCountFor("quota-acct")

	h.handleAccountFailure(&acc, errors.New("HTTP 429 from kiro: quota exhausted"))

	if after := errCountFor("quota-acct"); after <= before {
		t.Fatalf("quota error was not recorded: errorCount %d -> %d", before, after)
	}
}

// Status mapping for the other classes must be unchanged by the new case being
// inserted ahead of them.
func TestStatusMappingUnchangedForOtherClasses(t *testing.T) {
	cases := []struct {
		msg  string
		want int
	}{
		{`HTTP 429 from kiro: quota exhausted`, http.StatusTooManyRequests},
		{`HTTP 402 from Kiro IDE: OVERAGE limit exceeded`, http.StatusPaymentRequired},
		{"upstream error (status 401): unauthorized", http.StatusUnauthorized},
		{"HTTP 500 from kiro: internal error", http.StatusInternalServerError},
	}
	for _, c := range cases {
		if got := statusForUpstreamError(errors.New(c.msg)); got != c.want {
			t.Errorf("statusForUpstreamError(%q) = %d, want %d", c.msg, got, c.want)
		}
	}
}
