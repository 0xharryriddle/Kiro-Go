package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The /v1/responses store has NO owner field, and handleOpenAIResponses loads a
// previous_response_id with loadResponse(id) — a bare filename lookup with no
// tenant scoping (proxy/responses_handler.go:46, proxy/responses_store.go:84).
//
// The sibling response cache treats this exact property as the most serious
// defect it could have: responseCacheKey prefixes the apiKeyID specifically so
// "one tenant's cached responses [can never] be served to another key", and
// TestResponseCacheStoreKeepsTenantsSeparate pins it end to end. The persistent
// responses store has no equivalent — storedResponseDoc carries id/model/output/
// stored_input and no key identity at all.
//
// Consequence: any customer key that learns or guesses another customer's
// response id can replay it as previous_response_id. The victim's stored INPUT
// and the assistant's OUTPUT are then expanded by
// expandPreviousResponseHistory and forwarded upstream as the attacker's
// conversation history — and come back to the attacker as context in the
// continuation. That is a cross-tenant disclosure of prompt content, on a
// multi-tenant proxy whose whole billing model is per-customer keys.
//
// This test pins the property at the HANDLER, not the store, because the
// handler is where the authenticated identity actually exists.
func TestResponsesPreviousResponseIDIsTenantScoped(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	// Tenant A's completed turn, stored through the HANDLER as tenant A so it
	// carries whatever ownership the implementation records. Hand-writing the
	// record instead would store it with no owner, and an unowned record is
	// deliberately still readable (pre-ownership records, and the key-less
	// requireApiKey=false mode this proxy runs in) — so a hand-written victim
	// would make this test pass without proving tenant scoping at all.
	seedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "MY-PRIVATE-ANSWER-board-approved-the-deal",
		}))
	}))
	seedRestore := swapKiroEndpointsForTest(t, seedServer)
	seedReq := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"claude-sonnet-4.5","input":"MY-PRIVATE-PROMPT-acquisition-price-is-42M"}`))
	seedReq = seedReq.WithContext(context.WithValue(context.Background(),
		apiKeyContextKey{}, "tenant-A-victim"))
	seedRec := httptest.NewRecorder()
	h.handleOpenAIResponses(seedRec, seedReq)
	seedRestore()
	seedServer.Close()

	if seedRec.Code != http.StatusOK {
		t.Fatalf("seeding tenant A's response failed: %d %s", seedRec.Code, seedRec.Body.String())
	}
	var victimStored struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(seedRec.Body.Bytes(), &victimStored); err != nil {
		t.Fatalf("decode seeded victim response: %v", err)
	}
	if victimStored.ID == "" {
		t.Fatalf("seeded victim response carried no id: %s", seedRec.Body.String())
	}

	var upstreamPayload string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamPayload = string(body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "continuation",
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	// Tenant B — a DIFFERENT authenticated customer key — replays A's id.
	body := strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"input":"continue our earlier conversation",
		"previous_response_id":"` + victimStored.ID + `",
		"store":false
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	req = req.WithContext(context.WithValue(context.Background(),
		apiKeyContextKey{}, "tenant-B-attacker"))
	rec := httptest.NewRecorder()
	h.handleOpenAIResponses(rec, req)

	// The attacker must not be able to continue a response it does not own.
	// Either a 404 (same shape as a missing id — no existence oracle) or, at
	// minimum, none of the victim's content reaching upstream.
	if strings.Contains(upstreamPayload, "MY-PRIVATE-PROMPT") {
		t.Fatalf("cross-tenant leak: tenant A's stored INPUT was forwarded upstream "+
			"as tenant B's conversation history. previous_response_id is not "+
			"scoped to the owning api key. payload=%s", upstreamPayload)
	}
	if strings.Contains(upstreamPayload, "MY-PRIVATE-ANSWER") {
		t.Fatalf("cross-tenant leak: tenant A's assistant OUTPUT was forwarded "+
			"upstream as tenant B's conversation history. payload=%s", upstreamPayload)
	}
	if rec.Code == http.StatusOK {
		t.Fatalf("tenant B successfully continued tenant A's response (HTTP 200); "+
			"expected 404. body=%s", rec.Body.String())
	}
}

// The ancestor WALK is ownership-checked too, not just the entry point.
//
// expandPreviousResponseHistory follows previous_response_id links recorded at
// creation time. Checking only the caller's immediate previous_response_id would
// leave the deeper hops unchecked, so this pins the property at the hop that a
// handler-level test cannot reach: a chain whose mid-point belongs to another
// tenant must stop there rather than expanding the foreign record.
//
// Honest scope: with the handler check in place a mixed-owner chain should not
// be constructible through the API, so this guards a defense-in-depth layer
// rather than a proven-exploitable path. It is pinned because the induction
// argument that makes it safe lives in a different file.
func TestResponsesAncestorWalkStopsAtForeignOwner(t *testing.T) {
	h, _ := setupResponsesTestHandler(t)
	_ = h

	// root belongs to tenant A and holds the secret.
	root := &ResponsesObject{
		ID:            "resp_chain_root_tenantA",
		Object:        "response",
		Status:        "completed",
		Model:         "claude-sonnet-4.5",
		OwnerApiKeyID: "tenant-A",
		StoredInput:   json.RawMessage(`"ROOT-SECRET-owned-by-A"`),
		StoredAt:      1,
		Output: []ResponseOutputItem{{
			Type: "message", Role: "assistant",
			Content: []ResponseContentPart{{Type: "output_text", Text: "ROOT-ANSWER-owned-by-A"}},
		}},
	}
	// leaf belongs to tenant B but points at A's root.
	leaf := &ResponsesObject{
		ID:                 "resp_chain_leaf_tenantB",
		Object:             "response",
		Status:             "completed",
		Model:              "claude-sonnet-4.5",
		OwnerApiKeyID:      "tenant-B",
		PreviousResponseID: root.ID,
		StoredInput:        json.RawMessage(`"leaf input owned by B"`),
		StoredAt:           2,
		Output: []ResponseOutputItem{{
			Type: "message", Role: "assistant",
			Content: []ResponseContentPart{{Type: "output_text", Text: "leaf answer owned by B"}},
		}},
	}
	if err := saveResponse(root); err != nil {
		t.Fatalf("save root: %v", err)
	}
	if err := saveResponse(leaf); err != nil {
		t.Fatalf("save leaf: %v", err)
	}

	expanded := expandPreviousResponseHistory(leaf, "tenant-B")

	var all string
	for _, m := range expanded {
		if s, ok := m.Content.(string); ok {
			all += s + "\n"
		}
	}
	if strings.Contains(all, "ROOT-SECRET-owned-by-A") || strings.Contains(all, "ROOT-ANSWER-owned-by-A") {
		t.Fatalf("ancestor walk expanded a record owned by another tenant: %s", all)
	}
	// Tenant B's own leaf content must survive — stopping the walk must not
	// discard the caller's own turn.
	if !strings.Contains(all, "leaf input owned by B") {
		t.Fatalf("ancestor walk dropped the caller's OWN content: %s", all)
	}
}

// Positive control: the owning tenant MUST still be able to continue its own
// response. Without this, "reject every previous_response_id" would look like a
// valid fix while destroying multi-turn /v1/responses entirely.
func TestResponsesOwnerCanStillContinueOwnResponse(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	const owner = "tenant-A-owner"

	// Store the ancestor through the HANDLER so it is stamped with whatever
	// ownership the implementation records — the test must not hand-write the
	// owner field, or it would pass against a store that never sets one.
	seedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "first reply",
		}))
	}))
	restore := swapKiroEndpointsForTest(t, seedServer)

	seedReq := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"claude-sonnet-4.5","input":"first turn"}`))
	seedReq = seedReq.WithContext(context.WithValue(context.Background(),
		apiKeyContextKey{}, owner))
	seedRec := httptest.NewRecorder()
	h.handleOpenAIResponses(seedRec, seedReq)
	restore()
	seedServer.Close()

	if seedRec.Code != http.StatusOK {
		t.Fatalf("seed turn failed: %d %s", seedRec.Code, seedRec.Body.String())
	}
	var seeded struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(seedRec.Body.Bytes(), &seeded); err != nil {
		t.Fatalf("decode seed response: %v", err)
	}
	if seeded.ID == "" {
		t.Fatalf("seed response carried no id: %s", seedRec.Body.String())
	}

	var upstreamPayload string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamPayload = string(body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "second reply",
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"input":"second turn",
		"previous_response_id":"` + seeded.ID + `",
		"store":false
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	req = req.WithContext(context.WithValue(context.Background(),
		apiKeyContextKey{}, owner))
	rec := httptest.NewRecorder()
	h.handleOpenAIResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("owner could not continue its OWN response: %d %s",
			rec.Code, rec.Body.String())
	}
	if !strings.Contains(upstreamPayload, "first turn") {
		t.Fatalf("owner's continuation lost its own history; multi-turn is broken. "+
			"payload=%s", upstreamPayload)
	}
}
