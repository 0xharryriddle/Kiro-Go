package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUpdateAccountProfileSelectionPinsAndClearsAtomically(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	account := Account{
		ID:             "account-1",
		AuthMethod:     "social",
		Region:         "us-east-1",
		ProfileArn:     "arn:aws:codewhisperer:us-east-1:123456789012:profile/old",
		RegionOverride: "us-east-1",
	}
	if err := AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}

	selectedArn := "arn:aws:codewhisperer:eu-central-1:123456789012:profile/selected"
	changed, err := UpdateAccountProfileSelection(account.ID, selectedArn, "EU-CENTRAL-1", true)
	if err != nil || !changed {
		t.Fatalf("pin selection: changed=%v err=%v", changed, err)
	}
	got, ok := GetAccountByID(account.ID)
	if !ok {
		t.Fatal("account missing after selection")
	}
	if got.ProfileArn != selectedArn || !got.ProfilePinned || got.RegionOverride != "eu-central-1" {
		t.Fatalf("selection was not atomic: %+v", got)
	}
	if got.Region != "us-east-1" {
		t.Fatalf("auth region changed with data-plane selection: %q", got.Region)
	}

	changed, err = UpdateAccountProfileSelection(account.ID, "ignored", "ignored", false)
	if err != nil || !changed {
		t.Fatalf("restore auto mode: changed=%v err=%v", changed, err)
	}
	got, _ = GetAccountByID(account.ID)
	if got.ProfileArn != "" || got.ProfilePinned || got.RegionOverride != "" {
		t.Fatalf("automatic mode did not clear the selection tuple: %+v", got)
	}
}

func TestUpdateAccountProfileArnCannotReplaceManualSelection(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	selectedArn := "arn:aws:codewhisperer:eu-central-1:123456789012:profile/selected"
	account := Account{
		ID: "account-1", AuthMethod: "social", Region: "us-east-1",
		ProfileArn: selectedArn, ProfilePinned: true, RegionOverride: "eu-central-1",
	}
	if err := AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}

	if err := UpdateAccountProfileArn(account.ID,
		"arn:aws:codewhisperer:us-east-1:123456789012:profile/replacement"); err == nil {
		t.Fatal("automatic profile write replaced a manual selection")
	}
	got, _ := GetAccountByID(account.ID)
	if got.ProfileArn != selectedArn || !got.ProfilePinned || got.RegionOverride != "eu-central-1" {
		t.Fatalf("manual selection changed after rejected write: %+v", got)
	}
	if err := UpdateAccountProfileArn(account.ID, selectedArn); err != nil {
		t.Fatalf("idempotent write of selected ARN should be allowed: %v", err)
	}
}

func TestUpdateAccountRegionOverrideExitsManualProfileMode(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	account := Account{
		ID: "account-1", AuthMethod: "social", Region: "us-east-1",
		ProfileArn:    "arn:aws:codewhisperer:eu-central-1:123456789012:profile/selected",
		ProfilePinned: true, RegionOverride: "eu-central-1",
	}
	if err := AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}

	changed, err := UpdateAccountRegionOverride(account.ID, "ap-southeast-1")
	if err != nil || !changed {
		t.Fatalf("change override: changed=%v err=%v", changed, err)
	}
	got, _ := GetAccountByID(account.ID)
	if got.RegionOverride != "ap-southeast-1" || got.ProfileArn != "" || got.ProfilePinned {
		t.Fatalf("standalone region change left stale manual profile state: %+v", got)
	}
}

func TestUpdateAccountPreservesAtomicProfileTuple(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	selectedArn := "arn:aws:codewhisperer:eu-central-1:123456789012:profile/selected"
	account := Account{
		ID: "account-1", AuthMethod: "social", Region: "us-east-1", Nickname: "before",
		ProfileArn: selectedArn, ProfilePinned: true, RegionOverride: "eu-central-1",
	}
	if err := AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}

	stale := account
	stale.Nickname = "after"
	stale.ProfileArn = ""
	stale.ProfilePinned = false
	stale.RegionOverride = ""
	if err := UpdateAccount(account.ID, stale); err != nil {
		t.Fatalf("update account: %v", err)
	}
	got, _ := GetAccountByID(account.ID)
	if got.Nickname != "after" {
		t.Fatalf("ordinary field was not updated: %+v", got)
	}
	if got.ProfileArn != selectedArn || !got.ProfilePinned || got.RegionOverride != "eu-central-1" {
		t.Fatalf("stale whole-row update clobbered profile tuple: %+v", got)
	}

	replacement := got
	replacement.ProfileArn = ""
	replacement.ProfilePinned = false
	replacement.RegionOverride = ""
	if err := ReplaceAccount(account.ID, replacement); err != nil {
		t.Fatalf("replace account: %v", err)
	}
	got, _ = GetAccountByID(account.ID)
	if got.ProfileArn != "" || got.ProfilePinned || got.RegionOverride != "" {
		t.Fatalf("explicit replacement did not replace profile tuple: %+v", got)
	}
}

