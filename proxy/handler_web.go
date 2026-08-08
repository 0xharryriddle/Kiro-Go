package proxy

import (
	"net/http"
	"strings"
)

// ==================== 静态文件服务 ====================

// setWebSecurityHeaders adds anti-clickjacking and defense-in-depth headers to the
// HTML admin panel and self-service portal (M6).
//
//   - X-Frame-Options: DENY and CSP frame-ancestors 'none' stop the pages from being
//     embedded in an iframe, blocking clickjacking of a logged-in admin.
//   - X-Content-Type-Options: nosniff prevents MIME-sniffing of served assets.
//   - Referrer-Policy: no-referrer keeps admin URLs/keys out of Referer headers.
//
// The CSP intentionally does NOT lock down script-src/style-src: the panel uses inline
// <script> blocks, ~80 inline event handlers, and a runtime Tailwind build, so a strict
// policy would break it. It restricts the safe-to-restrict directives (framing, objects,
// base URI). A full script/style lockdown would require reworking the frontend to use
// nonces and external handlers.
func setWebSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'; object-src 'none'; base-uri 'none'")
}
func (h *Handler) serveAdminPage(w http.ResponseWriter, r *http.Request) {
	setWebSecurityHeaders(w)
	http.ServeFile(w, r, "web/index.html")
}
func (h *Handler) serveStaticFile(w http.ResponseWriter, r *http.Request) {
	setWebSecurityHeaders(w)
	path := strings.TrimPrefix(r.URL.Path, "/admin/")
	http.ServeFile(w, r, "web/"+path)
}
