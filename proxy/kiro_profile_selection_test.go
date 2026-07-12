package proxy

import (
	"io"
	"kiro-go/config"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// TestIsProfileOrPlanAuthzError verifies that a profile/plan 403 is distinguished
// from a genuine token-invalid 401/403 so the ban classifier does not auto-ban a
// valid account whose in-plan profile simply was not selected.
func TestIsProfileOrPlanAuthzError(t *testing.T) {
	cases := []struct {
		name   string
		errMsg string
		want   bool
	}{
		{"plan not authorized 403", `HTTP 403: {"message":"User is not authorized for this profile"}`, true},
		{"no active subscription 403", `HTTP 403: {"message":"No active subscription for profile"}`, true},
		{"access denied 403", `HTTP 403: {"__type":"AccessDeniedException","message":"not entitled"}`, true},
		{"token invalid 403 is NOT plan", `HTTP 403: {"message":"token is invalid"}`, false},
		{"token expired 403 is NOT plan", `HTTP 403: {"message":"credential expired"}`, false},
		{"401 is never a plan error", `HTTP 401: {"message":"not authorized"}`, false},
		{"suspension is not plan", `TEMPORARILY_SUSPENDED`, false},
		{"generic 500 is not plan", `HTTP 500: internal`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isProfileOrPlanAuthzError(tc.errMsg); got != tc.want {
				t.Fatalf("isProfileOrPlanAuthzError(%q) = %v, want %v", tc.errMsg, got, tc.want)
			}
		})
	}
}

// TestResolveProfileArnPrefersInPlanProfile reproduces the two-profile account:
// KiroProfile-us-east-1 (not in plan) and KiroProfile-eu-central-1 (in plan).
// The resolver must NOT blindly cache the first (us-east-1) profile — it must
// verify each via getUsageLimits and cache the in-plan eu-central-1 one.
func TestResolveProfileArnPrefersInPlanProfile(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(configPath); err != nil {
		t.Fatalf("init config: %v", err)
	}
	const (
		usEastArn = "arn:aws:codewhisperer:us-east-1:155119901513:profile/USEASTNOTINPLAN"
		euArn     = "arn:aws:codewhisperer:eu-central-1:155119901513:profile/9GARC7EDVKG9"
	)
	account := config.Account{
		ID:          "multi-profile-1",
		Email:       "petros@example.com",
		AccessToken: "access-token",
		AuthMethod:  "external_idp",
		Region:      "us-east-1", // external_idp login default; real profile is eu-central-1
		Enabled:     true,
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}

	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			isEU := strings.Contains(req.URL.Host, "eu-central-1")
			switch req.URL.Path {
			case "/ListAvailableProfiles":
				// us-east-1 exposes the not-in-plan profile; eu-central-1 exposes
				// the in-plan one. (Each region returns its own profile.)
				arn := usEastArn
				if isEU {
					arn = euArn
				}
				return jsonResp(http.StatusOK, `{"profiles":[{"arn":"`+arn+`"}]}`), nil
			case "/getUsageLimits":
				// The not-in-plan (us-east-1) profile is rejected with a plan 403;
				// the in-plan (eu-central-1) profile succeeds.
				if strings.Contains(req.URL.RawQuery, "USEASTNOTINPLAN") {
					return jsonResp(http.StatusForbidden, `{"message":"profile has no active subscription"}`), nil
				}
				return jsonResp(http.StatusOK, `{"subscriptionInfo":{"subscriptionTitle":"Kiro Power"}}`), nil
			default:
				t.Fatalf("unexpected path %s", req.URL.Path)
				return nil, nil
			}
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	req := account
	got, err := ResolveProfileArn(&req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != euArn {
		t.Fatalf("expected in-plan eu-central-1 profile %q, got %q", euArn, got)
	}
}

// TestRefreshAccountInfoDoesNotBanOnPlan403 verifies that when getUsageLimits
// returns a profile/plan 403 (and self-heal cannot find a usable alternative),
// the account is NOT flipped to BANNED.
func TestRefreshAccountInfoDoesNotBanOnPlan403(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(configPath); err != nil {
		t.Fatalf("init config: %v", err)
	}
	account := config.Account{
		ID:          "plan403-1",
		Email:       "petros@example.com",
		AccessToken: "access-token",
		AuthMethod:  "external_idp",
		Region:      "us-east-1",
		// Already has a cached profile so ResolveProfileArn short-circuits and the
		// getUsageLimits call goes straight to the plan-403 path.
		ProfileArn: "arn:aws:codewhisperer:us-east-1:155119901513:profile/ONLYNOTINPLAN",
		Enabled:    true,
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}

	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/getUsageLimits":
				return jsonResp(http.StatusForbidden, `{"message":"profile is not authorized - no active subscription"}`), nil
			case "/ListAvailableProfiles":
				// Self-heal probe: only the same not-in-plan profile exists, so no
				// usable alternative is found.
				return jsonResp(http.StatusOK, `{"profiles":[{"arn":"arn:aws:codewhisperer:us-east-1:155119901513:profile/ONLYNOTINPLAN"}]}`), nil
			default:
				t.Fatalf("unexpected path %s", req.URL.Path)
				return nil, nil
			}
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	req := account
	if _, err := RefreshAccountInfo(&req); err == nil {
		t.Fatal("expected getUsageLimits error to surface")
	}
	accounts := config.GetAccounts()
	if len(accounts) != 1 {
		t.Fatalf("expected one account, got %d", len(accounts))
	}
	if accounts[0].BanStatus == "BANNED" {
		t.Fatalf("plan 403 must NOT ban the account, got banStatus=%q reason=%q",
			accounts[0].BanStatus, accounts[0].BanReason)
	}
}

