package proxy

import (
	"kiro-go/config"
	"testing"
)

// TestRegionalizeURLForRegion asserts that a non-us-east-1 region collapses BOTH
// hardcoded us-east-1 hosts (q.* and codewhisperer.*) onto q.{region} — there is no
// codewhisperer.{region} host — and that us-east-1/empty are no-ops.
func TestRegionalizeURLForRegion(t *testing.T) {
	cases := []struct {
		name   string
		rawURL string
		region string
		want   string
	}{
		{
			name:   "codewhisperer host to q.eu-central-1",
			rawURL: "https://codewhisperer.us-east-1.amazonaws.com/ListAvailableProfiles",
			region: "eu-central-1",
			want:   "https://q.eu-central-1.amazonaws.com/ListAvailableProfiles",
		},
		{
			name:   "q host to q.eu-central-1",
			rawURL: "https://q.us-east-1.amazonaws.com/generateAssistantResponse",
			region: "eu-central-1",
			want:   "https://q.eu-central-1.amazonaws.com/generateAssistantResponse",
		},
		{
			name:   "us-east-1 is a no-op (codewhisperer host kept)",
			rawURL: "https://codewhisperer.us-east-1.amazonaws.com/ListAvailableProfiles",
			region: "us-east-1",
			want:   "https://codewhisperer.us-east-1.amazonaws.com/ListAvailableProfiles",
		},
		{
			name:   "empty region is a no-op",
			rawURL: "https://q.us-east-1.amazonaws.com/x",
			region: "",
			want:   "https://q.us-east-1.amazonaws.com/x",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := regionalizeURLForRegion(tc.rawURL, tc.region)
			if got != tc.want {
				t.Fatalf("regionalizeURLForRegion(%q, %q) = %q, want %q", tc.rawURL, tc.region, got, tc.want)
			}
		})
	}
}

// TestRegionalizeURLForRegionNoCodewhispererRegionalHost guards the user-stated
// invariant directly: a regionalized URL must never produce codewhisperer.{region}.
func TestRegionalizeURLForRegionNoCodewhispererRegionalHost(t *testing.T) {
	got := regionalizeURLForRegion("https://codewhisperer.us-east-1.amazonaws.com/GetUserInfo", "eu-central-1")
	if want := "https://q.eu-central-1.amazonaws.com/GetUserInfo"; got != want {
		t.Fatalf("got %q, want %q (must not be codewhisperer.eu-central-1)", got, want)
	}
}

// TestKiroProfileRegionCandidatesExternalIdp checks that an external_idp account —
// whose home region is unknown and defaults to us-east-1 — probes the account region
// first and then the built-in fallbacks, de-duplicated.
func TestKiroProfileRegionCandidatesExternalIdp(t *testing.T) {
	// Default us-east-1 external_idp login: fallbacks follow.
	got := kiroProfileRegionCandidates(&config.Account{AuthMethod: "external_idp", Region: "us-east-1"})
	assertOrder(t, got, []string{"us-east-1", "eu-central-1"})

	// Already-detected eu-central-1 leads; us-east-1 fallback follows.
	got = kiroProfileRegionCandidates(&config.Account{AuthMethod: "external_idp", Region: "eu-central-1"})
	assertOrder(t, got, []string{"eu-central-1", "us-east-1"})

	// A non-default region leads, both defaults follow.
	got = kiroProfileRegionCandidates(&config.Account{AuthMethod: "external_idp", Region: "ap-southeast-2"})
	assertOrder(t, got, []string{"ap-southeast-2", "us-east-1", "eu-central-1"})
}

// TestKiroProfileRegionCandidatesNoRegion checks an account with no region set falls
// back across the defaults regardless of auth method.
func TestKiroProfileRegionCandidatesNoRegion(t *testing.T) {
	got := kiroProfileRegionCandidates(&config.Account{})
	assertOrder(t, got, []string{"us-east-1", "eu-central-1"})
}

