package proxy

// Notification / webhook bus (F7): fan out selected security/warning audit events
// to a configured webhook URL (Slack/Discord-compatible JSON body).
//
// SECURITY: this is the ONLY feature that makes outbound requests to a
// third-party endpoint. It is opt-in (empty WebhookURL disables it, the default)
// and the payload carries ONLY safe fields already present in the audit log —
// category, action, status, account label, reason, and safe details. It never
// includes refresh/access tokens, client secrets, or raw prompt content,
// mirroring the audit-log redaction discipline.

import (
	"bytes"
	"encoding/json"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"strings"
	"time"
)

// webhookDispatchTimeout bounds each outbound webhook POST so a slow/hung
// endpoint can never stall the audit path (which runs the dispatch in a goroutine).
const webhookDispatchTimeout = 8 * time.Second

// webhookEventCategories are the audit categories worth pushing to an operator.
// Routine informational events (imports, previews) are intentionally excluded to
// avoid noise — only security/degradation signals fan out.
func isWebhookableEvent(entry AuditLog) bool {
	if entry.Category == "security" {
		return true
	}
	// Diagnostics failures are worth surfacing; successes are not.
	if entry.Category == "diagnostics" && entry.Status == "error" {
		return true
	}
	return false
}

// dispatchWebhook POSTs a safe JSON summary of an audit event to the configured
// webhook URL. Fire-and-forget: intended to run in a goroutine. No-op when no URL
// is configured or the event is not webhookable.
func (h *Handler) dispatchWebhook(entry AuditLog) {
	url := strings.TrimSpace(config.GetWebhookURL())
	if url == "" || !isWebhookableEvent(entry) {
		return
	}

	// Build a compact, human-readable summary line. account label uses email when
	// present, else the ID — both already appear in the (safe) audit log.
	account := strings.TrimSpace(entry.AccountEmail)
	if account == "" {
		account = entry.AccountID
	}
	summary := "[kiro-go] " + entry.Category + "/" + entry.Action + " (" + entry.Status + ")"
	if account != "" {
		summary += " account=" + account
	}
	if entry.Reason != "" {
		summary += " reason=" + entry.Reason
	}

	// Payload includes BOTH a Slack/Discord-style "text"/"content" field (so the
	// common chat webhooks render it directly) and a structured "event" object of
	// safe fields for generic consumers.
	payload := map[string]interface{}{
		"text":    summary, // Slack
		"content": summary, // Discord
		"event": map[string]interface{}{
			"time":         entry.Time,
			"category":     entry.Category,
			"action":       entry.Action,
			"status":       entry.Status,
			"accountId":    entry.AccountID,
			"accountEmail": entry.AccountEmail,
			"authMethod":   entry.AuthMethod,
			"provider":     entry.Provider,
			"source":       entry.Source,
			"reason":       entry.Reason,
			"safeDetails":  entry.SafeDetails,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		logger.Warnf("[Webhook] encode failed: %v", err)
		return
	}

	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		logger.Warnf("[Webhook] request build failed: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: webhookDispatchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		logger.Warnf("[Webhook] delivery failed: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		logger.Warnf("[Webhook] endpoint returned HTTP %d", resp.StatusCode)
	}
}
