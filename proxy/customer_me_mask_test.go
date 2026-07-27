package proxy

import (
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCustomerMeReportsMaskedKey pins that GET /api/me returns a NON-EMPTY
// masked key identifying which credential the caller authenticated with.
//
// Why this needs its own test: the pre-existing coverage
// (TestCustomerMeReturnsQuota) only asserts that the CLEARTEXT does not appear
// in the response. An empty "keyMasked" satisfies that assertion perfectly, so
// the leak test passes whether the mask works or not — it cannot distinguish
// "masked correctly" from "field is blank".
//
// The real contract comes from the handler's own documentation: "Returns the
// masked key so customers can confirm which key they queried with". A blank
// value silently fails that contract, and the customer-facing symptom is a UI
// that cannot tell two keys apart.
//
// Keys are hashed at rest — config.AddApiKey clears the plaintext Key field
// before persisting (config/apikeys.go) and stores the display form in KeyMask.
// So any masking that derives from entry.Key is computing MaskApiKey("") == "".
// config.ApiKeyDisplayMask is the accessor that reads the stored KeyMask, and
// it is what the admin view (proxy/admin_apikeys.go) already uses.
func TestCustomerMeReportsMaskedKey(t *testing.T) {
	mustInitConfig(t)
	entry := seedKey(t, "buyer-mask", 1000, 0)
	h := &Handler{}

	r := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	r.Header.Set("X-Api-Key", entry.Key)
	rec := serve(h, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)

	masked, ok := body["keyMasked"].(string)
	if !ok {
		t.Fatalf("keyMasked missing or not a string: %#v", body["keyMasked"])
	}
	if masked == "" {
		t.Fatalf("keyMasked is empty; the endpoint promises a masked key so the "+
			"customer can identify which credential they queried with (response: %s)",
			rec.Body.String())
	}

	// It must be the SAME masked form the rest of the system displays, so a
	// customer comparing /api/me against an operator's admin view sees one
	// identity for one key rather than two different strings.
	stored := config.GetApiKeyEntry(entry.ID)
	if stored == nil {
		t.Fatalf("seeded entry vanished")
	}
	if want := config.ApiKeyDisplayMask(*stored); masked != want {
		t.Fatalf("keyMasked = %q, want %q (must match the stored display mask)", masked, want)
	}

	// Guard the property the original test covered, so this test fully
	// supersedes it rather than trading one gap for another: masking must not
	// regress into echoing the real credential.
	if masked == entry.Key {
		t.Fatalf("keyMasked returned the cleartext credential")
	}
}