// TestListAvailableModelsSelfHealsPoisonedProfile verifies that when the cached
// profile is the not-in-plan one, ListAvailableModels re-resolves to the in-plan
// profile and retries — this is the exact path the re-enable/ModelsCache refresh
// hits, which previously 403'd with "not authorized to make this call" and never
// recovered.
func TestListAvailableModelsSelfHealsPoisonedProfile(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(configPath); err != nil {
		t.Fatalf("init config: %v", err)
	}
	const (
		usEastArn = "arn:aws:codewhisperer:us-east-1:155119901513:profile/USEASTNOTINPLAN"
		euArn     = "arn:aws:codewhisperer:eu-central-1:155119901513:profile/INPLAN"
	)
	account := config.Account{
		ID:          "models-heal-1",
		Email:       "petros@example.com",
		AccessToken: "access-token",
		AuthMethod:  "external_idp",
		Region:      "us-east-1",
		// Poisoned: the not-in-plan us-east-1 profile is already cached.
		ProfileArn: usEastArn,
		Enabled:    true,
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}

	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			isEU := strings.Contains(req.URL.Host, "eu-central-1")
			switch req.URL.Path {
			case "/ListAvailableModels":
				// The not-in-plan (us-east-1) profile is rejected; the in-plan
				// (eu-central-1) profile serves the model list.
				if strings.Contains(req.URL.RawQuery, "USEASTNOTINPLAN") {
					return jsonResp(http.StatusForbidden, `{"message":"Your account is not authorized to make this call."}`), nil
				}
				return jsonResp(http.StatusOK, `{"models":[{"modelId":"claude-sonnet-4"}]}`), nil
			case "/ListAvailableProfiles":
				arn := usEastArn
				if isEU {
					arn = euArn
				}
				return jsonResp(http.StatusOK, `{"profiles":[{"arn":"`+arn+`"}]}`), nil
			case "/getUsageLimits":
				// selectUsableProfile probe: not-in-plan fails, in-plan succeeds.
				if strings.Contains(req.URL.RawQuery, "USEASTNOTINPLAN") {
					return jsonResp(http.StatusForbidden, `{"message":"profile has no active subscription"}`), nil
				}
				return jsonResp(http.StatusOK, `{"subscriptionInfo":{"subscriptionTitle":"Kiro Power"}}`), nil
			default:
				t.Fatalf("unexpected path %s", req.URL.Path)
				return nil, nil
			}
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	req := account
	models, err := ListAvailableModels(&req)
	if err != nil {
		t.Fatalf("expected self-heal to recover model list, got %v", err)
	}
	if len(models) != 1 || models[0].ModelId != "claude-sonnet-4" {
		t.Fatalf("expected recovered model list, got %+v", models)
	}
	if req.ProfileArn != euArn {
		t.Fatalf("expected profile self-healed to in-plan %q, got %q", euArn, req.ProfileArn)
	}
}

func jsonResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}
