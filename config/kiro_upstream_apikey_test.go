package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestAccountUpstreamCredentialHelpers(t *testing.T) {
	tests := []struct {
		name     string
		account  *Account
		has      bool
		bearer   string
		refresh  bool
		kind     string
		isAPIKey bool
	}{
		{name: "nil", account: nil, kind: "none"},
		{name: "empty", account: &Account{}, kind: "none"},
		{
			name:    "oauth",
			account: &Account{AuthMethod: "social", AccessToken: " oauth-access ", RefreshToken: "oauth-refresh"},
			has:     true, bearer: "oauth-access", refresh: true, kind: "oauth",
		},
		{
			name:    "external idp",
			account: &Account{AuthMethod: " EXTERNAL_IDP ", AccessToken: "external-access", RefreshToken: "external-refresh"},
			has:     true, bearer: "external-access", refresh: true, kind: "external_idp",
		},
		{
			name:    "api key is the only source of truth",
			account: &Account{AuthMethod: " API_KEY ", KiroApiKey: " ksk_test ", AccessToken: "must-not-win", RefreshToken: "must-not-refresh"},
			has:     true, bearer: "ksk_test", kind: "api_key", isAPIKey: true,
		},
		{
			name:    "api key mode never falls back to access token",
			account: &Account{AuthMethod: "api_key", AccessToken: "must-not-win", RefreshToken: "must-not-refresh"},
			kind:    "api_key", isAPIKey: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.account.IsKiroAPIKeyCredential(); got != tt.isAPIKey {
				t.Fatalf("IsKiroAPIKeyCredential() = %v, want %v", got, tt.isAPIKey)
			}
			if got := tt.account.HasUpstreamCredential(); got != tt.has {
				t.Fatalf("HasUpstreamCredential() = %v, want %v", got, tt.has)
			}
			if got := tt.account.UpstreamBearerToken(); got != tt.bearer {
				t.Fatalf("UpstreamBearerToken() = %q, want %q", got, tt.bearer)
			}
			if got := tt.account.CanRefreshUpstreamCredential(); got != tt.refresh {
				t.Fatalf("CanRefreshUpstreamCredential() = %v, want %v", got, tt.refresh)
			}
			if got := tt.account.CredentialKind(); got != tt.kind {
				t.Fatalf("CredentialKind() = %q, want %q", got, tt.kind)
			}
		})
	}
}

func TestKiroAPIKeyAccountMigration(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")
	seed := map[string]interface{}{
		"password": "p",
		"port":     8080,
		"host":     "0.0.0.0",
		"accounts": []map[string]interface{}{
			{
				"id": "legacy-spelling", "enabled": true, "authMethod": "apikey",
				"kiroApiKey": "ksk_legacy", "accessToken": "ksk_legacy",
			},
			{
				"id": "implicit-method", "enabled": true, "kiroApiKey": "ksk_implicit",
			},
			{
				"id": "missing-key", "enabled": true, "authMethod": "api_key",
				"accessToken": "must-not-be-used",
			},
			{
				"id": "oauth-unchanged", "enabled": true, "authMethod": "social",
				"accessToken": "oauth-access",
			},
		},
	}
	raw, err := json.MarshalIndent(seed, "", "  ")
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := os.WriteFile(cfgFile, raw, 0600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}
	byID := map[string]Account{}
	for _, account := range GetAccounts() {
		byID[account.ID] = account
	}
	if got := byID["legacy-spelling"]; got.AuthMethod != "api_key" || got.AccessToken != "" || got.KiroApiKey != "ksk_legacy" {
		t.Fatalf("legacy account not normalized safely: %+v", got)
	}
	if got := byID["implicit-method"]; got.AuthMethod != "api_key" || !got.HasUpstreamCredential() {
		t.Fatalf("implicit account not normalized: %+v", got)
	}
	if got := byID["missing-key"]; got.Enabled || got.HasUpstreamCredential() {
		t.Fatalf("keyless api_key account must be disabled and unusable: %+v", got)
	}
	if got := byID["oauth-unchanged"]; got.AuthMethod != "social" || got.AccessToken != "oauth-access" || !got.Enabled {
		t.Fatalf("OAuth account changed during migration: %+v", got)
	}

	var persisted struct {
		Accounts []Account `json:"accounts"`
	}
	persistedRaw, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatalf("read persisted config: %v", err)
	}
	if err := json.Unmarshal(persistedRaw, &persisted); err != nil {
		t.Fatalf("decode persisted config: %v", err)
	}
	for _, account := range persisted.Accounts {
		if account.ID == "legacy-spelling" && account.AccessToken != "" {
			t.Fatal("duplicated Kiro key remained in persisted accessToken")
		}
	}
}
