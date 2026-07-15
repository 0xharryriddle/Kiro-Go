package proxy

import (
	"fmt"
	"kiro-go/config"
	"reflect"
	"testing"
)

func TestKiroProfileDiscoveryRegionsOrderAndDedup(t *testing.T) {
	t.Setenv("KIRO_PROFILE_REGIONS", "ap-southeast-1,eu-central-1,us-west-2")
	account := &config.Account{
		ProfileArn:     "arn:aws:codewhisperer:eu-central-1:123456789012:profile/current",
		RegionOverride: "us-west-2",
		Region:         "us-east-1",
	}
	got := kiroProfileDiscoveryRegions(account)
	want := []string{"eu-central-1", "us-west-2", "us-east-1", "ap-southeast-1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("regions = %#v, want %#v", got, want)
	}
}

func TestDiscoverKiroProfilesReturnsDeduplicatedPartialResults(t *testing.T) {
	t.Setenv("KIRO_PROFILE_REGIONS", "us-east-1,eu-central-1,ap-southeast-1")
	current := "arn:aws:codewhisperer:eu-central-1:123456789012:profile/current"
	other := "arn:aws:codewhisperer:us-east-1:123456789012:profile/other"
	invalid := "not-an-arn"
	account := &config.Account{
		ID:             "acct",
		AccessToken:    "oauth-access",
		AuthMethod:     "external_idp",
		Region:         "us-east-1",
		RegionOverride: "eu-central-1",
		ProfileArn:     current,
		ProfilePinned:  true,
	}

	oldDiscover := discoverKiroProfileArnsInRegion
	oldUsable := probeKiroProfileUsability
	t.Cleanup(func() {
		discoverKiroProfileArnsInRegion = oldDiscover
		probeKiroProfileUsability = oldUsable
	})

	var probed []string
	discoverKiroProfileArnsInRegion = func(probe *config.Account, region string) ([]string, error) {
		if probe.ProfileArn != "" || probe.ProfilePinned || probe.RegionOverride != "" {
			t.Fatalf("discovery did not clear current pin: %+v", probe)
		}
		probed = append(probed, region)
		switch region {
		case "eu-central-1":
			return []string{current}, nil
		case "us-east-1":
			return []string{other, current, invalid}, nil
		case "ap-southeast-1":
			return nil, fmt.Errorf("HTTP 503: unavailable")
		default:
			return nil, fmt.Errorf("empty profile list")
		}
	}
	probeKiroProfileUsability = func(_ *config.Account, arn, region string) bool {
		return arn == current && region == "eu-central-1"
	}

	got, err := discoverKiroProfiles(account)
	if err != nil {
		t.Fatalf("discoverKiroProfiles: %v", err)
	}
	wantProbed := []string{"eu-central-1", "us-east-1", "ap-southeast-1"}
	if !reflect.DeepEqual(probed, wantProbed) {
		t.Fatalf("probed = %#v, want %#v", probed, wantProbed)
	}
	if got.Mode != "selectable" || len(got.Profiles) != 2 {
		t.Fatalf("unexpected discovery: %+v", got)
	}
	if got.Profiles[0].Arn != current || !got.Profiles[0].Current || !got.Profiles[0].Pinned || !got.Profiles[0].Usable {
		t.Fatalf("unexpected current profile: %+v", got.Profiles[0])
	}
	if got.Profiles[1].Arn != other || got.Profiles[1].Current || got.Profiles[1].Usable {
		t.Fatalf("unexpected alternate profile: %+v", got.Profiles[1])
	}

	warningCodes := map[string]bool{}
	for _, warning := range got.Warnings {
		warningCodes[warning.Region+":"+warning.Code] = true
	}
	if !warningCodes["us-east-1:invalid_profile_arn"] {
		t.Fatalf("missing malformed ARN warning: %+v", got.Warnings)
	}
	if !warningCodes["ap-southeast-1:region_unavailable"] {
		t.Fatalf("missing partial-region warning: %+v", got.Warnings)
	}
}

func TestDiscoverKiroProfilesAPIKeyIsKeyBoundWithoutNetwork(t *testing.T) {
	oldDiscover := discoverKiroProfileArnsInRegion
	t.Cleanup(func() { discoverKiroProfileArnsInRegion = oldDiscover })
	discoverKiroProfileArnsInRegion = func(*config.Account, string) ([]string, error) {
		t.Fatal("key-bound account must not list profiles")
		return nil, nil
	}

	got, err := discoverKiroProfiles(&config.Account{
		AuthMethod: "api_key", KiroApiKey: "ksk_key-bound", RegionOverride: "us-east-1",
	})
	if err != nil {
		t.Fatalf("discoverKiroProfiles: %v", err)
	}
	if got.Mode != "key_bound" || len(got.Profiles) != 0 || len(got.Warnings) != 0 {
		t.Fatalf("unexpected key-bound discovery: %+v", got)
	}
}

func TestDiscoverKiroProfilesRejectsARNFromDifferentProbeRegion(t *testing.T) {
	t.Setenv("KIRO_PROFILE_REGIONS", "us-east-1")
	oldDiscover := discoverKiroProfileArnsInRegion
	oldProbe := probeKiroProfileUsability
	t.Cleanup(func() {
		discoverKiroProfileArnsInRegion = oldDiscover
		probeKiroProfileUsability = oldProbe
	})
	discoverKiroProfileArnsInRegion = func(*config.Account, string) ([]string, error) {
		return []string{"arn:aws:codewhisperer:eu-central-1:123456789012:profile/wrong-region"}, nil
	}
	probeKiroProfileUsability = func(*config.Account, string, string) bool {
		t.Fatal("mismatched ARN must not be usability-probed")
		return false
	}
	got, err := discoverKiroProfiles(&config.Account{ID: "profile-provenance", AuthMethod: "social", AccessToken: "test-access", Region: "us-east-1"})
	if err != nil {
		t.Fatalf("discover profiles: %v", err)
	}
	if len(got.Profiles) != 0 || len(got.Warnings) == 0 || got.Warnings[0].Code != "profile_region_mismatch" {
		t.Fatalf("unexpected discovery result: %+v", got)
	}
}