func TestAddAccountRollsBackWhenDurableWriteFails(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	oldPath := cfgPath
	t.Cleanup(func() { cfgPath = oldPath })

	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("block parent creation"), 0o600); err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	cfgPath = filepath.Join(blocker, "config.json")
	if err := AddAccount(Account{ID: "must-rollback", AuthMethod: "social"}); err == nil {
		t.Fatal("AddAccount unexpectedly succeeded with an invalid config parent")
	}
	if _, ok := GetAccountByID("must-rollback"); ok || len(GetAccounts()) != 0 {
		t.Fatalf("failed durable write left account in memory: %+v", GetAccounts())
	}
}

func TestProfileMutationPathsRollBackWhenDurableWriteFails(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	original := Account{
		ID: "rollback-profile", AuthMethod: "social", Region: "us-east-1", Nickname: "original",
		ProfileArn:    "arn:aws:codewhisperer:eu-central-1:123456789012:profile/selected",
		ProfilePinned: true, RegionOverride: "eu-central-1",
	}
	if err := AddAccount(original); err != nil {
		t.Fatalf("add account: %v", err)
	}
	oldPath := cfgPath
	t.Cleanup(func() { cfgPath = oldPath })
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("block parent creation"), 0o600); err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	cfgPath = filepath.Join(blocker, "config.json")

	if changed, err := UpdateAccountRegionOverride(original.ID, "ap-southeast-1"); err == nil || changed {
		t.Fatalf("region override failure: changed=%v err=%v", changed, err)
	}
	got, _ := GetAccountByID(original.ID)
	if got.ProfileArn != original.ProfileArn || got.ProfilePinned != original.ProfilePinned || got.RegionOverride != original.RegionOverride {
		t.Fatalf("failed region write changed memory: %+v", got)
	}

	replacement := original
	replacement.Nickname = "replacement"
	replacement.ProfileArn = ""
	replacement.ProfilePinned = false
	replacement.RegionOverride = ""
	if err := ReplaceAccount(original.ID, replacement); err == nil {
		t.Fatal("replacement unexpectedly succeeded")
	}
	got, _ = GetAccountByID(original.ID)
	if got.Nickname != original.Nickname || got.ProfileArn != original.ProfileArn || !got.ProfilePinned || got.RegionOverride != original.RegionOverride {
		t.Fatalf("failed replacement changed memory: %+v", got)
	}

	ordinary := original
	ordinary.Nickname = "ordinary-update"
	if err := UpdateAccount(original.ID, ordinary); err == nil {
		t.Fatal("ordinary update unexpectedly succeeded")
	}
	got, _ = GetAccountByID(original.ID)
	if got.Nickname != original.Nickname {
		t.Fatalf("failed ordinary update changed memory: %+v", got)
	}
}

func TestReplaceAccountAndDeleteIsAtomic(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	original := Account{ID: "existing", Nickname: "old", AuthMethod: "social"}
	temporary := Account{ID: "temporary", Nickname: "new", AuthMethod: "api_key"}
	if err := AddAccount(original); err != nil {
		t.Fatalf("add original: %v", err)
	}
	if err := AddAccount(temporary); err != nil {
		t.Fatalf("add temporary: %v", err)
	}
	if err := ReplaceAccountAndDelete(original.ID, temporary.ID, temporary); err != nil {
		t.Fatalf("atomic replacement: %v", err)
	}
	got, ok := GetAccountByID(original.ID)
	if !ok || got.Nickname != temporary.Nickname || got.ID != original.ID {
		t.Fatalf("unexpected replacement: %+v", got)
	}
	if _, ok := GetAccountByID(temporary.ID); ok || len(GetAccounts()) != 1 {
		t.Fatalf("temporary account survived replacement: %+v", GetAccounts())
	}
}
