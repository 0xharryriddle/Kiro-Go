package proxy

// Native Amazon Bedrock Runtime provider.
//
// A "bedrock" account (AuthMethod == "bedrock") holds a static IAM access key +
// region and calls the Bedrock Runtime InvokeModel / InvokeModelWithResponseStream
// endpoints directly, SigV4-signed. Because Bedrock speaks the native Anthropic
// Messages wire format for Claude models, the incoming client request needs only
// light rewriting (drop model/stream, pin anthropic_version) and the streaming
// response's inner events are re-emitted to the client unchanged — the same
// transparent-passthrough shape the custom_api forwarder uses, so it slots into the
// dispatch loop the same way.
//
// This file is self-contained: model-id resolution, request building, the two
// invoke paths, and per-request token billing against the customer API key.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"kiro-go/config"
	"kiro-go/logger"
)

// bedrockService is the SigV4 service name for the Bedrock data plane.
const bedrockService = "bedrock"

// bedrockAnthropicVersion is the required version tag in the request body for
// Anthropic models on Bedrock.
const bedrockAnthropicVersion = "bedrock-2023-05-31"

// bedrockHTTPClient is a dedicated client for Bedrock calls. Five-minute timeout
// matches the Kiro path (long streams); per-account outbound proxy is honored via
// GetClientForProxy when the account sets ProxyURL.
func bedrockHTTPClient(account *config.Account) *http.Client {
	if account != nil && strings.TrimSpace(account.ProxyURL) != "" {
		return GetClientForProxy(account.ProxyURL)
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

// bedrockHTTPClientFor is the seam invokeBedrockRegional uses to obtain its HTTP
// client. It defaults to bedrockHTTPClient; tests override it to inject a hermetic
// client (avoiding the process-wide http.ProxyFromEnvironment cache).
var bedrockHTTPClientFor = bedrockHTTPClient

// defaultBedrockModelMap maps common Anthropic model aliases the client might send
// to Bedrock inference-profile IDs. It is a CONVENIENCE default only and is fully
// overridable per-account (Account.BedrockModelMap) or globally (env
// BEDROCK_MODEL_MAP, a JSON object). Model IDs on Bedrock change over time and vary
// by region/account enablement, so operators should confirm these against their own
// enabled models; anything already looking like a Bedrock id is passed through
// untouched by resolveBedrockModelID regardless of this map.
var defaultBedrockModelMap = map[string]string{
	"claude-3-5-sonnet": "us.anthropic.claude-3-5-sonnet-20241022-v2:0",
	"claude-3-5-haiku":  "us.anthropic.claude-3-5-haiku-20241022-v1:0",
	"claude-3-7-sonnet": "us.anthropic.claude-3-7-sonnet-20250219-v1:0",
	"claude-sonnet-4":   "us.anthropic.claude-sonnet-4-20250514-v1:0",
	"claude-sonnet-4-5": "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
	"claude-opus-4":     "us.anthropic.claude-opus-4-20250514-v1:0",
	"claude-opus-4-1":   "us.anthropic.claude-opus-4-1-20250805-v1:0",
	"claude-haiku-4-5":  "us.anthropic.claude-haiku-4-5-20251001-v1:0",
}

// looksLikeBedrockModelID reports whether s is already a Bedrock model or inference
// profile id (so it should be used verbatim). Concrete ids carry a vendor prefix
// segment (optionally region-prefixed) and a version suffix with a colon, e.g.
// "anthropic.claude-...-v2:0", "us.amazon.nova-pro-v1:0",
// "meta.llama3-70b-instruct-v1:0", "us.deepseek.r1-v1:0". Convenience aliases such
// as "claude-3-5-sonnet" have neither a "." nor a ":", so this stays specific
// enough not to swallow them while still matching non-Anthropic ids (needed for
// the Converse path).
func looksLikeBedrockModelID(s string) bool {
	return strings.Contains(s, ":") && strings.Contains(s, ".")
}

// resolveBedrockModelID turns the client-requested model into a Bedrock model id.
// Precedence: per-account map, then env BEDROCK_MODEL_MAP, then pass-through if it
// already looks like a Bedrock id, then the built-in default map. Returns an error
// only when none of these resolve, so the operator gets a clear "configure the map"
// signal instead of a confusing Bedrock 400.
func resolveBedrockModelID(account *config.Account, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", fmt.Errorf("bedrock: empty model")
	}
	if account != nil && account.BedrockModelMap != nil {
		if id, ok := account.BedrockModelMap[requested]; ok && id != "" {
			return id, nil
		}
	}
	if raw := strings.TrimSpace(os.Getenv("BEDROCK_MODEL_MAP")); raw != "" {
		var envMap map[string]string
		if err := json.Unmarshal([]byte(raw), &envMap); err == nil {
			if id, ok := envMap[requested]; ok && id != "" {
				return id, nil
			}
		}
	}
	// Discovered callable models (populated by apiGetAccountModels / background
	// discovery): exact or unique-alias match against what this account can
	// actually invoke. Reads the cache only, never the network.
	if account != nil {
		if id := discoveredBedrockModelFor(account.ID, requested); id != "" {
			return id, nil
		}
	}
	if looksLikeBedrockModelID(requested) {
		return requested, nil
	}
	// Last-resort convenience defaults (may not be callable by this account).
	if id, ok := defaultBedrockModelMap[requested]; ok {
		return id, nil
	}
	return "", fmt.Errorf("bedrock: no model mapping for %q (set account BedrockModelMap or BEDROCK_MODEL_MAP)", requested)
}

// buildBedrockBody rewrites the incoming Anthropic-format request body for Bedrock:
// removes "model" and "stream" (model goes in the URL, stream is chosen by
// endpoint) and pins anthropic_version. All other fields (messages, system,
// max_tokens, temperature, tools, thinking, ...) pass through unchanged.
func buildBedrockBody(rawBody []byte) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &m); err != nil {
		return nil, fmt.Errorf("bedrock: invalid request body: %w", err)
	}
	delete(m, "model")
	delete(m, "stream")
	m["anthropic_version"], _ = json.Marshal(bedrockAnthropicVersion)
	return json.Marshal(m)
}

