package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"kiro-go/config"
)

// Every other admin surface in this repo bounds its request body before
// decoding: handler.go does it at 7 sites (16 KiB - 1 MiB), and both
// kiro_apikey_admin.go and kiro_profiles_admin.go wrap the decoder in
// http.MaxBytesReader. admin_bot_api.go bounded NONE of its 7 decode sites.
//
// Why that matters here specifically: these are the mutating endpoints — mint an
// API key, delete a key, add an account. json.Decoder streams, so a single
// unbounded request can make the process buffer an arbitrary amount before the
// body is rejected as invalid. An admin credential is required to reach them,
// but "authenticated" is not "trusted to send a well-formed 500 MB body", and
// the rest of the codebase already decided this by bounding everywhere else.
//
// This pins the convention rather than a specific limit: the assertion is that
// an over-limit body is REFUSED, not that it is refused at exactly N bytes.
func adminBotBoundHandler(t *testing.T) *Handler {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdateSettingsPatch(nil, nil, "topsecret"); err != nil {
		t.Fatalf("set admin password: %v", err)
	}
	return &Handler{}
}

// adminBotOversizedBody builds a syntactically VALID JSON object that is far
// larger than any legitimate admin request. Valid on purpose: if it were
// malformed, a 400 would prove nothing about the size bound.
func adminBotOversizedBody(sizeBytes int) string {
	padding := strings.Repeat("A", sizeBytes)
	return fmt.Sprintf(`{"name":%q,"credits":100}`, padding)
}

func TestAdminBotApiBoundsRequestBody(t *testing.T) {
	h := adminBotBoundHandler(t)

	// 4 MiB of padding: far above any real admin payload, and above every
	// bound used elsewhere in the repo (max 1 MiB).
	body := adminBotOversizedBody(4 << 20)

	cases := []struct {
		name    string
		path    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"new_api_key", "/admin/new_api_key", h.handleAdminNewApiKey},
		{"delete_api_key", "/admin/delete_api_key", h.handleAdminDeleteApiKey},
		{"recharge_api_key", "/admin/recharge_api_key", h.handleAdminRechargeApiKey},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(body))
			req.Header.Set("X-Admin-Password", "topsecret")
			rec := httptest.NewRecorder()
			c.handler(rec, req)

			// An oversized body must be refused. Any 4xx is acceptable: the
			// point is that the request does not succeed and the process is
			// not obliged to buffer it all first.
			if rec.Code < 400 || rec.Code >= 500 {
				t.Errorf("%s accepted a %d-byte body with status %d; an oversized request must be refused",
					c.path, len(body), rec.Code)
			}
		})
	}
}

// The bound must not break legitimate requests. A normal-sized, well-formed
// mint request has to still succeed, or the "fix" is just an outage.
func TestAdminBotApiAcceptsNormalBody(t *testing.T) {
	h := adminBotBoundHandler(t)

	req := httptest.NewRequest(http.MethodPost, "/admin/new_api_key",
		strings.NewReader(`{"name":"buyer-42","credits":500,"tokenLimit":0}`))
	req.Header.Set("X-Admin-Password", "topsecret")
	rec := httptest.NewRecorder()
	h.handleAdminNewApiKey(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("a normal mint request was rejected: status %d body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"key"`) {
		t.Fatalf("mint response carried no key: %s", rec.Body.String())
	}
}
