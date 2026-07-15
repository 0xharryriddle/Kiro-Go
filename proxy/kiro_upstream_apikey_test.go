package proxy

import (
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestApplyKiroBaseHeadersCredentialMatrix(t *testing.T) {
	tests := []struct {
		name          string
		account       *config.Account
		authorization string
		tokenType     string
	}{
		{
			name:          "OAuth remains unchanged",
			account:       &config.Account{AuthMethod: "social", AccessToken: "oauth-access"},
			authorization: "Bearer oauth-access",
		},
		{
			name:          "external IdP remains unchanged",
			account:       &config.Account{AuthMethod: " EXTERNAL_IDP ", AccessToken: "external-access"},
			authorization: "Bearer external-access", tokenType: "EXTERNAL_IDP",
		},
		{
			name:          "Kiro API key",
			account:       &config.Account{AuthMethod: "api_key", KiroApiKey: "ksk_upstream", AccessToken: "must-not-win"},
			authorization: "Bearer ksk_upstream", tokenType: "API_KEY",
		},
		{
			name:    "empty Kiro API key emits no auth",
			account: &config.Account{AuthMethod: "api_key", AccessToken: "must-not-win"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "https://example.invalid/test", nil)
			applyKiroBaseHeaders(req, tt.account, kiroHeaderValues{Host: "q.us-east-1.amazonaws.com"})
			if got := req.Header.Get("Authorization"); got != tt.authorization {
				t.Fatalf("Authorization = %q, want %q", got, tt.authorization)
			}
			if got := req.Header.Get("TokenType"); got != tt.tokenType {
				t.Fatalf("TokenType = %q, want %q", got, tt.tokenType)
			}
			if req.Host != "q.us-east-1.amazonaws.com" {
				t.Fatalf("request host = %q", req.Host)
			}
		})
	}
}

func TestResolveProfileArnSoftSkipsKiroAPIKeyAccount(t *testing.T) {
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("Kiro API-key profile resolution must not make a network request")
			return nil, nil
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	account := &config.Account{AuthMethod: "api_key", KiroApiKey: "ksk_test", Region: "us-east-1"}
	profileArn, err := ResolveProfileArn(account)
	if profileArn != "" {
		t.Fatalf("profile ARN = %q, want empty key-bound profile", profileArn)
	}
	if !isProfileArnResolutionSoftError(err) {
		t.Fatalf("expected a soft profile-resolution skip, got %v", err)
	}
}

func TestValidateKiroDispatchProfile(t *testing.T) {
	matching := "arn:aws:codewhisperer:eu-central-1:123456789012:profile/matching"
	mismatch := "arn:aws:codewhisperer:us-east-1:123456789012:profile/mismatch"
	tests := []struct {
		name       string
		account    *config.Account
		profileArn string
		wantErr    bool
	}{
		{name: "nil account"},
		{name: "no override preserves soft behavior", account: &config.Account{AuthMethod: "social"}},
		{name: "OAuth override requires ARN", account: &config.Account{AuthMethod: "social", RegionOverride: "eu-central-1"}, wantErr: true},
		{name: "OAuth matching ARN", account: &config.Account{AuthMethod: "social", RegionOverride: "eu-central-1"}, profileArn: matching},
		{name: "OAuth mismatched ARN", account: &config.Account{AuthMethod: "social", RegionOverride: "eu-central-1"}, profileArn: mismatch, wantErr: true},
		{name: "API key override permits key-bound empty ARN", account: &config.Account{AuthMethod: "api_key", KiroApiKey: "ksk_test", RegionOverride: "eu-central-1"}},
		{name: "API key matching ARN", account: &config.Account{AuthMethod: "api_key", KiroApiKey: "ksk_test", RegionOverride: "eu-central-1"}, profileArn: matching},
		{name: "API key mismatched nonempty ARN", account: &config.Account{AuthMethod: "api_key", KiroApiKey: "ksk_test", RegionOverride: "eu-central-1"}, profileArn: mismatch, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateKiroDispatchProfile(tt.account, tt.profileArn)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateKiroDispatchProfile() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestEnsureValidTokenSkipsKiroAPIKeyRefresh(t *testing.T) {
	h := &Handler{}
	account := &config.Account{
		AuthMethod:   "api_key",
		KiroApiKey:   "ksk_test",
		RefreshToken: "must-not-refresh",
		ExpiresAt:    time.Now().Add(-time.Hour).Unix(),
	}
	if err := h.ensureValidToken(account); err != nil {
		t.Fatalf("API-key token validation should be a no-op, got %v", err)
	}
}

func TestAcceptRefreshedProfileArnPreservesManualPin(t *testing.T) {
	pinned := "arn:aws:codewhisperer:eu-central-1:123456789012:profile/pinned"
	other := "arn:aws:codewhisperer:eu-central-1:123456789012:profile/other"
	account := &config.Account{
		ProfileArn:     pinned,
		ProfilePinned:  true,
		RegionOverride: "eu-central-1",
	}

	if acceptRefreshedProfileArn(account, other) {
		t.Fatal("refresh must not replace a manually pinned profile")
	}
	if account.ProfileArn != pinned {
		t.Fatalf("pinned ARN changed to %q", account.ProfileArn)
	}
	if !acceptRefreshedProfileArn(account, pinned) {
		t.Fatal("refresh echo of the pinned ARN should be accepted as a no-op")
	}
}

func TestReresolveProfileArnSkipsManualPinWithoutNetwork(t *testing.T) {
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("manual pin self-heal must not make a network request")
			return nil, nil
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	pinned := "arn:aws:codewhisperer:eu-central-1:123456789012:profile/pinned"
	account := &config.Account{
		ProfileArn:     pinned,
		ProfilePinned:  true,
		RegionOverride: "eu-central-1",
		AccessToken:    "oauth-access",
	}
	if arn, changed := reresolveProfileArn(account); changed || arn != "" {
		t.Fatalf("manual pin self-healed unexpectedly: arn=%q changed=%v", arn, changed)
	}
	if account.ProfileArn != pinned {
		t.Fatalf("pinned ARN changed to %q", account.ProfileArn)
	}
}
