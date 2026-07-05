package config

import (
	"path/filepath"
	"testing"
)

func TestRecordApiKeyUsagePerModelBreakdown(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := Init(cfgFile); err != nil {
		t.Fatalf("Init: %v", err)
	}
	created, err := AddApiKey(ApiKeyEntry{Key: "sk-model-usage-test", Enabled: true})
	if err != nil {
		t.Fatalf("AddApiKey: %v", err)
	}

	// Two requests on model-a, one on model-b.
	if err := RecordApiKeyUsage(created.ID, 100, 1.0, "model-a"); err != nil {
		t.Fatalf("record a1: %v", err)
	}
	if err := RecordApiKeyUsage(created.ID, 50, 0.5, "model-a"); err != nil {
		t.Fatalf("record a2: %v", err)
	}
	if err := RecordApiKeyUsage(created.ID, 30, 0.25, "model-b"); err != nil {
		t.Fatalf("record b1: %v", err)
	}

	got := GetApiKeyEntry(created.ID)
	if got == nil {
		t.Fatalf("entry missing")
	}
	// Aggregate counters cover all three requests.
	if got.RequestsCount != 3 || got.TokensUsed != 180 {
		t.Fatalf("aggregate mismatch: reqs=%d tokens=%d", got.RequestsCount, got.TokensUsed)
	}
	if got.ModelUsage == nil {
		t.Fatalf("modelUsage not populated")
	}
	a := got.ModelUsage["model-a"]
	if a.Requests != 2 || a.Tokens != 150 || a.Credits != 1.5 {
		t.Fatalf("model-a mismatch: %+v", a)
	}
	b := got.ModelUsage["model-b"]
	if b.Requests != 1 || b.Tokens != 30 || b.Credits != 0.25 {
		t.Fatalf("model-b mismatch: %+v", b)
	}
}

func TestRecordApiKeyUsageEmptyModelBucketsUnderUnknown(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := Init(cfgFile); err != nil {
		t.Fatalf("Init: %v", err)
	}
	created, err := AddApiKey(ApiKeyEntry{Key: "sk-unknown-model", Enabled: true})
	if err != nil {
		t.Fatalf("AddApiKey: %v", err)
	}
	if err := RecordApiKeyUsage(created.ID, 10, 0.1, ""); err != nil {
		t.Fatalf("record: %v", err)
	}
	got := GetApiKeyEntry(created.ID)
	if got == nil || got.ModelUsage == nil {
		t.Fatalf("modelUsage missing")
	}
	if u := got.ModelUsage["unknown"]; u.Requests != 1 || u.Tokens != 10 {
		t.Fatalf("expected empty model under 'unknown', got %+v", u)
	}
}

func TestResetApiKeyUsageClearsModelBreakdown(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := Init(cfgFile); err != nil {
		t.Fatalf("Init: %v", err)
	}
	created, err := AddApiKey(ApiKeyEntry{Key: "sk-reset-model", Enabled: true})
	if err != nil {
		t.Fatalf("AddApiKey: %v", err)
	}
	if err := RecordApiKeyUsage(created.ID, 100, 1.5, "model-a"); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := ResetApiKeyUsage(created.ID); err != nil {
		t.Fatalf("reset: %v", err)
	}
	got := GetApiKeyEntry(created.ID)
	if got == nil {
		t.Fatalf("entry missing")
	}
	if got.ModelUsage != nil {
		t.Fatalf("expected modelUsage cleared, got %+v", got.ModelUsage)
	}
	if got.TokensUsed != 0 || got.RequestsCount != 0 {
		t.Fatalf("aggregate not reset")
	}
}
