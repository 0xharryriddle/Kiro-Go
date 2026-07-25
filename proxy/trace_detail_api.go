package proxy

import (
	"encoding/json"
	"net/http"
	"strings"

	"kiro-go/config"
)

// traceDetailResponse is the payload for GET /admin/api/logs/{traceID}.
//
// Bodies are returned as decoded strings rather than nested JSON so the admin UI
// can render them verbatim (including deliberately non-JSON upstream error
// payloads) without a second parse step.
type traceDetailResponse struct {
	Success       bool       `json:"success"`
	Record        RequestLog `json:"record"`
	CaptureMode   string     `json:"captureMode"`
	BodiesStored  bool       `json:"bodiesStored"`
	BodyTruncated bool       `json:"bodyTruncated,omitempty"`
	Request       string     `json:"request,omitempty"`
	Response      string     `json:"response,omitempty"`
	// Note explains an absent body so the UI never presents "no bodies" as a
	// malfunction when capture is simply disabled.
	Note string `json:"note,omitempty"`
}

// traceIDFromLogsPath extracts the trace ID from an admin path of the form
// "/logs/{traceID}". It returns "" when the path is not a detail path.
func traceIDFromLogsPath(path string) string {
	const prefix = "/logs/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	return strings.Trim(strings.TrimPrefix(path, prefix), "/")
}

// findRequestLogByTraceID locates a record in the live ring by its trace ID.
func (h *Handler) findRequestLogByTraceID(traceID string) (RequestLog, bool) {
	if h == nil || strings.TrimSpace(traceID) == "" {
		return RequestLog{}, false
	}
	h.requestLogsMu.RLock()
	defer h.requestLogsMu.RUnlock()
	// Newest first: a replayed trace ID should resolve to the latest record.
	for i := len(h.requestLogs) - 1; i >= 0; i-- {
		if h.requestLogs[i].RequestID == traceID {
			return h.requestLogs[i], true
		}
	}
	return RequestLog{}, false
}

// apiGetTraceDetail serves GET /admin/api/logs/{traceID}: the full trace record
// plus captured bodies when body capture was enabled for that request.
//
// no-store is set because the response can carry prompt text; letting an
// intermediary or the browser cache it would widen the blast radius of the one
// endpoint in this service that intentionally returns request payloads.
func (h *Handler) apiGetTraceDetail(w http.ResponseWriter, r *http.Request, traceID string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")

	traceID = strings.TrimSpace(traceID)
	if traceID == "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "trace id is required",
		})
		return
	}

	record, ok := h.findRequestLogByTraceID(traceID)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "trace not found",
		})
		return
	}

	resp := traceDetailResponse{
		Success:     true,
		Record:      record,
		CaptureMode: config.GetTraceCaptureMode(),
	}

	if strings.TrimSpace(record.BodyRef) == "" {
		resp.Note = "bodies not stored for this request (capture mode: " + resp.CaptureMode + ")"
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	body, err := h.readTraceBody(record.BodyRef)
	if err != nil || body == nil {
		// The record claims a body but it is gone (pruned by retention, or the
		// ref is unreadable). Report the record rather than failing the whole
		// request: the metadata is still the useful part.
		resp.Note = "captured bodies are no longer available (pruned or unreadable)"
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	resp.BodiesStored = true
	resp.BodyTruncated = body.Truncated || record.BodyTruncated
	resp.Request = string(body.Request)
	resp.Response = string(body.Response)
	_ = json.NewEncoder(w).Encode(resp)
}

// readTraceBody loads a captured body via the handler's body store.
func (h *Handler) readTraceBody(ref string) (*traceBody, error) {
	if h == nil || h.traceBodies == nil {
		return nil, nil
	}
	return h.traceBodies.Read(ref)
}
