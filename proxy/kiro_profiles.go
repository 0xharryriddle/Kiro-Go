package proxy

import (
	"fmt"
	"kiro-go/config"
	"os"
	"strings"
)

// KiroProfile is a profile discovered for an existing OAuth/external-IdP
// credential. Current reports the account's cached selection; Pinned reports
// whether that current selection was explicitly chosen by an operator.
type KiroProfile struct {
	Arn     string `json:"arn"`
	Region  string `json:"region"`
	Usable  bool   `json:"usable"`
	Current bool   `json:"current"`
	Pinned  bool   `json:"pinned"`
}

// KiroProfileRegionWarning makes partial discovery explicit. A failed region is
// never silently treated as an authoritative empty result.
type KiroProfileRegionWarning struct {
	Region string `json:"region"`
	Code   string `json:"code"`
}

type KiroProfileDiscovery struct {
	Mode     string                     `json:"mode"`
	Profiles []KiroProfile              `json:"profiles"`
	Warnings []KiroProfileRegionWarning `json:"warnings,omitempty"`
}

// kiroProfileDiscoveryRegions returns the deterministic region order used only
// for the operator-facing all-profile scan. Unlike ordinary lazy resolution it
// deliberately ignores RegionOverride as a restriction so alternatives remain
// discoverable, while still trying that region near the front of the list.
func kiroProfileDiscoveryRegions(account *config.Account) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, 6)
	add := func(region string) {
		region = strings.ToLower(strings.TrimSpace(region))
		if region == "" || seen[region] {
			return
		}
		seen[region] = true
		out = append(out, region)
	}
	if account != nil {
		add(regionFromProfileArn(account.ProfileArn))
		add(account.EffectiveRegionOverride())
		add(account.Region)
	}
	if env := strings.TrimSpace(os.Getenv("KIRO_PROFILE_REGIONS")); env != "" {
		for _, region := range strings.Split(env, ",") {
			add(region)
		}
	} else {
		for _, region := range defaultKiroProfileRegions {
			add(region)
		}
	}
	return out
}

func classifyKiroProfileDiscoveryError(err error) string {
	if err == nil {
		return ""
	}
	lower := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lower, "empty profile list"):
		return "no_profiles"
	case strings.Contains(lower, "401"), strings.Contains(lower, "403"), strings.Contains(lower, "unauthorized"), strings.Contains(lower, "invalid"), strings.Contains(lower, "expired"):
		return "unauthorized"
	case strings.Contains(lower, "429"), strings.Contains(lower, "rate limit"):
		return "rate_limited"
	case strings.Contains(lower, "timeout"), strings.Contains(lower, "connection"), strings.Contains(lower, "no such host"), strings.Contains(lower, "http 5"):
		return "region_unavailable"
	default:
		return "upstream_error"
	}
}

var discoverKiroProfileArnsInRegion = func(account *config.Account, region string) ([]string, error) {
	return listAllAvailableProfilesWithRetryInRegion(account, region)
}

var probeKiroProfileUsability = func(account *config.Account, arn, region string) bool {
	probe := *account
	probe.ProfileArn = strings.TrimSpace(arn)
	probe.ProfilePinned = false
	probe.RegionOverride = strings.ToLower(strings.TrimSpace(region))
	_, err := GetUsageLimits(&probe)
	return err == nil
}

// discoverKiroProfiles performs a read-only, cross-region scan for one existing
// account. It never changes the persisted/current profile. API-key accounts are
// key-bound and intentionally expose no ARN picker.
func discoverKiroProfiles(account *config.Account) (KiroProfileDiscovery, error) {
	if account == nil {
		return KiroProfileDiscovery{}, fmt.Errorf("account is nil")
	}
	if account.IsKiroAPIKeyCredential() {
		return KiroProfileDiscovery{Mode: "key_bound", Profiles: []KiroProfile{}}, nil
	}
	if !account.HasUpstreamCredential() {
		return KiroProfileDiscovery{}, fmt.Errorf("account has no upstream credential")
	}

	currentArn := strings.TrimSpace(account.ProfileArn)
	result := KiroProfileDiscovery{
		Mode:     "selectable",
		Profiles: make([]KiroProfile, 0),
		Warnings: make([]KiroProfileRegionWarning, 0),
	}
	seen := make(map[string]bool)
	for _, region := range kiroProfileDiscoveryRegions(account) {
		// Discovery ignores the account's current pin as a search restriction.
		probe := *account
		probe.ProfileArn = ""
		probe.ProfilePinned = false
		probe.RegionOverride = ""
		arns, err := discoverKiroProfileArnsInRegion(&probe, region)
		if err != nil {
			result.Warnings = append(result.Warnings, KiroProfileRegionWarning{
				Region: region,
				Code:   classifyKiroProfileDiscoveryError(err),
			})
			continue
		}
		for _, arn := range arns {
			arn = strings.TrimSpace(arn)
			if arn == "" || seen[arn] {
				continue
			}
			arnRegion := regionFromProfileArn(arn)
			if arnRegion == "" {
				result.Warnings = append(result.Warnings, KiroProfileRegionWarning{
					Region: region,
					Code:   "invalid_profile_arn",
				})
				continue
			}
			seen[arn] = true
			result.Profiles = append(result.Profiles, KiroProfile{
				Arn:     arn,
				Region:  arnRegion,
				Usable:  probeKiroProfileUsability(account, arn, arnRegion),
				Current: arn == currentArn,
				Pinned:  arn == currentArn && account.ProfilePinned,
			})
		}
	}
	return result, nil
}
