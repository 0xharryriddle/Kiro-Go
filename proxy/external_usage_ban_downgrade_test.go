package proxy

import (
	"kiro-go/config"
	"path/filepath"
	"testing"

	accountpool "kiro-go/pool"
)

// F3's quarantine calls SetAccountBanStatus(id, "DISABLED", ...), and that setter
// overwrites BanStatus/BanReason unconditionally. An account that upstream already
// BANNED (auth revoked, AWS suspension) is therefore rewritten to DISABLED by an
// unrelated external-usage observation.
//
// That is a downgrade of a permanent state to a recoverable one: BANNED is
// operator-only, while DISABLED is exactly what pool.reprobeDisabled revisits
// (pool/account.go). So a banned credential could re-enter rotation because a
// billing-period audit happened to run afterwards, and the original ban reason —
// the only record of WHY it was banned — is destroyed.
//
// F3 must quarantine an out-of-rotation account without ever weakening a stronger
// existing state.
func TestExternalUsageAutoDisableDoesNotDowngradeBan(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdateExternalUsageAutoDisable(true); err != nil {
		t.Fatalf("UpdateExternalUsageAutoDisable: %v", err)
	}

	const banReason = "AWS temporarily suspended - unusual user activity detected"
	if err := config.AddAccount(config.Account{
		ID: "acct", Email: "a@b.c", Enabled: false,
		BanStatus: "BANNED", BanReason: banReason,
		// Mid-period baseline: upstream grew beyond what we drove.
		ExternalPeriodKey: "p", ExternalPeriodStart: 100, ExternalPeriodOurCredit: 0,
		ExternalConfidence: config.ExternalConfidenceClean,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	h := &Handler{pool: accountpool.GetPool()}
	h.pool.Reload()

	// Upstream usage grew to 150 in the same period while we are not routing:
	// ComputeExternalUsage returns strong_external, so F3 fires.
	h.recomputeExternalUsage("acct", &config.AccountInfo{
		NextResetDate: "p", UsageCurrent: 150, UsageLimit: 1000,
	}, true)

	acc, ok := config.GetAccountByID("acct")
	if !ok {
		t.Fatal("account vanished")
	}
	if acc.BanStatus != "BANNED" {
		t.Fatalf("permanent ban downgraded to %q: an operator-only state became auto-recoverable", acc.BanStatus)
	}
	if acc.BanReason != banReason {
		t.Fatalf("original ban reason destroyed:\n  got  %q\n  want %q", acc.BanReason, banReason)
	}
	if acc.Enabled {
		t.Fatal("account must remain out of rotation")
	}
}

// The quarantine must still be applied to an account that is merely disabled
// (the normal F3 case), otherwise fixing the downgrade above would make F3 inert
// again — the exact defect this whole thread started from.
func TestExternalUsageAutoDisableStillQuarantinesDisabledAccount(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdateExternalUsageAutoDisable(true); err != nil {
		t.Fatalf("UpdateExternalUsageAutoDisable: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID: "acct", Email: "a@b.c", Enabled: false,
		BanStatus: "DISABLED", BanReason: "operator paused",
		ExternalPeriodKey: "p", ExternalPeriodStart: 100, ExternalPeriodOurCredit: 0,
		ExternalConfidence: config.ExternalConfidenceClean,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	h := &Handler{pool: accountpool.GetPool()}
	h.pool.Reload()
	h.recomputeExternalUsage("acct", &config.AccountInfo{
		NextResetDate: "p", UsageCurrent: 150, UsageLimit: 1000,
	}, true)

	acc, ok := config.GetAccountByID("acct")
	if !ok {
		t.Fatal("account vanished")
	}
	if acc.BanReason != config.ExternalUsageDisableReason {
		t.Fatalf("F3 did not quarantine a disabled account: BanReason = %q, want %q",
			acc.BanReason, config.ExternalUsageDisableReason)
	}
}
