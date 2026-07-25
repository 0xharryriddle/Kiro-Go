package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"kiro-go/config"
)

// The admin gate is a bare equality: `if password != config.GetPassword()`.
// There is no non-empty check on the CONFIGURED side, so when cfg.Password is ""
// an unauthenticated request satisfies "" == "" and every /admin/api/* route
// opens up — including /config/export (raw config.json with refresh tokens and
// ksk_ keys) and /export (full credential export).
//
// The admin UI cannot blank the password (both write paths skip empty values),
// but config.SetPassword is unguarded and a hand-edited or migrated config.json
// with "password": "" loads verbatim — Load() runs several migrations but never
// normalises an empty password. So this is a latent fail-OPEN on operator-supplied
// config.
//
// The API-key path in the same codebase already fails CLOSED in exactly this
// situation ("Auth required but nothing configured → fail closed",
// proxy/auth.go), so this is inconsistent rather than intentional.
func TestAdminAPIFailsClosedWhenConfiguredPasswordIsEmpty(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	// Simulate an operator-supplied / migrated config with a blank password.
	config.SetPassword("")

	h := &Handler{}

	// A request carrying NO credential at all must not be authorised.
	//
	// The probe routes deliberately avoid handlers that dereference h.pool: with
	// the gate open, execution reaches the handler and panics on the nil pool,
	// which would mask the status assertion. A nonexistent route is enough —
	// reaching the switch at all means the gate let us through, and that shows up
	// as 404 instead of 401.
	for _, target := range []string{
		"/admin/api/nonexistent-probe",
		"/admin/api/config/export",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, target, nil)
		h.handleAdminAPI(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: empty configured password allowed an unauthenticated admin request (status %d) — full credential export is exposed",
				target, rec.Code)
		}
	}
}

// An explicitly-sent empty password must also be rejected, not just an absent one.
func TestAdminAPIRejectsExplicitEmptyPasswordHeader(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	config.SetPassword("")

	h := &Handler{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/nonexistent-probe", nil)
	req.Header.Set("X-Admin-Password", "")
	h.handleAdminAPI(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("explicit empty X-Admin-Password accepted (status %d)", rec.Code)
	}
}

// Guard against over-correcting: a correctly configured password must still
// authenticate, otherwise the admin panel would be locked out entirely.
func TestAdminAPIStillAcceptsCorrectPassword(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	config.SetPassword("a-real-admin-secret")

	h := &Handler{pool: nil}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/nonexistent-route", nil)
	req.Header.Set("X-Admin-Password", "a-real-admin-secret")
	h.handleAdminAPI(rec, req)

	// The route does not exist, so a 404 proves we got PAST the password gate.
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("correct password was rejected: admin panel would be locked out")
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 past the gate, got %d", rec.Code)
	}
}

// And a wrong password must still be rejected.
func TestAdminAPIRejectsWrongPassword(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	config.SetPassword("a-real-admin-secret")

	h := &Handler{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/nonexistent-probe", nil)
	req.Header.Set("X-Admin-Password", "guess")
	h.handleAdminAPI(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password accepted (status %d)", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] == "" {
		t.Fatalf("no error payload on rejection: %s", rec.Body.String())
	}
}
