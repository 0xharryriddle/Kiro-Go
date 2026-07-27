package proxy

import (
	"net/http"
	"testing"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

// The bot supply route accepts an explicit region and persists it verbatim:
//
//	if explicit := strings.TrimSpace(req.Region); explicit != "" {
//	    targetRegions = []string{explicit}      // admin_bot_api.go:660-661
//	...
//	    Region: region,                          // admin_bot_api.go:707
//
// Nothing validates the shape, while the sibling admin routes all run the value
// through validateRegionOverride first (kiro_apikey_admin.go:186, :353;
// handler.go:6452).
//
// Consequence chain for a malformed label such as "bogus":
//
//   - the account is persisted with Region:"bogus";
//   - regionalizeURL rejects that shape and silently leaves traffic on
//     us-east-1, so the stored identity bucket disagrees with where requests
//     actually go;
//   - dedup is keyed on (UserId, region), so the SAME upstream account can be
//     added again under us-east-1 — two pool slots for one account, doubling its
//     routing weight and double-counting its quota in /admin/pool.
//
// The probe is stubbed to SUCCEED for any region, so the only thing that can stop
// a bogus region from being persisted is validation in the route. That is what
// makes this test discriminate rather than passing on a network failure.
func TestBotSupplyRejectsMalformedRegion(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("topsecret")
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}

	// Probe accepts EVERY region, so persistence is reached unless the route
	// refuses the region itself.
	origProbe := probeKiroApiKey
	defer func() { probeKiroApiKey = origProbe }()
	var probedRegions []string
	probeKiroApiKey = func(key, region string) (*config.AccountInfo, error) {
		probedRegions = append(probedRegions, region)
		return &config.AccountInfo{
			Email:  "bot-supply@example.com",
			UserId: "kiro-user-bogus-region",
		}, nil
	}

	rec := serve(h, adminReq(http.MethodPost, "/admin/add_kiro_api_key",
		`{"kiroApiKey":"ksk_bogusRegion","region":"bogus","enabled":false}`, "topsecret"))

	// Any persisted account carrying the bogus region is the defect.
	for _, a := range config.GetAccounts() {
		if a.Region == "bogus" {
			t.Fatalf("account %s was persisted with Region:%q (HTTP %d). regionalizeURL "+
				"rejects that shape and silently routes traffic to us-east-1, so the "+
				"stored region bucket disagrees with reality and the same upstream "+
				"account can be added again under us-east-1 — two pool slots, doubled "+
				"routing weight, double-counted quota", a.ID, a.Region, rec.Code)
		}
	}
	if rec.Code == http.StatusOK {
		t.Fatalf("route returned 200 for region %q; a malformed region must be "+
			"rejected before the upstream probe (probed regions: %v)", "bogus", probedRegions)
	}
	if len(probedRegions) != 0 {
		t.Fatalf("route probed upstream with a malformed region %v; validation must "+
			"happen before any network work", probedRegions)
	}
}

// Positive control: a legitimate explicit region must still be accepted and
// persisted. Without this, "reject every region" would pass the test above while
// breaking the supply path entirely.
func TestBotSupplyStillAcceptsRealRegion(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("topsecret")
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}

	origProbe := probeKiroApiKey
	defer func() { probeKiroApiKey = origProbe }()
	probeKiroApiKey = func(key, region string) (*config.AccountInfo, error) {
		return &config.AccountInfo{
			Email:  "good-region@example.com",
			UserId: "kiro-user-good-region",
		}, nil
	}

	rec := serve(h, adminReq(http.MethodPost, "/admin/add_kiro_api_key",
		`{"kiroApiKey":"ksk_goodRegion","region":"eu-central-1","enabled":false}`, "topsecret"))
	if rec.Code != http.StatusOK {
		t.Fatalf("legitimate region eu-central-1 was rejected: HTTP %d %s",
			rec.Code, rec.Body.String())
	}

	var found bool
	for _, a := range config.GetAccounts() {
		if a.KiroApiKey == "ksk_goodRegion" {
			found = true
			if a.Region != "eu-central-1" {
				t.Fatalf("account persisted with Region:%q, want eu-central-1", a.Region)
			}
		}
	}
	if !found {
		t.Fatal("account with a valid region was not persisted")
	}
}
