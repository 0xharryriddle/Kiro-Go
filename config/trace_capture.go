package config

import (
	"os"
	"strconv"
	"strings"
)

// Trace capture modes. See Config.TraceCaptureMode for semantics.
const (
	// TraceCaptureOff disables trace recording entirely.
	TraceCaptureOff = "off"
	// TraceCaptureMeta records metadata and per-attempt routing detail only.
	// This is the default: no prompt or response text ever reaches disk.
	TraceCaptureMeta = "meta"
	// TraceCaptureRedacted additionally stores bodies with PII redaction.
	TraceCaptureRedacted = "redacted"
	// TraceCaptureFull stores bodies verbatim. Requires an explicit risk
	// acknowledgement; without it, it degrades to TraceCaptureRedacted.
	TraceCaptureFull = "full"
)

const (
	// traceDefaultRetentionHours is 7 days.
	traceDefaultRetentionHours = 168
	// traceDefaultMaxBodyBytes is 256KB per captured body.
	traceDefaultMaxBodyBytes = 262144
)

// NormalizeTraceCaptureMode maps arbitrary input to a known mode.
//
// Unrecognised, empty, or malformed values resolve to TraceCaptureMeta rather
// than to a body-capturing mode: a typo must never silently start retaining
// prompts. "full" is honoured only when acknowledgeRisk is true; otherwise it
// degrades to "redacted", so verbatim prompt retention is always a deliberate
// two-step decision.
func NormalizeTraceCaptureMode(raw string, acknowledgeRisk bool) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case TraceCaptureOff:
		return TraceCaptureOff
	case TraceCaptureRedacted:
		return TraceCaptureRedacted
	case TraceCaptureFull:
		if acknowledgeRisk {
			return TraceCaptureFull
		}
		return TraceCaptureRedacted
	case TraceCaptureMeta:
		return TraceCaptureMeta
	default:
		return TraceCaptureMeta
	}
}

// GetTraceCaptureMode returns the effective capture mode.
//
// TRACE_CAPTURE_MODE overrides the stored config so an operator can dial
// capture down (or up, with TRACE_CAPTURE_ACK_RISK=true) without editing
// config.json — useful for a short debugging window.
func GetTraceCaptureMode() string {
	cfgLock.RLock()
	raw := ""
	ack := false
	if cfg != nil {
		raw = cfg.TraceCaptureMode
		ack = cfg.TraceCaptureAcknowledgeRisk
	}
	cfgLock.RUnlock()

	if env := strings.TrimSpace(os.Getenv("TRACE_CAPTURE_MODE")); env != "" {
		raw = env
	}
	if env := strings.TrimSpace(os.Getenv("TRACE_CAPTURE_ACK_RISK")); env != "" {
		if parsed, err := strconv.ParseBool(env); err == nil {
			ack = parsed
		}
	}
	return NormalizeTraceCaptureMode(raw, ack)
}

// TraceCaptureBodies reports whether the effective mode stores request and
// response bodies.
func TraceCaptureBodies() bool {
	mode := GetTraceCaptureMode()
	return mode == TraceCaptureRedacted || mode == TraceCaptureFull
}

// TraceCaptureEnabled reports whether trace records should be written at all.
func TraceCaptureEnabled() bool {
	return GetTraceCaptureMode() != TraceCaptureOff
}

// GetTraceCaptureAcknowledgeRisk reports whether verbatim body capture has been
// explicitly acknowledged. Exposed so the admin UI can show the operator why a
// "full" selection is currently behaving as "redacted".
func GetTraceCaptureAcknowledgeRisk() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.TraceCaptureAcknowledgeRisk
}

// GetTraceRetentionHours returns the trace retention window in hours,
// defaulting to 7 days.
func GetTraceRetentionHours() int {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.TraceRetentionHours <= 0 {
		return traceDefaultRetentionHours
	}
	return cfg.TraceRetentionHours
}

// GetTraceMaxBodyBytes returns the per-body capture cap, defaulting to 256KB.
func GetTraceMaxBodyBytes() int {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.TraceMaxBodyBytes <= 0 {
		return traceDefaultMaxBodyBytes
	}
	return cfg.TraceMaxBodyBytes
}

// UpdateTraceCaptureConfig persists the trace capture settings.
//
// The mode is normalised before being stored, so an unacknowledged "full"
// is written as "redacted" rather than being kept as a latent setting that
// would start capturing verbatim prompts the moment the acknowledgement flag
// was flipped somewhere else.
func UpdateTraceCaptureConfig(mode string, acknowledgeRisk bool, retentionHours, maxBodyBytes int) error {
	normalized := NormalizeTraceCaptureMode(mode, acknowledgeRisk)

	cfgLock.Lock()
	prevMode := cfg.TraceCaptureMode
	prevAck := cfg.TraceCaptureAcknowledgeRisk
	prevRetention := cfg.TraceRetentionHours
	prevMaxBody := cfg.TraceMaxBodyBytes

	cfg.TraceCaptureMode = normalized
	cfg.TraceCaptureAcknowledgeRisk = acknowledgeRisk
	if retentionHours > 0 {
		cfg.TraceRetentionHours = retentionHours
	}
	if maxBodyBytes > 0 {
		cfg.TraceMaxBodyBytes = maxBodyBytes
	}
	err := Save()
	if err != nil {
		// Roll back so in-memory state never diverges from what is durable.
		cfg.TraceCaptureMode = prevMode
		cfg.TraceCaptureAcknowledgeRisk = prevAck
		cfg.TraceRetentionHours = prevRetention
		cfg.TraceMaxBodyBytes = prevMaxBody
	}
	cfgLock.Unlock()
	return err
}
