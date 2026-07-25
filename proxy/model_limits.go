package proxy

import (
	"strings"
	"sync"

	"kiro-go/config"
)

// Upstream-declared per-model token limits.
//
// Kiro's ListAvailableModels response carries a tokenLimits object per model
// (maxInputTokens / maxOutputTokens). That is the ONLY authoritative statement
// of a model's window available to this proxy: everything else in the codebase
// infers the window from the model NAME via a version regex
// (isLargeContextModel), which necessarily lags the product. A newly published
// flagship — "claude-opus-5" being the motivating case — is sized by pattern
// guess until someone edits the classifier, and a guess that comes in low makes
// clients compact far earlier than needed while a guess that comes in high
// overflows the request upstream.
//
// The limits were already parsed into ModelInfo.TokenLimits and merged across
// accounts (mergeModelInfo), then discarded. This registry retains them so the
// window functions can prefer a fact over a heuristic, and falls back to the
// heuristic whenever upstream said nothing.
type modelTokenLimits struct {
	MaxInputTokens  int
	MaxOutputTokens int
}

var (
	modelTokenLimitsMu   sync.RWMutex
	modelTokenLimitsByID = map[string]modelTokenLimits{}
)

// recordModelTokenLimits captures the upstream-declared token limits for every
// model that reports them. Called wherever a model list is fetched and cached.
//
// Entries are additive and last-writer-wins per model ID: a model absent from a
// later fetch keeps its previously declared limits rather than reverting to the
// name heuristic, because a single account's partial or failed listing must not
// silently resize a model for the whole fleet. Models with no tokenLimits, or
// with a non-positive input limit, are skipped so they continue to fall through
// to the heuristic instead of recording a bogus zero window.
func recordModelTokenLimits(models []ModelInfo) {
	if len(models) == 0 {
		return
	}
	modelTokenLimitsMu.Lock()
	defer modelTokenLimitsMu.Unlock()
	for _, m := range models {
		if m.TokenLimits == nil {
			continue
		}
		key := modelLimitsKey(m.ModelId)
		if key == "" {
			continue
		}
		if m.TokenLimits.MaxInputTokens <= 0 && m.TokenLimits.MaxOutputTokens <= 0 {
			continue
		}
		prev := modelTokenLimitsByID[key]
		next := modelTokenLimits{
			MaxInputTokens:  prev.MaxInputTokens,
			MaxOutputTokens: prev.MaxOutputTokens,
		}
		if m.TokenLimits.MaxInputTokens > 0 {
			next.MaxInputTokens = m.TokenLimits.MaxInputTokens
		}
		if m.TokenLimits.MaxOutputTokens > 0 {
			next.MaxOutputTokens = m.TokenLimits.MaxOutputTokens
		}
		modelTokenLimitsByID[key] = next
	}
}

// declaredModelInputLimit returns the upstream-declared max input tokens for a
// model, and whether one was declared.
func declaredModelInputLimit(model string) (int, bool) {
	limits, ok := lookupDeclaredModelLimits(model)
	if !ok || limits.MaxInputTokens <= 0 {
		return 0, false
	}
	return limits.MaxInputTokens, true
}

// declaredModelOutputLimit returns the upstream-declared max output tokens for a
// model, and whether one was declared.
func declaredModelOutputLimit(model string) (int, bool) {
	limits, ok := lookupDeclaredModelLimits(model)
	if !ok || limits.MaxOutputTokens <= 0 {
		return 0, false
	}
	return limits.MaxOutputTokens, true
}

// lookupDeclaredModelLimits resolves a client-supplied model name against the
// registry. A client may send the thinking variant ("claude-opus-5-thinking")
// while upstream lists only the base model, so the base name is tried as a
// fallback — the thinking suffix is a proxy-side routing concept and does not
// change the model's window.
func lookupDeclaredModelLimits(model string) (modelTokenLimits, bool) {
	key := modelLimitsKey(model)
	if key == "" {
		return modelTokenLimits{}, false
	}
	modelTokenLimitsMu.RLock()
	defer modelTokenLimitsMu.RUnlock()
	if limits, ok := modelTokenLimitsByID[key]; ok {
		return limits, true
	}
	// Fall back to the base model with the configured thinking suffix removed.
	if base := modelLimitsKey(stripThinkingSuffixForLimits(model)); base != "" && base != key {
		if limits, ok := modelTokenLimitsByID[base]; ok {
			return limits, true
		}
	}
	return modelTokenLimits{}, false
}

// stripThinkingSuffixForLimits removes the thinking suffix from a model name.
//
// It intentionally does NOT go through ParseModelAndThinking: that function also
// applies alias redirection and dash-to-dot version rewriting, which would map a
// name onto a DIFFERENT model and could return limits that belong to another
// model entirely. Only the suffix is stripped here.
func stripThinkingSuffixForLimits(model string) string {
	trimmed := strings.TrimSpace(model)
	// Match the suffix case-insensitively, mirroring ParseModelAndThinking.
	for _, suffix := range thinkingSuffixCandidates() {
		if suffix == "" {
			continue
		}
		if len(trimmed) > len(suffix) && strings.EqualFold(trimmed[len(trimmed)-len(suffix):], suffix) {
			return trimmed[:len(trimmed)-len(suffix)]
		}
	}
	return trimmed
}

// thinkingSuffixCandidates returns the suffixes to try when reducing a model
// name to its base form. The operator-configured suffix is checked first, then
// the built-in default so a non-default configuration still resolves the
// conventional "-thinking" name.
func thinkingSuffixCandidates() []string {
	configured := strings.TrimSpace(config.GetThinkingConfig().Suffix)
	if configured != "" && !strings.EqualFold(configured, "-thinking") {
		return []string{configured, "-thinking"}
	}
	return []string{"-thinking"}
}

// modelLimitsKey normalizes a model ID for registry lookup.
func modelLimitsKey(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}

// resetDeclaredModelLimitsForTest clears the registry. Test-only helper: the
// registry is process-global, so a test that records limits would otherwise leak
// them into unrelated tests' window calculations.
func resetDeclaredModelLimitsForTest() {
	modelTokenLimitsMu.Lock()
	defer modelTokenLimitsMu.Unlock()
	modelTokenLimitsByID = map[string]modelTokenLimits{}
}