// TestKiroProfileRegionCandidatesSingleRegionAuthMethods checks that idc/social/
// Builder ID accounts — which already carry their authoritative region — are probed
// against that single region only, with no fallback probing.
func TestKiroProfileRegionCandidatesSingleRegionAuthMethods(t *testing.T) {
	for _, method := range []string{"idc", "social", "builderId", ""} {
		got := kiroProfileRegionCandidates(&config.Account{AuthMethod: method, Region: "eu-central-1"})
		if len(got) != 1 || got[0] != "eu-central-1" {
			t.Fatalf("authMethod %q: candidate regions = %v, want [eu-central-1] only", method, got)
		}
	}
}

// TestKiroProfileRegionCandidatesEnvOverride checks KIRO_PROFILE_REGIONS replaces
// the built-in fallbacks (external_idp only) while the account region is tried first.
func TestKiroProfileRegionCandidatesEnvOverride(t *testing.T) {
	t.Setenv("KIRO_PROFILE_REGIONS", "eu-west-1, ap-south-1 ,eu-west-1")
	got := kiroProfileRegionCandidates(&config.Account{AuthMethod: "external_idp", Region: "us-east-1"})
	// us-east-1 (account) first; env values de-duplicated and trimmed; no built-in defaults.
	assertOrder(t, got, []string{"us-east-1", "eu-west-1", "ap-south-1"})

	// A non-external_idp account ignores the env fallbacks entirely.
	got = kiroProfileRegionCandidates(&config.Account{AuthMethod: "idc", Region: "us-east-1"})
	assertOrder(t, got, []string{"us-east-1"})
}

// TestKiroProfileRegionCandidatesEnterpriseIdC checks that a Kiro IDE
// "Enterprise" import (authMethod idc, provider Enterprise) probes fallback
// regions too. Its credential cache stores the SSO/auth region, which is NOT
// necessarily where the profile lives (observed: cache region eu-central-1 but
// the actual profile in us-east-1), so it must behave like external_idp and not
// be pinned to its single cached region.
func TestKiroProfileRegionCandidatesEnterpriseIdC(t *testing.T) {
	got := kiroProfileRegionCandidates(&config.Account{AuthMethod: "idc", Provider: "Enterprise", Region: "eu-central-1"})
	assertOrder(t, got, []string{"eu-central-1", "us-east-1"})

	// A plain idc account with a non-Enterprise provider stays single-region.
	got = kiroProfileRegionCandidates(&config.Account{AuthMethod: "idc", Provider: "BuilderId", Region: "eu-central-1"})
	if len(got) != 1 || got[0] != "eu-central-1" {
		t.Fatalf("non-Enterprise idc should stay single-region, got %v", got)
	}
}

