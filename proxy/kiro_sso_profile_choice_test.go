package proxy

import (
	"kiro-go/auth"
	"testing"
	"time"
)

func testSsoChoiceProfiles() []KiroProfile {
	return []KiroProfile{
		{Arn: "arn:aws:codewhisperer:us-east-1:123456789012:profile/east", Region: "us-east-1", Usable: true},
		{Arn: "arn:aws:codewhisperer:eu-central-1:123456789012:profile/eu", Region: "eu-central-1", Usable: true},
		{Arn: "arn:aws:codewhisperer:ap-southeast-1:123456789012:profile/unusable", Region: "ap-southeast-1", Usable: false},
	}
}

func TestKiroSsoProfileChoiceStoreInvalidRetryPreservesDeadlineAndCredential(t *testing.T) {
	store := newKiroSsoProfileChoiceStore(time.Minute)
	credential := auth.KiroSsoResult{AccessToken: "access-secret", RefreshToken: "refresh-secret"}
	tokenExpiry := time.Now().Add(30 * time.Minute).Unix()
	deadline, err := store.put("session-1", credential, tokenExpiry, testSsoChoiceProfiles(), nil)
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	if _, _, _, err := store.consume("session-1", "not-offered"); err == nil || err.Error() != "profile_not_offered" {
		t.Fatalf("invalid choice error=%v", err)
	}
	profiles, _, afterDeadline, err := store.get("session-1")
	if err != nil {
		t.Fatalf("get after invalid choice: %v", err)
	}
	if !afterDeadline.Equal(deadline) || len(profiles) != 2 {
		t.Fatalf("invalid retry changed pending entry: deadline=%v want=%v profiles=%+v", afterDeadline, deadline, profiles)
	}

	got, selected, gotExpiry, err := store.consume("session-1", testSsoChoiceProfiles()[0].Arn)
	if err != nil {
		t.Fatalf("valid consume: %v", err)
	}
	if got.AccessToken != credential.AccessToken || got.RefreshToken != credential.RefreshToken || gotExpiry != tokenExpiry {
		t.Fatalf("credential/expiry changed: got=%+v expiry=%d", got, gotExpiry)
	}
	if selected.Region != "us-east-1" {
		t.Fatalf("selected=%+v", selected)
	}
	if _, _, _, err := store.consume("session-1", selected.Arn); err == nil || err.Error() != "profile_choice_not_found" {
		t.Fatalf("second consume error=%v", err)
	}
}

func TestKiroSsoProfileChoiceStoreCancelAndExpiry(t *testing.T) {
	store := newKiroSsoProfileChoiceStore(20 * time.Millisecond)
	credential := auth.KiroSsoResult{AccessToken: "access-secret"}
	if _, err := store.put("cancelled", credential, 123, testSsoChoiceProfiles(), nil); err != nil {
		t.Fatalf("put cancelled: %v", err)
	}
	if !store.cancel("cancelled") {
		t.Fatal("cancel returned false")
	}
	if _, _, _, err := store.get("cancelled"); err == nil || err.Error() != "profile_choice_not_found" {
		t.Fatalf("get cancelled error=%v", err)
	}

	if _, err := store.put("expired", credential, 123, testSsoChoiceProfiles(), nil); err != nil {
		t.Fatalf("put expired: %v", err)
	}
	time.Sleep(40 * time.Millisecond)
	if _, _, _, err := store.get("expired"); err == nil || (err.Error() != "profile_choice_not_found" && err.Error() != "profile_choice_expired") {
		t.Fatalf("get expired error=%v", err)
	}
}

func TestKiroSsoProfileChoiceStoreReturnsDeterministicProfiles(t *testing.T) {
	store := newKiroSsoProfileChoiceStore(time.Minute)
	profiles := []KiroProfile{
		{Arn: "arn:aws:codewhisperer:us-east-1:123456789012:profile/z", Region: "us-east-1", Usable: true},
		{Arn: "arn:aws:codewhisperer:eu-central-1:123456789012:profile/a", Region: "eu-central-1", Usable: true},
	}
	if _, err := store.put("ordered", auth.KiroSsoResult{AccessToken: "access"}, 123, profiles, nil); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, _, _, err := store.get("ordered")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got) != 2 || got[0].Region != "eu-central-1" || got[1].Region != "us-east-1" {
		t.Fatalf("profiles not deterministic: %+v", got)
	}
}
