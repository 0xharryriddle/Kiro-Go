package proxy

import (
	"net/http"
	"testing"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

// resolveApiKeyRegion takes an explicit region and probes/returns it WITHOUT ever
// checking its shape (admin_bot_api.go:900-919):
//
//	targetRegions := kiroApiKeyCandidateRegions()
//	if explicit := strings.TrimSpace(explicitRegion); explicit != "" {
//	    targetRegions = []string{explicit}     // no validateRegionOverride
//	}
//	...
//	return region, info, false, nil            // the caller's label, verbatim
//
// apiAddAccount (POST /admin/api/accounts) feeds it a caller-supplied
// account.Region and then persists whatever comes back (handler.go:4270, :4280):
//
//	region, info, retryable, err := resolveApiKeyRegion(account.KiroApiKey, account.Region)
//	...
//	account.Region = region
//
// This is the SAME defect class already fixed on the sibling bot supply route in
// 7bb854f (see admin_bot_region_test.go) — that fix validated the region inside
// handleAdminAddKiroApiKey, so it did not cover this second entrypoint into the
// same probe helper.
//
// Consequence chain for a malformed label such as "bogus":
//
//   - the account is persisted with Region:"bogus";
//   - regionalizeURLForRegion refuses that shape and returns the URL unchanged
//     (kiro_api.go:236-238), so traffic silently stays on us-east-1 while the
//     stored bucket claims otherwise;
//   - findAccountForKiroIdentity dedups on (UserId, data-plane region), so the
//     SAME upstream Kiro account can be added again under us-east-1 — two pool
//     slots for one account, doubling its routing weight and double-counting its
//     quota in /admin/pool.
//
// The probe is stubbed to SUCCEED for any region, so the only thing that can stop
// a bogus region from being persisted is validation in the code under test. That
// is what makes this test discriminate rather than pass on a network failure.
func TestAddAccountRejectsMalformedApiKeyRegion(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("topsecret")
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}

	origProbe := probeKiroApiKey
	defer func() { probeKiroApiKey = origProbe }()
	var probedRegions []string
	probeKiroApiKey = func(key, region string) (*config.AccountInfo, error) {
		probedRegions = append(probedRegions, region)
		return &config.AccountInfo{
			Email:  "panel-supply@example.com",
			UserId: "kiro-user-panel-bogus-region",
		}, nil
	}

	rec := serve(h, adminReq(http.MethodPost, "/admin/api/accounts",
		`{"kiroApiKey":"ksk_panelBogusRegion","region":"bogus","enabled":false}`, "topsecret"))

	for _, a := range config.GetAccounts() {
		if a.Region == "bogus" {
			t.Fatalf("account %s was persisted with Region:%q (HTTP %d). "+
				"regionalizeURLForRegion rejects that shape and silently routes "+
				"traffic to us-east-1, so the stored region bucket disagrees with "+
				"reality and the same upstream account can be added again under "+
				"us-east-1 — two pool slots, doubled routing weight, double-counted "+
				"quota", a.ID, a.Region, rec.Code)
		}
	}
	if rec.Code == http.StatusOK {
		t.Fatalf("route returned 200 for region %q; a malformed region must be "+
			"rejected before the upstream probe (probed regions: %v)",
			"bogus", probedRegions)
	}
	if len(probedRegions) != 0 {
		t.Fatalf("probed upstream with a malformed region %v; validation must "+
			"happen before any network work", probedRegions)
	}
}

// Positive control: a legitimate explicit region must still be accepted and
// persisted through this same path. Without it, "reject every explicit region"
// would satisfy the test above while breaking the panel's api_key add flow.
func TestAddAccountStillAcceptsRealApiKeyRegion(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("topsecret")
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}

	origProbe := probeKiroApiKey
	defer func() { probeKiroApiKey = origProbe }()
	var probedRegions []string
	probeKiroApiKey = func(key, region string) (*config.AccountInfo, error) {
		probedRegions = append(probedRegions, region)
		return &config.AccountInfo{
			Email:  "panel-good@example.com",
			UserId: "kiro-user-panel-good-region",
		}, nil
	}

	rec := serve(h, adminReq(http.MethodPost, "/admin/api/accounts",
		`{"kiroApiKey":"ksk_panelGoodRegion","region":"eu-central-1","enabled":false}`, "topsecret"))
	if rec.Code != http.StatusOK {
		t.Fatalf("legitimate region eu-central-1 was rejected: HTTP %d %s",
			rec.Code, rec.Body.String())
	}

	var found bool
	for _, a := range config.GetAccounts() {
		if a.KiroApiKey == "ksk_panelGoodRegion" {
			found = true
			if a.Region != "eu-central-1" {
				t.Fatalf("account persisted with Region:%q, want eu-central-1", a.Region)
			}
		}
	}
	if !found {
		t.Fatal("account with a valid region was not persisted")
	}
	// The explicit region must still narrow the probe set to exactly that region,
	// rather than silently degrading to probing every candidate.
	if len(probedRegions) != 1 || probedRegions[0] != "eu-central-1" {
		t.Fatalf("probed %v, want exactly [eu-central-1]: an explicit region must "+
			"narrow the probe set, not widen it", probedRegions)
	}
}

// A mixed-case region is a legitimate operator input (AWS labels are lowercase,
// but panels and copy-paste produce "EU-Central-1"). validateRegionOverride
// normalizes case, so the persisted region must be the lowercase form — otherwise
// the dedup bucket ("EU-Central-1") differs by case from the one every other
// path computes, which is the same two-slots-for-one-account outcome by a
// different route.
func TestAddAccountNormalizesApiKeyRegionCase(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("topsecret")
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}

	origProbe := probeKiroApiKey
	defer func() { probeKiroApiKey = origProbe }()
	var probedRegions []string
	probeKiroApiKey = func(key, region string) (*config.AccountInfo, error) {
		probedRegions = append(probedRegions, region)
		return &config.AccountInfo{
			Email:  "panel-case@example.com",
			UserId: "kiro-user-panel-case",
		}, nil
	}

	rec := serve(h, adminReq(http.MethodPost, "/admin/api/accounts",
		`{"kiroApiKey":"ksk_panelCaseRegion","region":"EU-Central-1","enabled":false}`, "topsecret"))
	if rec.Code != http.StatusOK {
		t.Fatalf("mixed-case region EU-Central-1 was rejected: HTTP %d %s",
			rec.Code, rec.Body.String())
	}

	for _, a := range config.GetAccounts() {
		if a.KiroApiKey == "ksk_panelCaseRegion" {
			if a.Region != "eu-central-1" {
				t.Fatalf("account persisted with Region:%q, want the normalized "+
					"lowercase eu-central-1: a case-variant bucket does not match "+
					"the one other paths compute, so dedup misses and the account "+
					"can be added twice", a.Region)
			}
		}
	}
	if len(probedRegions) != 1 || probedRegions[0] != "eu-central-1" {
		t.Fatalf("probed %v, want exactly [eu-central-1]: the normalized region "+
			"must be what reaches the upstream probe", probedRegions)
	}
}
