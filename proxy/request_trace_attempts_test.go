package proxy

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"kiro-go/config"
)

// A request that fails over across accounts must keep every attempt visible on a
// single record, so the quota error that caused the reroute is not erased by the
// eventual success.
func TestTraceRecorderKeepsEveryFailoverAttempt(t *testing.T) {
	rec := newTraceRecorder("claude", "sonnet", true, "key-1")

	a1 := rec.beginAttempt(&config.Account{ID: "acc-1", Email: "one@example.com", Region: "us-east-1"})
	rec.endAttempt(a1, errors.New("HTTP 429 from us-east-1: quota exhausted"))

	a2 := rec.beginAttempt(&config.Account{ID: "acc-2", Email: "two@example.com", Region: "eu-central-1"})
	rec.endAttempt(a2, errors.New("HTTP 500 from eu-central-1: boom"))

	a3 := rec.beginAttempt(&config.Account{ID: "acc-3", Email: "three@example.com", Region: "eu-central-1"})
	rec.endAttempt(a3, nil)

	entry := rec.finish(outcomeSuccess, 200)

	if entry.AttemptCount != 3 {
		t.Fatalf("AttemptCount = %d, want 3", entry.AttemptCount)
	}
	if len(entry.Attempts) != 3 {
		t.Fatalf("len(Attempts) = %d, want 3", len(entry.Attempts))
	}
	if entry.Attempts[0].AccountID != "acc-1" || entry.Attempts[1].AccountID != "acc-2" || entry.Attempts[2].AccountID != "acc-3" {
		t.Fatalf("attempt account IDs not distinct/ordered: %#v", entry.Attempts)
	}
	if entry.Attempts[0].ErrorType != "quota" {
		t.Fatalf("first attempt errorType = %q, want quota", entry.Attempts[0].ErrorType)
	}
	if entry.Attempts[0].Outcome != outcomeError || entry.Attempts[2].Outcome != outcomeSuccess {
		t.Fatalf("unexpected attempt outcomes: %q %q", entry.Attempts[0].Outcome, entry.Attempts[2].Outcome)
	}
	for i, a := range entry.Attempts {
		if a.Seq != i+1 {
			t.Fatalf("attempt %d has Seq %d", i, a.Seq)
		}
	}
	// The record must be joinable and carry the request-level context.
	if !strings.HasPrefix(entry.RequestID, "trc_") {
		t.Fatalf("RequestID = %q, want trc_ prefix", entry.RequestID)
	}
	if entry.API != "claude" || !entry.Stream || entry.ApiKeyID != "key-1" {
		t.Fatalf("request context lost: %#v", entry)
	}
	if entry.HTTPStatus != 200 || entry.Outcome != outcomeSuccess {
		t.Fatalf("outcome/status = %q/%d", entry.Outcome, entry.HTTPStatus)
	}
	// Routing context is taken from the attempt that actually served the request.
	if entry.Region != "eu-central-1" {
		t.Fatalf("Region = %q, want the serving attempt's region", entry.Region)
	}
}

func TestTraceRecorderRecordsTTFBOnce(t *testing.T) {
	rec := newTraceRecorder("claude", "sonnet", true, "")
	time.Sleep(2 * time.Millisecond)
	rec.markFirstByte()
	first := rec.finish(outcomeSuccess, 200).TTFBMs
	if first <= 0 {
		t.Fatalf("TTFBMs = %d, want > 0", first)
	}
	time.Sleep(5 * time.Millisecond)
	rec.markFirstByte()
	if again := rec.finish(outcomeSuccess, 200).TTFBMs; again != first {
		t.Fatalf("TTFBMs changed on second mark: %d -> %d", first, again)
	}
}

// A trace record must never carry upstream credential material, even when an
// upstream error message quotes the offending header.
func TestTraceRecorderScrubsSecretsFromAttemptErrors(t *testing.T) {
	rec := newTraceRecorder("openai", "gpt", false, "")
	a := rec.beginAttempt(&config.Account{ID: "acc-1", KiroApiKey: "ksk_live_supersecret"})
	rec.endAttempt(a, errors.New("HTTP 401: rejected key ksk_live_supersecret via Authorization: Bearer abc.def.ghi"))
	entry := rec.finish(outcomeError, 401)

	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "ksk_live_supersecret") {
		t.Fatalf("trace record leaked a Kiro API key: %s", raw)
	}
	if strings.Contains(string(raw), "abc.def.ghi") {
		t.Fatalf("trace record leaked a bearer token: %s", raw)
	}
}