// bedrockCredsFor extracts the static IAM credentials from a bedrock account.
func bedrockCredsFor(account *config.Account) (awsCredentials, error) {
	ak := strings.TrimSpace(account.BedrockAccessKeyID)
	sk := strings.TrimSpace(account.BedrockSecretAccessKey)
	if ak == "" || sk == "" {
		return awsCredentials{}, fmt.Errorf("bedrock: account %s missing access key or secret", account.ID)
	}
	return awsCredentials{
		AccessKeyID:     ak,
		SecretAccessKey: sk,
		SessionToken:    strings.TrimSpace(account.BedrockSessionToken),
	}, nil
}

// authorizeBedrockRequest attaches authentication to a Bedrock request in place.
//
// Two credential styles are supported. When the account carries a Bedrock API key
// (a bearer token, "ABSK..." — set via BedrockAPIKey), it is used verbatim as an
// `Authorization: Bearer` header and SigV4 is skipped entirely; this is the newer
// Bedrock API-key auth and works for principals whose raw IAM access key is denied
// InvokeModel. Otherwise the account's static IAM access key SigV4-signs the request
// over payload. region is the SigV4 region (also the request host's region).
//
// Returns an error only when neither credential is usable, so callers surface a
// pre-stream failure and fail over.
func authorizeBedrockRequest(account *config.Account, req *http.Request, payload []byte, region string) error {
	if key := strings.TrimSpace(account.BedrockAPIKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
		return nil
	}
	creds, err := bedrockCredsFor(account)
	if err != nil {
		return err
	}
	signSigV4(req, payload, creds, region, bedrockService, time.Now())
	return nil
}

// bedrockRegionFor returns the account's region, defaulting to us-east-1.
func bedrockRegionFor(account *config.Account) string {
	if r := strings.TrimSpace(account.Region); r != "" {
		return r
	}
	return "us-east-1"
}

// bedrockEndpoint builds the invoke URL for a model id and streaming flag.
func bedrockEndpoint(region, modelID string, streaming bool) string {
	verb := "invoke"
	if streaming {
		verb = "invoke-with-response-stream"
	}
	// modelID is placed raw here; SigV4 canonicalization percent-encodes the path,
	// and net/http encodes the outgoing request-target consistently because we set
	// URL.Opaque below in the caller. See invokeBedrock*.
	return fmt.Sprintf("https://bedrock-runtime.%s.amazonaws.com/model/%s/%s", region, modelID, verb)
}