func assertOrder(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("candidate regions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidate regions = %v, want %v", got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Per-account data-plane region override
// ---------------------------------------------------------------------------

// TestKiroRegionForProfileOverrideWins is the core subtlety: a region override
// must beat the cached-profile-ARN region (which otherwise dominates account.Region).
func TestKiroRegionForProfileOverrideWins(t *testing.T) {
	acc := &config.Account{
		Region:         "us-east-1",
		ProfileArn:     "arn:aws:codewhisperer:us-east-1:000000000000:profile/SAMPLE",
		RegionOverride: "eu-central-1",
	}
	// Even with a payload ARN in yet another region, the override wins.
	if got := kiroRegionForProfile(acc, "arn:aws:codewhisperer:ap-southeast-2:000000000000:profile/OTHER"); got != "eu-central-1" {
		t.Fatalf("override must win over ARN-derived region, got %q", got)
	}
}

// TestKiroRegionForProfileNoOverrideUnchanged proves the existing precedence
// ladder is preserved byte-for-byte when no override is set.
func TestKiroRegionForProfileNoOverrideUnchanged(t *testing.T) {
	// Cached ARN region wins over account.Region.
	acc := &config.Account{
		Region:     "us-east-1",
		ProfileArn: "arn:aws:codewhisperer:eu-central-1:000000000000:profile/SAMPLE",
	}
	if got := kiroRegionForProfile(acc, ""); got != "eu-central-1" {
		t.Fatalf("ARN region should win with no override, got %q", got)
	}
	// No ARN → account.Region.
	acc2 := &config.Account{Region: "ap-southeast-2"}
	if got := kiroRegionForProfile(acc2, ""); got != "ap-southeast-2" {
		t.Fatalf("account.Region fallback broken, got %q", got)
	}
	// Nothing → us-east-1 default.
	if got := kiroRegionForProfile(&config.Account{}, ""); got != "us-east-1" {
		t.Fatalf("default region broken, got %q", got)
	}
}

// TestKiroProfileRegionCandidatesOverridePinsSingleRegion proves an override
// pins discovery to exactly that one region regardless of auth method / env.
func TestKiroProfileRegionCandidatesOverridePinsSingleRegion(t *testing.T) {
	t.Setenv("KIRO_PROFILE_REGIONS", "eu-west-1,ap-south-1")
	for _, method := range []string{"idc", "social", "external_idp", "builderId", ""} {
		acc := &config.Account{AuthMethod: method, Region: "us-east-1", RegionOverride: "eu-central-1"}
		got := kiroProfileRegionCandidates(acc)
		if len(got) != 1 || got[0] != "eu-central-1" {
			t.Fatalf("authMethod %q: override must pin to [eu-central-1], got %v", method, got)
		}
	}
}

// TestArnRegionAllowed covers the guard used at every ARN-acceptance path.
func TestArnRegionAllowed(t *testing.T) {
	noOverride := &config.Account{}
	// No override → any ARN allowed.
	if !arnRegionAllowed(noOverride, "arn:aws:codewhisperer:ap-southeast-2:0:profile/X") {
		t.Fatal("no override must allow any ARN")
	}
	ov := &config.Account{RegionOverride: "eu-central-1"}
	// Matching region → allowed.
	if !arnRegionAllowed(ov, "arn:aws:codewhisperer:eu-central-1:0:profile/X") {
		t.Fatal("matching-region ARN must be allowed")
	}
	// Mismatched region → refused.
	if arnRegionAllowed(ov, "arn:aws:codewhisperer:us-east-1:0:profile/X") {
		t.Fatal("cross-region ARN must be refused under override")
	}
	// Empty ARN → allowed (means not-yet-resolved; dispatch gates on emptiness).
	if !arnRegionAllowed(ov, "") {
		t.Fatal("empty ARN must be allowed (resolution not yet done)")
	}
}

// TestValidateRegionOverride covers the API-level validator: empty clears,
// well-formed AWS regions (incl. us-gov multi-part) pass, junk is rejected.
func TestValidateRegionOverride(t *testing.T) {
	ok := []string{"us-east-1", "eu-central-1", "ap-southeast-2", "us-gov-east-1", "us-gov-west-1", "ap-northeast-3"}
	for _, r := range ok {
		if got, valid := validateRegionOverride(r); !valid || got != r {
			t.Fatalf("expected %q valid, got (%q,%v)", r, got, valid)
		}
	}
	// Empty clears (valid, normalized to "").
	if got, valid := validateRegionOverride("  "); !valid || got != "" {
		t.Fatalf("blank must clear, got (%q,%v)", got, valid)
	}
	// Case-normalized.
	if got, valid := validateRegionOverride("US-East-1"); !valid || got != "us-east-1" {
		t.Fatalf("must lower-case, got (%q,%v)", got, valid)
	}
	bad := []string{"useast1", "us_east_1", "us-east", "-us-east-1", "us-east-1-", "us--east-1", "us-east-x", "region with space"}
	for _, r := range bad {
		if _, valid := validateRegionOverride(r); valid {
			t.Fatalf("expected %q invalid", r)
		}
	}
}
