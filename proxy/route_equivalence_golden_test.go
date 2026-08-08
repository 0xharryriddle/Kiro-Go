package proxy

// routeGolden is the recorded dispatch fingerprint for every probe in
// routeProbes(), captured from the live ServeHTTP switch (handler.go:682) BEFORE
// any table-driven routing conversion.
//
// Format: "<status> | <content-type> | <normalized body prefix>", with digit runs
// collapsed to N so uptimes/timestamps do not float between runs.
//
// TO REGENERATE (only when a route is INTENTIONALLY added or changed):
//
// \tKIROGO_ROUTE_CAPTURE=1 go test ./proxy/ -run TestRouteDispatchMatchesGolden -v
//
// then paste the captured block below.
//
// Regenerating this table to make a failing test pass defeats the entire purpose
// of the file. A diff here during a refactor means the refactor changed observable
// behaviour, and the fix belongs in the refactor. Because that temptation is real,
// the invariants that actually matter are ALSO asserted relationally in
// TestRoutePrecedenceHazardsStayDistinct and TestRouteAliasesStayUnified, which a
// regeneration cannot paper over.
//
// The SENTINEL-* bodies come from the throwaway web/ assets created by
// sentinelWebRoot; they are what makes the four file-serving arms distinguishable
// from each other instead of collapsing into one shared 404.
var routeGolden = map[string]string{
	"GET /healthz":                       "200 | text/plain; charset=utf-8 | {\"status\":\"ok\",\"time\":N}\\n",
	"GET /readyz":                        "200 |  | {\"checks\":{\"accounts\":N,\"config\":true,\"configWritable\":true,\"importsWrit",
	"GET /metrics":                       "404 | text/plain; charset=utf-8 | N page not found\\n",
	"POST /v1/messages":                  "401 | application/json; charset=utf-8 | {\"error\":{\"message\":\"API key authentication is required but no keys are ",
	"POST /messages":                     "401 | application/json; charset=utf-8 | {\"error\":{\"message\":\"API key authentication is required but no keys are ",
	"POST /anthropic/v1/messages":        "401 | application/json; charset=utf-8 | {\"error\":{\"message\":\"API key authentication is required but no keys are ",
	"POST /v1/messages/count_tokens":     "401 | application/json; charset=utf-8 | {\"error\":{\"message\":\"API key authentication is required but no keys are ",
	"POST /messages/count_tokens":        "401 | application/json; charset=utf-8 | {\"error\":{\"message\":\"API key authentication is required but no keys are ",
	"POST /v1/chat/completions":          "401 | application/json; charset=utf-8 | {\"error\":{\"message\":\"API key authentication is required but no keys are ",
	"POST /chat/completions":             "401 | application/json; charset=utf-8 | {\"error\":{\"message\":\"API key authentication is required but no keys are ",
	"POST /v1/responses":                 "401 | application/json; charset=utf-8 | {\"error\":{\"message\":\"API key authentication is required but no keys are ",
	"POST /responses":                    "401 | application/json; charset=utf-8 | {\"error\":{\"message\":\"API key authentication is required but no keys are ",
	"GET /v1/models":                     "200 | application/json; charset=utf-8 | {\"data\":[{\"capabilities\":{\"image\":true,\"image_vision\":true,\"vision\":true",
	"GET /models":                        "200 | application/json; charset=utf-8 | {\"data\":[{\"capabilities\":{\"image\":true,\"image_vision\":true,\"vision\":true",
	"GET /v1/key/info":                   "401 | application/json; charset=utf-8 | {\"error\":\"Invalid or missing API key\"}\\n",
	"GET /key/info":                      "401 | application/json; charset=utf-8 | {\"error\":\"Invalid or missing API key\"}\\n",
	"GET /v1/key/logs":                   "401 | application/json; charset=utf-8 | {\"error\":\"Invalid or missing API key\"}\\n",
	"GET /key/logs":                      "401 | application/json; charset=utf-8 | {\"error\":\"Invalid or missing API key\"}\\n",
	"POST /api/event_logging/batch":      "200 | application/json; charset=utf-8 | {\"status\":\"ok\"}",
	"GET /api/stats":                     "401 | application/json; charset=utf-8 | {\"error\":\"Missing API key\"}\\n",
	"POST /api/stats":                    "401 | application/json; charset=utf-8 | {\"error\":\"Missing API key\"}\\n",
	"GET /api/me":                        "401 | application/json; charset=utf-8 | {\"error\":\"Missing API key\"}\\n",
	"POST /api/me":                       "401 | application/json; charset=utf-8 | {\"error\":\"Missing API key\"}\\n",
	"GET /api/logs":                      "401 | application/json; charset=utf-8 | {\"error\":\"Missing API key\"}\\n",
	"POST /api/logs":                     "401 | application/json; charset=utf-8 | {\"error\":\"Missing API key\"}\\n",
	"DELETE /api/stats":                  "404 | text/plain; charset=utf-8 | Not Found\\n",
	"DELETE /api/me":                     "404 | text/plain; charset=utf-8 | Not Found\\n",
	"DELETE /api/logs":                   "404 | text/plain; charset=utf-8 | Not Found\\n",
	"POST /admin/new_api_key":            "401 | application/json; charset=utf-8 | {\"error\":\"Unauthorized\"}\\n",
	"POST /admin/delete_api_key":         "401 | application/json; charset=utf-8 | {\"error\":\"Unauthorized\"}\\n",
	"POST /admin/recharge_api_key":       "401 | application/json; charset=utf-8 | {\"error\":\"Unauthorized\"}\\n",
	"POST /admin/stats":                  "401 | application/json; charset=utf-8 | {\"error\":\"Unauthorized\"}\\n",
	"GET /admin/pool":                    "401 | application/json; charset=utf-8 | {\"error\":\"Unauthorized\"}\\n",
	"POST /admin/add_kiro_api_key":       "401 | application/json; charset=utf-8 | {\"error\":\"Unauthorized\"}\\n",
	"POST /admin/add_kiro_account":       "401 | application/json; charset=utf-8 | {\"error\":\"Unauthorized\"}\\n",
	"POST /admin/add_custom_api_account": "401 | application/json; charset=utf-8 | {\"error\":\"Unauthorized\"}\\n",
	"POST /admin/add_bedrock_account":    "401 | application/json; charset=utf-8 | {\"error\":\"Unauthorized\"}\\n",
	"GET /admin/new_api_key":             "404 | text/plain; charset=utf-8 | N page not found\\n",
	"GET /admin/api/login":               "405 | application/json; charset=utf-8 | {\"error\":\"Method Not Allowed\"}\\n",
	"POST /admin/api/login":              "401 | application/json; charset=utf-8 | {\"error\":\"Unauthorized\"}\\n",
	"GET /admin/api/logout":              "200 | application/json; charset=utf-8 | {\"success\":true}\\n",
	"POST /admin/api/logout":             "200 | application/json; charset=utf-8 | {\"success\":true}\\n",
	"GET /admin/api/accounts":            "401 |  | {\"error\":\"Unauthorized\"}\\n",
	"GET /admin/api/nonexistent-probe":   "401 |  | {\"error\":\"Unauthorized\"}\\n",
	"GET /admin":                         "200 | text/html; charset=utf-8 | <!doctype html><title>SENTINEL-ADMIN-INDEX</title>",
	"GET /admin/":                        "200 | text/html; charset=utf-8 | <!doctype html><title>SENTINEL-ADMIN-INDEX</title>",
	"GET /admin/app.js":                  "200 | text/javascript; charset=utf-8 | /* SENTINEL-ADMIN-STATIC-APPJS */",
	"GET /check":                         "200 | text/html; charset=utf-8 | <!doctype html><title>SENTINEL-CHECK-PORTAL</title>",
	"GET /check/":                        "200 | text/html; charset=utf-8 | <!doctype html><title>SENTINEL-CHECK-PORTAL</title>",
	"GET /usage":                         "200 | text/html; charset=utf-8 | <!doctype html><title>SENTINEL-USAGE-PAGE</title>",
	"GET /usage/":                        "200 | text/html; charset=utf-8 | <!doctype html><title>SENTINEL-USAGE-PAGE</title>",
	"POST /usage":                        "401 | application/json; charset=utf-8 | {\"error\":\"Missing API key\"}\\n",
	"POST /usage/":                       "401 | application/json; charset=utf-8 | {\"error\":\"Missing API key\"}\\n",
	"POST /v1/usage":                     "401 | application/json; charset=utf-8 | {\"error\":\"Missing API key\"}\\n",
	"GET /v1/usage":                      "404 | text/plain; charset=utf-8 | Not Found\\n",
	"DELETE /usage":                      "404 | text/plain; charset=utf-8 | Not Found\\n",
	"GET /health":                        "200 | application/json; charset=utf-8 | {\"status\":\"ok\",\"uptime\":N,\"version\":\"N.N.N\"}\\n",
	"GET /":                              "200 | application/json; charset=utf-8 | {\"status\":\"ok\",\"uptime\":N,\"version\":\"N.N.N\"}\\n",
	"GET /v1/stats":                      "401 | application/json; charset=utf-8 | {\"error\":\"Invalid or missing API key\"}\\n",
	"OPTIONS /v1/messages":               "204 |  | ",
	"OPTIONS /admin/api/accounts":        "204 |  | ",
	"GET /definitely-not-a-route":        "404 | text/plain; charset=utf-8 | Not Found\\n",
	"GET /v1/unknown":                    "404 | text/plain; charset=utf-8 | Not Found\\n",
	"GET /adminx":                        "404 | text/plain; charset=utf-8 | Not Found\\n",
}