// bedrockTestReply runs a minimal non-streaming Bedrock call for the admin
// account-test panel and returns the assistant's reply text. It uses the Converse
// path for converse accounts and native invoke otherwise, so the test exercises
// the same code path a real request would.
func (h *Handler) bedrockTestReply(account *config.Account, model string) (string, error) {
	p := forwardParams{account: account, model: model}
	anthropicBody := []byte(`{"anthropic_version":"` + bedrockAnthropicVersion + `","max_tokens":16,"messages":[{"role":"user","content":"Reply with exactly: OK"}]}`)

	var resp *http.Response
	var err error
	if accountUsesConverse(account) {
		conv, cerr := anthropicToConverseBody(anthropicBody)
		if cerr != nil {
			return "", cerr
		}
		resp, err = h.doBedrockConverseInvoke(p, conv, false)
	} else {
		resp, err = h.doBedrockInvoke(p, anthropicBody, false)
	}
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("upstream status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	// Both paths yield an Anthropic Messages response (Converse is converted); pull
	// the text out for display.
	anthropicJSON := respBody
	if accountUsesConverse(account) {
		converted, _, _, cerr := converseResponseToAnthropicMessage(respBody, model)
		if cerr != nil {
			return "", cerr
		}
		anthropicJSON = converted
	}
	return extractAnthropicReplyText(anthropicJSON), nil
}

// extractAnthropicReplyText joins the text blocks of an Anthropic Messages
// response body.
func extractAnthropicReplyText(body []byte) string {
	var m struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	var parts []string
	for _, c := range m.Content {
		if c.Type == "text" && c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "")
}

// doBedrockInvoke builds, signs, and sends a Bedrock native-invoke request for an
// already-Anthropic-format body. Model resolution, body rewrite and per-account HTTP
// client live here; region selection, auth, throttle and the 429/access-error
// handling are delegated to invokeBedrockRegional so the account's candidate regions
// are tried and the callable one is learned/cached. The returned response body is
// the caller's to close. All returned errors occur before any client bytes, so
// callers may fail over.
func (h *Handler) doBedrockInvoke(p forwardParams, anthropicBody []byte, streaming bool) (*http.Response, error) {
	modelID, err := resolveBedrockModelID(p.account, p.model)
	if err != nil {
		return nil, err
	}
	body, err := buildBedrockBody(anthropicBody)
	if err != nil {
		return nil, err
	}
	return h.invokeBedrockRegional(p, modelID, body, func(region string) (*http.Request, error) {
		req, err := newBedrockRequest(region, modelID, streaming, body)
		if err != nil {
			return nil, err
		}
		// Bedrock returns AWS event-stream framing for streaming invokes and plain
		// JSON otherwise; set Accept to match so the wire format is unambiguous.
		if streaming {
			req.Header.Set("Accept", "application/vnd.amazon.eventstream")
		} else {
			req.Header.Set("Accept", "application/json")
		}
		return req, nil
	})
}

// invokeBedrockStream performs a streaming Bedrock call and re-emits each inner
// Anthropic event to the client as SSE, then bills the customer key. It mirrors
// forwardToUpstream's contract: returns nil once a response has been (at least
// partially) streamed, or an error before any client bytes so the caller can fail
// over to another account.
func (h *Handler) invokeBedrockStream(w http.ResponseWriter, flusher http.Flusher, p forwardParams) error {
	// Non-Anthropic models are served via the Converse API instead of native invoke.
	if accountUsesConverse(p.account) {
		return h.invokeBedrockConverseAnthropicStream(w, flusher, p)
	}
	reqStart := time.Now()

	resp, err := h.doBedrockInvoke(p, p.body, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return fmt.Errorf("bedrock: upstream status %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}

	var inputTokens, outputTokens int
	var streamedAny bool

	streamErr := readBedrockEventStream(resp.Body, func(eventType string, anthropicJSON []byte) error {
		// Track usage from the native events without disturbing passthrough.
		switch {
		case bytes.Contains(anthropicJSON, []byte(`"message_start"`)):
			if it := extractInputTokens(anthropicJSON); it > 0 {
				inputTokens = it
			}
		case bytes.Contains(anthropicJSON, []byte(`"message_delta"`)):
			if ot := extractOutputTokens(anthropicJSON); ot > 0 {
				outputTokens = ot
			}
		}

		// Derive the SSE event name from the inner event when the frame header
		// didn't carry one (Bedrock sets :event-type "chunk", not the Anthropic type).
		evtName := eventType
		if evtName == "" || evtName == "chunk" {
			evtName = innerEventType(anthropicJSON)
		}
		// A failed write means the CLIENT went away, not that Bedrock broke, so it
		// is tagged (writeAnthropicSSE) and classified separately below. streamedAny
		// is set only after the write succeeds so it means what it says: bytes
		// actually reached the client.
		if werr := writeAnthropicSSE(w, flusher, evtName, anthropicJSON); werr != nil {
			return werr // stop reading; disposition decided by the classifier
		}
		streamedAny = true
		return nil
	})

	switch classifyBedrockStreamOutcome(streamErr, streamedAny) {
	case bedrockStreamClientGone:
		// The customer hung up. Not the account's fault and nobody is left to fail
		// over for, so record nothing — matching the custom_api forwarder's
		// contract (custom_api_forward.go:445-447). Charging this to the account
		// cooled a healthy one after three departing clients.
		logger.Debugf("[Bedrock] client disconnected mid-stream (account %s): %v", p.account.ID, streamErr)
		return nil
	case bedrockStreamFailover:
		// Upstream failed before any client bytes → allow account failover.
		return streamErr
	case bedrockStreamPartialFailure:
		// Partial stream: the client already got a prefix so we cannot fail over,
		// but this is a FAILURE, not a success. Recording it as a success cleared
		// the account's error state and cooldown, letting an account that throws
		// repeated mid-stream exceptions keep looking healthy.
		h.recordBedrockPartialFailure(p, streamErr)
		return nil
	}

	h.recordBedrockSuccess(p, inputTokens, outputTokens, reqStart)
	return nil
}

// invokeBedrockNonStream performs a non-streaming Bedrock call, writes the JSON
// response to the client, and bills the customer key.
func (h *Handler) invokeBedrockNonStream(w http.ResponseWriter, p forwardParams) error {
	// Non-Anthropic models are served via the Converse API instead of native invoke.
	if accountUsesConverse(p.account) {
		return h.invokeBedrockConverseAnthropicNonStream(w, p)
	}
	reqStart := time.Now()

	resp, err := h.doBedrockInvoke(p, p.body, false)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, readErr := readBedrockResponseBody(resp)
	if readErr != nil {
		// Nothing has been written to the client yet, so returning an error here
		// lets the dispatch loop fail over instead of serving a truncated body
		// as a success.
		return readErr
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bedrock: upstream status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	inputTokens, outputTokens := extractNonStreamUsage(respBody)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(respBody)

	h.recordBedrockSuccess(p, inputTokens, outputTokens, reqStart)
	return nil
}

// bedrockMaxNonStreamResponseBytes bounds a non-streaming Bedrock response body.
// Matches the ceiling the sibling custom_api forwarder already applies to the same
// kind of read (custom_api_forward.go:413).
const bedrockMaxNonStreamResponseBytes = 32 << 20

// readBedrockResponseBody reads a non-streaming Bedrock response body with a byte
// ceiling and a CHECKED error.
//
// Both properties were missing at every non-stream call site, which used
// `respBody, _ := io.ReadAll(resp.Body)`:
//
//   - The discarded error meant that when Bedrock returned 200 and the body then
//     failed partway through, the partial bytes were written to the client as a
//     successful response and a success was billed. Nothing had reached the client
//     when the failure happened, so CLAUDE.md requires an error here instead, to
//     let the dispatch loop fail over to a healthy account.
//   - The unbounded read allocated proportionally to whatever the upstream sent.
//
// Returning ([]byte, error) rather than writing to the client keeps the decision
// with the caller, which must write only after a nil error.
func readBedrockResponseBody(resp *http.Response) ([]byte, error) {
	if resp == nil || resp.Body == nil {
		return nil, fmt.Errorf("bedrock: no response body")
	}
	// +1 so a body exactly at the ceiling is distinguishable from an over-limit one.
	body, err := io.ReadAll(io.LimitReader(resp.Body, bedrockMaxNonStreamResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("bedrock: read response body: %w", err)
	}
	if len(body) > bedrockMaxNonStreamResponseBytes {
		return nil, fmt.Errorf("bedrock: response body exceeds %d bytes", bedrockMaxNonStreamResponseBytes)
	}
	return body, nil
}

// newBedrockRequest builds the HTTP request with the model id carried as an opaque
// request-target so net/http does not re-encode the "%3A" in inference-profile ids.
// URL.Path is set to the decoded path so SigV4's canonicalizer sees the raw ":".
func newBedrockRequest(region, modelID string, streaming bool, body []byte) (*http.Request, error) {
	return newBedrockRequestForURL(bedrockEndpoint(region, modelID, streaming), body)
}

// bedrockHostSuffix is the only host suffix a Bedrock request may target.
const bedrockHostSuffix = ".amazonaws.com"

// newBedrockRequestForURL builds a POST request to an arbitrary Bedrock URL with
// the given body. Shared by the invoke and Converse paths so both construct the
// request identically before SigV4 signing.
//
// The resolved host is verified to be an AWS Bedrock host before the request is
// returned. Both callers build the URL by interpolating the account's region into
// the authority (bedrockEndpoint / bedrockConverseEndpoint), and the region is
// operator-supplied data that reaches config unvalidated. A crafted value such as
// "x@attacker.example/" turns the userinfo separator into a host boundary, so the
// parsed host became attacker.example while the string still LOOKED like an AWS
// URL — and authorizeBedrockRequest then attached the account's live credential
// (bearer Bedrock API key, or SigV4 access-key id + signature + session token) to
// a request aimed at an attacker-controlled server. Failing closed here covers
// every Bedrock caller through one funnel rather than trusting each URL builder.
func newBedrockRequestForURL(rawURL string, body []byte) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("bedrock: build request: %w", err)
	}
	// Check the PARSED host, not the raw string: the whole point of the attack is
	// that the two disagree. Reject userinfo outright — a legitimate Bedrock URL
	// never carries credentials in the authority, and its presence means the
	// region injected a host boundary.
	if req.URL.User != nil {
		return nil, fmt.Errorf("bedrock: refusing request with credentials in the URL authority (bad region?)")
	}
	host := strings.ToLower(req.URL.Hostname())
	if host == "" || !strings.HasSuffix(host, bedrockHostSuffix) {
		return nil, fmt.Errorf("bedrock: refusing request to non-AWS host %q (bad region?)", host)
	}
	return req, nil
}

// recordBedrockPartialFailure records a stream that broke AFTER the client already
// received bytes.
//
// Failover is correctly impossible here — the client holds committed headers and a
// partial body — but the outcome is still a FAILURE, and it used to be recorded as
// a success via recordBedrockSuccess. That was wrong in three ways:
//
//   - pool.RecordSuccess CLEARS the account's transient error state and cooldown, so
//     an account throwing repeated mid-stream Bedrock exceptions kept looking
//     healthy and kept being selected;
//   - the request log claimed success for a request the client saw fail;
//   - usage was metered from a truncated stream, whose terminal usage event never
//     arrived, so the figures were incomplete anyway.
//
// This mirrors the custom_api forwarder's mid-stream contract exactly
// (custom_api_forward.go:467-489): emit nothing further, record the error against
// the account, log a failure, and do not meter tokens.
func (h *Handler) recordBedrockPartialFailure(p forwardParams, streamErr error) {
	endpoint := "claude"
	if p.endpoint == "openai" || p.endpoint == "responses" {
		endpoint = "openai"
	}
	logger.Warnf("[Bedrock] stream ended with error after partial output (account %s): %v", p.account.ID, streamErr)
	h.pool.RecordError(p.account.ID, false)
	h.recordFailureWithDetails(endpoint, p.model, p.account.ID, p.apiKeyID, streamErr)
}

// recordBedrockSuccess bills the customer API key by tokens and updates account +
// pool stats, mirroring recordCustomApiSuccess. Credits are derived from a
// configurable per-1k-token rate purely for operator accounting/analytics; the
// customer key's TokenLimit is the real quota gate (RecordApiKeyUsage enforces it).
func (h *Handler) recordBedrockSuccess(p forwardParams, inputTokens, outputTokens int, reqStart time.Time) {
	endpoint := "claude"
	if p.endpoint == "openai" || p.endpoint == "responses" {
		endpoint = "openai"
	}
	credits := bedrockCreditsForTokens(inputTokens + outputTokens)
	h.recordSuccessForApiKey(p.apiKeyID, inputTokens, outputTokens, credits, p.model, p.account, endpoint, reqStart)
	h.pool.RecordSuccess(p.account.ID)
	h.pool.RecordLatency(p.account.ID, float64(time.Since(reqStart).Milliseconds()))
	h.pool.UpdateStats(p.account.ID, inputTokens+outputTokens, credits)
	h.recordSuccessLog(endpoint, p.model, p.account.ID, p.apiKeyID, inputTokens+outputTokens, credits, time.Since(reqStart).Milliseconds())
}

// bedrockCreditsForTokens converts a token count to operator credits using
// BEDROCK_CREDITS_PER_1K_TOKENS (default 0 → credits accounting disabled, pure
// token billing). Token-based keys work regardless of this value.
func bedrockCreditsForTokens(tokens int) float64 {
	rate := envFloatDefault("BEDROCK_CREDITS_PER_1K_TOKENS", 0)
	if rate <= 0 {
		return 0
	}
	return float64(tokens) / 1000.0 * rate
}

// envFloatDefault reads a float from env var name, returning def when unset or
// unparsable.
func envFloatDefault(name string, def float64) float64 {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

// ---- small JSON usage extractors (tolerant, allocation-light) ----

// extractInputTokens reads message.usage input tokens from a message_start event.
//
// Cache tokens are ADDED to input_tokens, not ignored. In the native Anthropic
// wire format input_tokens counts only the FRESH (uncached) prefix; tokens served
// from cache are reported separately as cache_read_input_tokens, and tokens
// written to cache as cache_creation_input_tokens. The upstream charges for all
// three, and buildBedrockBody preserves cache_control, so these fields are live
// on exactly this path.
//
// Reading only input_tokens therefore under-billed every cache-heavy request:
// 50 fresh + 100 cache-write + 200 cache-read was recorded as 50 tokens instead
// of 350, against the customer key's TokenLimit, TPM accounting, per-account and
// global totals, per-model usage, and token-derived credits.
//
// This matches how the Kiro path already computes the same figure internally —
// `inputTokens = uncached + cacheRead + cacheWrite` (proxy/kiro.go:966). The
// subtraction in billedClaudeInputTokens (cache_tracker.go:745) is a separate
// concern: it de-duplicates the CLIENT-FACING usage map, where the cache fields
// are reported alongside input_tokens and would otherwise be double-counted by
// the reader.
func extractInputTokens(eventJSON []byte) int {
	var e struct {
		Message struct {
			Usage bedrockUsageTokens `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(eventJSON, &e) != nil {
		return 0
	}
	return e.Message.Usage.totalInput()
}

// bedrockUsageTokens is the usage shape shared by the streaming and non-streaming
// extractors, so the two can never disagree about what counts as input.
type bedrockUsageTokens struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// totalInput returns every input token the upstream will charge for.
func (u bedrockUsageTokens) totalInput() int {
	return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

// extractOutputTokens reads usage.output_tokens from a message_delta event.
func extractOutputTokens(eventJSON []byte) int {
	var e struct {
		Usage struct {
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(eventJSON, &e) != nil {
		return 0
	}
	return e.Usage.OutputTokens
}

// extractNonStreamUsage reads usage input/output tokens from a full response.
// Input includes cache-read and cache-creation tokens for the reason documented
// on extractInputTokens: the upstream charges for all three, and counting only
// the fresh prefix under-bills every cache-heavy request.
func extractNonStreamUsage(respJSON []byte) (int, int) {
	var r struct {
		Usage bedrockUsageTokens `json:"usage"`
	}
	if json.Unmarshal(respJSON, &r) != nil {
		return 0, 0
	}
	return r.Usage.totalInput(), r.Usage.OutputTokens
}

// innerEventType reads the "type" field of an Anthropic event for SSE naming.
func innerEventType(eventJSON []byte) string {
	var e struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(eventJSON, &e) != nil || e.Type == "" {
		return "message"
	}
	return e.Type
}
