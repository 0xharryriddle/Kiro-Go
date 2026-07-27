// Package proxy is the core proxy layer for the Kiro API.
// It handles streaming API calls to the Kiro backend and parses AWS Event Stream responses.
package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/google/uuid"
)

// isPlaceholderReasoning reports whether a reasoning string carries no real
// content — it is empty, whitespace-only, or consists solely of dot / ellipsis
// runs (e.g. the "..." redaction marker the upstream sends in place of a hidden
// chain-of-thought for GPT-5.x / o-series models). Genuine reasoning always
// contains at least one non-dot, non-space character, so this never matches
// real chain-of-thought.
func isPlaceholderReasoning(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" {
		return true
	}
	for _, r := range t {
		if r != '.' && r != '\u2026' && !unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// Endpoint configuration (auto-fallback on quota exhaustion).
const directProxyOptOut = "direct"

type kiroEndpoint struct {
	Name      string
	URL       string
	Origin    string
	AmzTarget string
}

var kiroEndpoints = []kiroEndpoint{
	{
		URL:       "https://q.us-east-1.amazonaws.com/generateAssistantResponse",
		Origin:    "AI_EDITOR",
		AmzTarget: "",
		Name:      "Kiro IDE",
	},
	{
		URL:       "https://codewhisperer.us-east-1.amazonaws.com/generateAssistantResponse",
		Origin:    "AI_EDITOR",
		AmzTarget: "AmazonCodeWhispererStreamingService.GenerateAssistantResponse",
		Name:      "CodeWhisperer",
	},
	{
		URL:       "https://q.us-east-1.amazonaws.com/generateAssistantResponse",
		Origin:    "AI_EDITOR",
		AmzTarget: "AmazonQDeveloperStreamingService.SendMessage",
		Name:      "AmazonQ",
	},
}

// kiroCLIEndpoint is the headless / API Key path used by Kiro CLI:
// POST https://runtime.{region}.kiro.dev/ with AWS JSON 1.0 protocol.
var kiroCLIEndpoint = kiroEndpoint{
	URL:       "https://runtime.us-east-1.kiro.dev/",
	Origin:    "KIRO_CLI",
	AmzTarget: "AmazonCodeWhispererStreamingService.GenerateAssistantResponse",
	Name:      "Kiro CLI",
}

// Global HTTP clients, swappable at runtime to apply proxy reconfiguration without restart.
var kiroHttpStore atomic.Pointer[http.Client]
var kiroRestHttpStore atomic.Pointer[http.Client]

// proxyClientCache caches http.Client instances keyed by proxy URL for per-account proxy support.
var proxyClientCache sync.Map

func init() {
	InitKiroHttpClient("")
}

// GetClientForProxy returns an http.Client configured for the given proxy URL.
// If proxyURL is empty, returns the global kiro HTTP client.
func GetClientForProxy(proxyURL string) *http.Client {
	if proxyURL == "" {
		return kiroHttpStore.Load()
	}
	if cached, ok := proxyClientCache.Load(proxyURL); ok {
		return cached.(*http.Client)
	}
	client := &http.Client{
		Timeout:   5 * time.Minute,
		Transport: buildKiroTransport(proxyURL),
	}
	proxyClientCache.Store(proxyURL, client)
	return client
}

// GetRestClientForProxy returns a rest http.Client (30s timeout) for the given proxy URL.
// If proxyURL is empty, returns the global kiro REST HTTP client.
func GetRestClientForProxy(proxyURL string) *http.Client {
	if proxyURL == "" {
		return kiroRestHttpStore.Load()
	}
	cacheKey := "rest:" + proxyURL
	if cached, ok := proxyClientCache.Load(cacheKey); ok {
		return cached.(*http.Client)
	}
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: buildKiroTransport(proxyURL),
	}
	proxyClientCache.Store(cacheKey, client)
	return client
}

// ResolveAccountProxyURL returns the effective proxy URL for an account.
// Falls back to global config.GetProxyURL() if the account has no per-account proxy.
func ResolveAccountProxyURL(account *config.Account) string {
	if account != nil && strings.TrimSpace(account.ProxyURL) != "" {
		// Per-account opt-out: imported IDE credentials often need to mirror the IDE's
		// direct network path even when Kiro-Go has a global proxy configured.
		if isDirectProxyOptOut(account.ProxyURL) {
			return directProxyOptOut
		}
		return normalizeOutboundProxyURL(account.ProxyURL)
	}
	return normalizeOutboundProxyURL(config.GetProxyURL())
}

func isDirectProxyOptOut(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "direct", "none", "no_proxy", "noproxy", "off":
		return true
	default:
		return false
	}
}

func normalizeOutboundProxyURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if isDirectProxyOptOut(raw) {
		return directProxyOptOut
	}
	if !strings.Contains(raw, "://") {
		return "http://" + raw
	}
	return raw
}

// buildKiroTransport constructs an HTTP Transport with optional outbound proxy support.
func buildKiroTransport(proxyURL string) *http.Transport {
	t := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  false,
		ForceAttemptHTTP2:   true,
	}
	if isDirectProxyOptOut(proxyURL) {
		return t
	}
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			t.Proxy = http.ProxyURL(u)
			// Proxied connections cannot negotiate HTTP/2.
			t.ForceAttemptHTTP2 = false
		}
	} else {
		t.Proxy = http.ProxyFromEnvironment
	}
	return t
}

// InitKiroHttpClient initializes (or reinitializes) the HTTP clients used for Kiro API requests.
func InitKiroHttpClient(proxyURL string) {
	client := &http.Client{
		Timeout:   5 * time.Minute,
		Transport: buildKiroTransport(proxyURL),
	}
	kiroHttpStore.Store(client)

	restClient := &http.Client{
		Timeout:   30 * time.Second,
		Transport: buildKiroTransport(proxyURL),
	}
	kiroRestHttpStore.Store(restClient)
}

// ==================== Request Structs ====================

// KiroPayload is the top-level request body sent to the Kiro API.
type KiroPayload struct {
	ConversationState struct {
		AgentContinuationId string `json:"agentContinuationId,omitempty"`
		AgentTaskType       string `json:"agentTaskType,omitempty"`
		ChatTriggerType     string `json:"chatTriggerType"`
		ConversationID      string `json:"conversationId"`
		CurrentMessage      struct {
			UserInputMessage KiroUserInputMessage `json:"userInputMessage"`
		} `json:"currentMessage"`
		History []KiroHistoryMessage `json:"history,omitempty"`
	} `json:"conversationState"`
	ProfileArn      string           `json:"profileArn,omitempty"`
	InferenceConfig *InferenceConfig `json:"inferenceConfig,omitempty"`

	// ToolNameMap maps sanitized tool names (sent to Kiro) back to the
	// original names supplied by the client. Used to restore original names
	// in tool_use responses so the client can match them to its tool registry.
	// Not serialized to the Kiro API request body.
	ToolNameMap map[string]string `json:"-"`
}

type KiroUserInputMessage struct {
	Content                 string                   `json:"content"`
	ModelID                 string                   `json:"modelId,omitempty"`
	Origin                  string                   `json:"origin"`
	Images                  []KiroImage              `json:"images,omitempty"`
	UserInputMessageContext *UserInputMessageContext `json:"userInputMessageContext,omitempty"`
}

type UserInputMessageContext struct {
	Tools       []KiroToolWrapper `json:"tools,omitempty"`
	ToolResults []KiroToolResult  `json:"toolResults,omitempty"`
}

type KiroToolWrapper struct {
	ToolSpecification struct {
		Name        string      `json:"name"`
		Description string      `json:"description"`
		InputSchema InputSchema `json:"inputSchema"`
	} `json:"toolSpecification"`
}

type InputSchema struct {
	JSON interface{} `json:"json"`
}

type KiroToolResult struct {
	ToolUseID string              `json:"toolUseId"`
	Content   []KiroResultContent `json:"content"`
	Status    string              `json:"status"`
}

type KiroResultContent struct {
	Text string `json:"text"`
}

type KiroImage struct {
	Format string `json:"format"`
	Source struct {
		Bytes string `json:"bytes"`
	} `json:"source"`
}

type KiroHistoryMessage struct {
	UserInputMessage         *KiroUserInputMessage         `json:"userInputMessage,omitempty"`
	AssistantResponseMessage *KiroAssistantResponseMessage `json:"assistantResponseMessage,omitempty"`
}

type KiroAssistantResponseMessage struct {
	Content  string        `json:"content"`
	ToolUses []KiroToolUse `json:"toolUses,omitempty"`
}

type KiroToolUse struct {
	ToolUseID string                 `json:"toolUseId"`
	Name      string                 `json:"name"`
	Input     map[string]interface{} `json:"input"`
}

type InferenceConfig struct {
	MaxTokens   int     `json:"maxTokens,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
	TopP        float64 `json:"topP,omitempty"`
}

// ==================== Stream Callbacks ====================

// KiroStreamCallback stream response callbacks
type KiroStreamCallback struct {
	OnText         func(text string, isThinking bool)
	OnToolUse      func(toolUse KiroToolUse)
	OnComplete     func(inputTokens, outputTokens int)
	OnError        func(err error)
	OnCredits      func(credits float64)
	OnContextUsage func(percentage float64)

	// OnUpstreamException reports an AWS event-stream EXCEPTION frame, i.e. a
	// frame whose `:message-type` header is "exception" rather than "event".
	//
	// This is OBSERVATION ONLY and deliberately so. The parser previously read
	// just the `:event-type` header (extractEventType), so an exception frame
	// matched no case in the dispatch switch and was silently discarded — which
	// is why a mid-stream upstream failure could only ever reach the handler as
	// a transport error, classifying as HTTP 500 / api_error no matter what the
	// upstream actually said.
	//
	// It does NOT abort the stream, because the decision of which exception
	// types are fatal cannot be made responsibly yet: no real Kiro exception
	// frame has been observed. The trace corpus cannot supply one either —
	// traceRecorder.noteResponseText stores ASSEMBLED text, so frame headers are
	// destroyed before capture. Treating a frame as fatal on the strength of the
	// AWS spec alone risks killing live streams mid-answer on the path every
	// request uses, which is a worse failure than the classification imprecision
	// it would fix.
	//
	// So this exists to make the frames observable. Once a real one is recorded,
	// promoting specific exception types to fatal becomes a small change made
	// against evidence instead of inference.
	OnUpstreamException func(exceptionType string, payload []byte)
}

// ==================== API Call ====================

func setPayloadProfileArnForAccount(payload *KiroPayload, account *config.Account) {
	if payload == nil {
		return
	}

	// API Key credentials must not carry IDE/profile semantics.
	if config.IsAPIKeyAccount(account) {
		payload.ProfileArn = ""
		return
	}

	payload.ProfileArn = strings.TrimSpace(payload.ProfileArn)
	if account != nil {
		if profileArn := strings.TrimSpace(account.ProfileArn); profileArn != "" {
			payload.ProfileArn = profileArn
		}
	}
}

// endpointsForAccount returns the upstream endpoint list for a credential.
// API Key accounts always use the CLI runtime protocol; OAuth accounts keep
// the configured preferred-endpoint fallback chain.
func endpointsForAccount(account *config.Account) []kiroEndpoint {
	if config.IsAPIKeyAccount(account) {
		return []kiroEndpoint{kiroCLIEndpoint}
	}
	return getSortedEndpoints(config.GetPreferredEndpoint())
}

// cliRuntimeURL builds the regional Kiro CLI runtime URL.
func cliRuntimeURL(account *config.Account) string {
	region := "us-east-1"
	if account != nil {
		if r := strings.TrimSpace(account.Region); r != "" {
			region = r
		}
	}
	return fmt.Sprintf("https://runtime.%s.kiro.dev/", region)
}

// getSortedEndpoints returns endpoints ordered by user preference, with optional fallback.
func getSortedEndpoints(preferred string) []kiroEndpoint {
	fallback := config.GetEndpointFallback()

	var primary int
	switch preferred {
	case "kiro":
		primary = 0
	case "codewhisperer":
		primary = 1
	case "amazonq":
		primary = 2
	default:
		// "auto": Kiro first, then fallback to others
		return []kiroEndpoint{kiroEndpoints[0], kiroEndpoints[1], kiroEndpoints[2]}
	}

	if !fallback {
		// No fallback: only use the selected endpoint
		return []kiroEndpoint{kiroEndpoints[primary]}
	}

	// With fallback: selected first, then others in order
	result := []kiroEndpoint{kiroEndpoints[primary]}
	for i, ep := range kiroEndpoints {
		if i != primary {
			result = append(result, ep)
		}
	}
	return result
}

// validateKiroDispatchProfile enforces the host/profile region invariant before
// any network request is created. Without a region override, the historical
// soft-fail behavior is preserved. With an override, OAuth credentials require
// a matching profile ARN; key-scoped API-key credentials may omit it.
func validateKiroDispatchProfile(account *config.Account, profileArn string) error {
	if account == nil || account.EffectiveRegionOverride() == "" {
		return nil
	}
	arn := strings.TrimSpace(profileArn)
	if (arn == "" && !account.IsKiroAPIKeyCredential()) || (arn != "" && !arnRegionAllowed(account, arn)) {
		return fmt.Errorf("no available Kiro profile in override region %q", account.EffectiveRegionOverride())
	}
	return nil
}

// upstreamError builds a classifiable error from a non-200 Kiro response.
// 402 is tagged "overage" so the failover layer routes it to overage handling
// (disableAccountOverage → refresh OverageStatus) instead of falling through to
// the generic RecordError path. All other codes produce "HTTP <code> ...", which
// pool.IsAuthFailure reads via its digit-boundary status-token matcher.
func upstreamError(statusCode int, endpoint, body string) error {
	if statusCode == 402 {
		return fmt.Errorf("HTTP 402 overage from %s: %s", endpoint, body)
	}
	return fmt.Errorf("HTTP %d from %s: %s", statusCode, endpoint, body)
}

// parseAndStream wraps parseEventStream so CallKiroAPI's 200 path can defer the
// upstream body close (closing even on a callback panic, so the TCP connection
// is returned to the transport pool instead of leaking). Without the defer, a
// panic in OnText/OnToolUse/parseEventStream unwinds past a plain Close and
// leaks the connection (repeated panics exhaust MaxIdleConnsPerHost=20 / FDs;
// net/http's per-request recover catches the panic but the body stays open).
func parseAndStream(body io.ReadCloser, callback *KiroStreamCallback) error {
	defer body.Close()
	return parseEventStream(body, callback)
}

// CallKiroAPI calls the Kiro streaming API, trying each configured endpoint with automatic fallback.
//
// This is a thin wrapper that discards upstream diagnostics. Callers that need
// the per-endpoint HTTP status, upstream correlation ID, or resolved host for
// tracing should use CallKiroAPIWithDiagnostics instead.
func CallKiroAPI(account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	return CallKiroAPIWithDiagnostics(account, payload, callback, nil)
}

// CallKiroAPIWithDiagnostics is CallKiroAPI plus per-endpoint upstream detail.
//
// diag may be nil, in which case behaviour is identical to CallKiroAPI. When
// non-nil it accumulates one entry per endpoint tried, including the HTTP status
// and any upstream correlation ID — detail that was previously read and thrown
// away, leaving a support escalation with nothing to correlate on.
func CallKiroAPIWithDiagnostics(account *config.Account, payload *KiroPayload, callback *KiroStreamCallback, diag *KiroCallDiagnostics) error {
	originalProfileArn := ""
	if payload != nil {
		originalProfileArn = payload.ProfileArn
		defer func() {
			payload.ProfileArn = originalProfileArn
		}()
	}
	setPayloadProfileArnForAccount(payload, account)

	if _, err := json.Marshal(payload); err != nil {
		return err
	}

	// Debug: dump full payload for troubleshooting upstream rejections
	if payloadJSON, err := json.Marshal(payload); err == nil {
		logger.Debugf("[KiroAPI] Request payload: %s", string(payloadJSON))
	}

	// Wrap OnToolUse to restore original tool names for the client.
	if callback != nil && callback.OnToolUse != nil && len(payload.ToolNameMap) > 0 {
		originalOnToolUse := callback.OnToolUse
		nameMap := payload.ToolNameMap
		wrapped := *callback
		wrapped.OnToolUse = func(tu KiroToolUse) {
			if original, ok := nameMap[tu.Name]; ok {
				tu.Name = original
			}
			originalOnToolUse(tu)
		}
		callback = &wrapped
	}

	if payload != nil && strings.TrimSpace(payload.ProfileArn) == "" && !config.IsAPIKeyAccount(account) {
		if profileArn, err := ResolveProfileArn(account); err == nil {
			payload.ProfileArn = profileArn
		} else if isProfileArnResolutionSoftError(err) {
			logger.Debugf("[ProfileArn] Skipped profile ARN resolution for %s: %v", accountEmailForLog(account), err)
		} else {
			logger.Warnf("[ProfileArn] Failed to resolve profile ARN for %s: %v", accountEmailForLog(account), err)
		}
	}

	// Fail closed under a region override: never dispatch a request whose profile
	// ARN is empty or belongs to a region other than the override. Kiro API-key
	// credentials are key-scoped, so an empty ARN is valid for them while the
	// override still pins the regional host.
	profileArn := ""
	if payload != nil {
		profileArn = payload.ProfileArn
	}
	if err := validateKiroDispatchProfile(account, profileArn); err != nil {
		return err
	}

	// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): union. The fork's fail-closed
	// region-override validation above is kept, and upstream's credential-aware
	// endpoint selection replaces the fork's unconditional getSortedEndpoints:
	// endpointsForAccount routes api_key accounts to the Kiro CLI runtime host
	// instead of the IDE/Q hosts, which is required for those credentials to work
	// at all. isAPIKey is consumed by the per-endpoint rewrite inside the loop.
	endpoints := endpointsForAccount(account)
	isAPIKey := config.IsAPIKeyAccount(account)

	var lastErr error
	// shrunkForLength guards the one-shot payload reduction below. It is declared
	// outside the endpoint loop so a single request can shrink at most once in
	// total, never once per endpoint.
	shrunkForLength := false
	for _, ep := range endpoints {
	retryEndpoint:
		// Update the origin field for the selected endpoint.
		payload.ConversationState.CurrentMessage.UserInputMessage.Origin = ep.Origin

		// Target the profile's data-plane region; endpoint URLs are declared for us-east-1.
		// API Key accounts use the CLI runtime host instead of IDE/Q hosts.
		epURL := regionalizeURLForProfile(ep.URL, account, payload.ProfileArn)
		if isAPIKey {
			epURL = cliRuntimeURL(account)
		}

		reqBody, _ := json.Marshal(payload)
		req, err := http.NewRequest("POST", epURL, bytes.NewReader(reqBody))
		if err != nil {
			lastErr = err
			continue
		}

		host := ""
		if parsedURL, parseErr := url.Parse(epURL); parseErr == nil {
			host = parsedURL.Host
		}
		headerValues := buildStreamingHeaderValues(account, host)

		if isAPIKey {
			req.Header.Set("Content-Type", "application/x-amz-json-1.0")
		} else {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept", "*/*")
		if ep.AmzTarget != "" {
			req.Header.Set("X-Amz-Target", ep.AmzTarget)
		}
		applyKiroBaseHeaders(req, account, headerValues)
		if !isAPIKey {
			req.Header.Set("x-amzn-kiro-agent-mode", "vibe")
		}
		// CLI captures use optout=false; IDE path keeps true.
		if isAPIKey {
			req.Header.Set("x-amzn-codewhisperer-optout", "false")
		} else {
			req.Header.Set("x-amzn-codewhisperer-optout", "true")
		}
		req.Header.Set("Amz-Sdk-Request", "attempt=1; max=3")
		req.Header.Set("Amz-Sdk-Invocation-Id", uuid.New().String())

		epStart := time.Now()
		// Base diagnostics for this endpoint attempt. Region comes from the
		// regionalised URL, so it reflects where the request actually went
		// rather than the account's (possibly unrelated) auth region.
		epAttempt := TraceAttempt{
			UpstreamEndpoint: ep.Name,
			UpstreamHost:     host,
			Region:           regionFromKiroHost(host),
			ProfileArn:       profileArnSuffix(payload.ProfileArn),
			StartedAtMs:      epStart.UnixMilli(),
		}
		recordEndpointAttempt := func(status int, header http.Header, outcome, retryAfter string, callErr error) {
			epAttempt.HTTPStatus = status
			epAttempt.Outcome = outcome
			epAttempt.DurationMs = time.Since(epStart).Milliseconds()
			epAttempt.RetryAfter = retryAfter
			if header != nil {
				epAttempt.UpstreamRequestID = upstreamRequestIDFromHeader(header)
			}
			if callErr != nil {
				msg := callErr.Error()
				epAttempt.ErrorType = classifyError(msg)
				epAttempt.Error = scrubTraceText(msg)
			}
			diag.record(epAttempt)
		}

		resp, err := GetClientForProxy(ResolveAccountProxyURL(account)).Do(req)
		if err != nil {
			lastErr = err
			recordEndpointAttempt(0, nil, outcomeError, "", err)
			logger.Warnf("[KiroAPI] Endpoint %s failed: %v", ep.Name, err)
			continue
		}

		if resp.StatusCode == 429 {
			retryAfter := retryAfterFromHeader(resp.Header.Get("Retry-After"))
			header := resp.Header
			resp.Body.Close()
			if retryAfter != "" {
				logger.Warnf("[KiroAPI] Endpoint %s throttled/quota exhausted (429, retry after %s); stopping endpoint fan-out for this account", ep.Name, retryAfter)
				err := fmt.Errorf("HTTP 429 from %s: quota exhausted; retry after %s", ep.Name, retryAfter)
				recordEndpointAttempt(429, header, outcomeError, retryAfter, err)
				return err
			}
			logger.Warnf("[KiroAPI] Endpoint %s throttled/quota exhausted (429); stopping endpoint fan-out for this account", ep.Name)
			err := fmt.Errorf("HTTP 429 from %s: quota exhausted", ep.Name)
			recordEndpointAttempt(429, header, outcomeError, "", err)
			return err
		}

		if resp.StatusCode != 200 {
			errBody, _ := io.ReadAll(resp.Body)
			status := resp.StatusCode
			header := resp.Header
			resp.Body.Close()
			// upstreamError tags 402 as "overage" so the failover layer routes it
			// to overage handling instead of the generic RecordError path.
			lastErr = upstreamError(status, ep.Name, string(errBody))
			recordEndpointAttempt(status, header, outcomeError, "", lastErr)
			// Auth failures (401/403) and overage (402) are account-level: do not
			// retry across endpoints. Other status codes fall through to the next
			// endpoint.
			if status == 401 || status == 403 || status == 402 {
				return lastErr
			}
			// The request was too large for the model. Rotating endpoints or
			// accounts cannot help — every one of them rejects the same bytes —
			// so shrink the payload once and retry the SAME endpoint. Nothing
			// has streamed to the client yet on this path (the non-200 branch
			// runs before parseAndStream), which is what makes retrying safe
			// here and nowhere later.
			//
			// This is the recovery net for a body ceiling that is now derived
			// from each model's declared window rather than one hand-tuned
			// constant: if that derivation ever over-estimates what Kiro will
			// accept, the request degrades to a smaller context instead of
			// failing outright.
			if isInputTooLongErrorMessage(lastErr.Error()) && !shrunkForLength {
				shrunkForLength = true
				if shrinkPayloadAfterLengthRejection(payload) {
					logger.Warnf("[KiroAPI] Endpoint %s rejected the request as too long; retrying once with a reduced payload", ep.Name)
					goto retryEndpoint
				}
				logger.Warnf("[KiroAPI] Endpoint %s rejected the request as too long and it could not be reduced further", ep.Name)
				return lastErr
			}
			logger.Warnf("[KiroAPI] Endpoint %s error: %v", ep.Name, lastErr)
			continue
		}

		streamHeader := resp.Header
		// parseAndStream defers resp.Body.Close(), so a panic in a streaming
		// callback (OnText/OnToolUse/parseEventStream) still returns the upstream
		// TCP connection to the transport pool instead of leaking it.
		err = parseAndStream(resp.Body, callback)
		if err != nil {
			recordEndpointAttempt(200, streamHeader, outcomeError, "", err)
		} else {
			recordEndpointAttempt(200, streamHeader, outcomeSuccess, "", nil)
		}
		return err
	}

	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("all endpoints failed")
}

func accountEmailForLog(account *config.Account) string {
	if account == nil {
		return "<nil>"
	}
	return account.Email
}

func retryAfterFromHeader(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if seconds, err := strconv.Atoi(raw); err == nil && seconds >= 0 {
		return (time.Duration(seconds) * time.Second).String()
	}
	if t, err := http.ParseTime(raw); err == nil {
		if d := time.Until(t).Round(time.Second); d > 0 {
			return d.String()
		}
	}
	return raw
}

// ==================== Event Stream Parsing ====================

// maxEventStreamMessageBytes caps a single AWS event-stream message's total
// length before parseEventStream allocates a buffer for it. Real Kiro/AWS
// event-stream messages are small (text deltas + tool JSON, low KB); a corrupt
// or malicious 32-bit totalLength near 2^32 would otherwise drive a multi-GB
// make([]byte, …) (alloc-panic under net/http's recover → connection dropped
// with no terminal event → client hang) or hold a multi-GB buffer to the
// 5-minute client timeout (memory-pressure DoS). 16 MiB is a generous ceiling.
const maxEventStreamMessageBytes = 16 * 1024 * 1024

// errEventStreamFrameTooLarge is returned by parseEventStream when a frame's
// totalLength exceeds maxEventStreamMessageBytes, so the caller (CallKiroAPI)
// funnels it into the upstream-error / response.failed path instead of
// allocating gigabytes.
var errEventStreamFrameTooLarge = errors.New("event-stream: frame totalLength exceeds maximum")

// errKiroEmptyStream reports an HTTP 200 whose body carried no event-stream data
// at all. It is the Kiro-path counterpart of errBedrockEmptyStream.
var errKiroEmptyStream = errors.New("kiro stream: upstream returned no events")

// parseEventStream decodes an AWS binary Event Stream response body.
func parseEventStream(body io.Reader, callback *KiroStreamCallback) error {
	if callback == nil {
		callback = &KiroStreamCallback{}
	}

	// Read directly without bufio to avoid buffering latency in streaming responses.
	var inputTokens, outputTokens int
	var totalCredits float64
	var currentToolUse *toolUseState

	// Read the placeholder-reasoning toggle once (not per event). When on
	// (default), reasoning that is still a pure redaction placeholder ("...") is
	// withheld; the moment real reasoning text appears the cumulative buffer is
	// no longer placeholder-only and every delta flows normally.
	suppressPlaceholderReasoning := config.GetThinkingConfig().SuppressPlaceholderReasoning

	// framesSeen distinguishes "the stream ended normally" from "no frame ever
	// arrived". Both used to return nil, and on the Claude/OpenAI streaming paths
	// that nil is the ONLY failover signal — so an HTTP 200 with an empty body was
	// served to the customer as a successful empty answer: no failover to a healthy
	// account, pool.RecordSuccess CLEARING the offending account's error count and
	// cooldown (pool/account.go:818), the customer key billed an estimated input
	// total for zero output (handler.go:2144-2146, :2163-2167), and the client sent
	// stop_reason "end_turn" as though the empty response were real.
	//
	// 8f49a4b fixed exactly this in both Bedrock readers (readBedrockEventStream,
	// readBedrockConverseEventStream) and left this third reader — the one that
	// serves the main Kiro path — unguarded. This is that same guard.
	//
	// Counted for any prelude that ARRIVES, including a frame the loop then skips
	// as malformed: a skipped frame still proves the upstream responded, which is
	// the distinction this guard exists to make (and which
	// TestEventStreamHandlesUndersizedFrameLength pins).
	framesSeen := 0
	for {
		// Prelude: 12 bytes (total_len + headers_len + crc)
		prelude := make([]byte, 12)
		_, err := io.ReadFull(body, prelude)
		if err == io.EOF {
			// Clean EOF on a frame boundary is a normal end of stream — but only
			// when at least one frame actually arrived.
			if framesSeen > 0 {
				break
			}
			return errKiroEmptyStream
		}
		if err != nil {
			return err
		}
		framesSeen++

		totalLength := int(prelude[0])<<24 | int(prelude[1])<<16 | int(prelude[2])<<8 | int(prelude[3])
		headersLength := int(prelude[4])<<24 | int(prelude[5])<<16 | int(prelude[6])<<8 | int(prelude[7])

		if totalLength < 16 {
			continue
		}
		// Reject a corrupt/malicious totalLength before the multi-GB make — a
		// single bit-flip in this 32-bit field would otherwise drive
		// make([]byte, ~4GB) (alloc-panic under net/http's per-request recover →
		// no terminal event → client hang) or hold a multi-GB buffer to the 5-min
		// client timeout (memory-pressure DoS). Returning an error here funnels
		// the corrupt frame into the upstream-error / response.failed path
		// instead of allocating gigabytes.
		if totalLength > maxEventStreamMessageBytes {
			return errEventStreamFrameTooLarge
		}

		// Read the remaining message bytes.
		remaining := totalLength - 12
		msgBuf := make([]byte, remaining)
		_, err = io.ReadFull(body, msgBuf)
		if err != nil {
			return err
		}

		if headersLength > len(msgBuf)-4 {
			continue
		}

		eventType := extractEventType(msgBuf[0:headersLength])
		payloadBytes := msgBuf[headersLength : len(msgBuf)-4]

		// Surface AWS event-stream EXCEPTION frames.
		//
		// The dispatch switch below keys off `:event-type`, which an exception
		// frame does not carry — it sets `:message-type: exception` plus
		// `:exception-type`. So such a frame matched no case and was discarded
		// without a trace, which is why a mid-stream upstream failure reached
		// the handler only as a transport error (classified HTTP 500 /
		// api_error) regardless of what the upstream reported.
		//
		// Reported, NOT acted on: see KiroStreamCallback.OnUpstreamException for
		// why promoting these to fatal needs a real observed frame first. The
		// stream continues exactly as before, so this cannot change behaviour
		// for any traffic that works today.
		if headers := msgBuf[0:headersLength]; isUpstreamFailureFrame(headers, eventType) {
			exceptionType := upstreamFailureLabel(headers, eventType)
			// Count usage BEFORE skipping. A failure frame can still carry a
			// usage block, and the pre-existing code fed every frame to
			// updateTokensFromEvent before this branch existed — so skipping
			// unconditionally silently stopped billing for those tokens. The
			// upstream charged for them either way.
			if len(payloadBytes) > 0 {
				var failureEvent map[string]interface{}
				if json.Unmarshal(payloadBytes, &failureEvent) == nil {
					inputTokens, outputTokens = updateTokensFromEvent(failureEvent, inputTokens, outputTokens)
				}
			}
			logger.Warnf("[KiroStream] upstream failure frame: type=%q payloadBytes=%d",
				truncateForLog(exceptionType, maxLoggedExceptionType), len(payloadBytes))
			if callback.OnUpstreamException != nil {
				callback.OnUpstreamException(exceptionType, payloadBytes)
			}
			continue
		}

		if len(payloadBytes) == 0 {
			continue
		}

		var event map[string]interface{}
		if err := json.Unmarshal(payloadBytes, &event); err != nil {
			continue
		}

		inputTokens, outputTokens = updateTokensFromEvent(event, inputTokens, outputTokens)

		// Dispatch by event type.
		switch eventType {
		// Both text streams are passed through verbatim: Kiro sends
		// assistantResponseEvent and reasoningContentEvent as incremental deltas, not
		// as cumulative snapshots. A dropped stream is retried as a whole new request
		// rather than resumed from an offset.
		//
		// Scope of what the transport actually guarantees: the single TCP connection
		// carrying the response gives byte ordering and prevents a TRANSPORT-level
		// retransmission from surfacing as a duplicate event. It says nothing about
		// the upstream APPLICATION emitting the same event twice -- a producer-side
		// replay remains possible in principle. Do not read TCP as proof of
		// at-most-once delivery at the event layer.
		//
		// Do NOT reintroduce content-based de-duplication here. At the string level a
		// replayed chunk is indistinguishable from text that simply repeats itself, so
		// such a heuristic can only guess -- and when it guesses wrong it silently eats
		// real output. The previous implementation turned "6666666666" into "666",
		// "abababab" into "abab" and "1833" into "183", on both streams. The events we
		// decode carry no sequence number or message id we can use to tell the two
		// cases apart; extractEventType only reads :event-type, so if a future protocol
		// revision adds an ordering/version header, CHECK THE DECODED HEADERS before
		// concluding none exists rather than relying on the base-spec argument.
		//
		// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): upstream removed the
		// normalizeChunk() de-duplication (their #138) and this fork removed it
		// independently; the two removals agree. The fork's per-chunk
		// placeholder-reasoning suppression is NOT part of that heuristic and is
		// retained below.
		//
		// Caveat on the delta premise: it is an inference from the protocol shape and
		// from upstream's own bug report. It is NOT confirmed by the captured traces,
		// and that was checked rather than assumed: traceRecorder.noteResponseText
		// stores already-assembled text (request_trace_recorder.go), and a scan of the
		// full local corpus (9,312 capture files) found ZERO retaining per-frame
		// boundaries. No capture mode helps -- off/meta/redacted/full govern how much
		// text is kept, not whether frame boundaries survive, and the boundaries are
		// destroyed upstream of the recorder. So the corpus cannot settle this
		// question, no matter how large it grows.
		//
		// If doubled text is ever observed in the wild, the fix is NOT "compare the new
		// chunk against the accumulated buffer": that is still content guessing, and it
		// silently deletes valid output. Frames ["ha", "haha"] are legitimate deltas
		// meaning "hahaha", but a prefix-of-accumulator test emits only "ha" and yields
		// "haha", eating a real chunk -- the same class of bug as normalizeChunk.
		//
		// The sound way to settle it is a predicate-only observer, not a mutator: for
		// ADJACENT non-empty payloads on the same channel, evaluate
		// strings.HasPrefix(current, previous) in memory and persist only
		// fixed-cardinality counters (pairs checked, prefix / non-prefix / equal
		// counts, per event type). ONE same-block non-prefix pair conclusively refutes
		// the cumulative hypothesis. Compare against the PREVIOUS PAYLOAD, never the
		// concatenated accumulator. Persist no text, no prefixes, no raw frames and no
		// content hashes -- reasoning text routinely contains source code and secrets,
		// and "debug-only" is not an adequate control for retaining it.
		case "assistantResponseEvent":
			if content, ok := event["content"].(string); ok && content != "" {
				if callback.OnText != nil {
					callback.OnText(content, false)
				}
			}
		case "reasoningContentEvent":
			if text, ok := event["text"].(string); ok && text != "" {
				if callback.OnText != nil {
					// Suppress ONLY the individual redaction-placeholder deltas
					// ("...", signature-block markers). This is a per-chunk
					// decision, not a cumulative-buffer one: every real reasoning
					// delta flows through, so extended-thinking text is never lost
					// even when it is interleaved with redacted "..." markers.
					// Disabled by config toggle.
					if suppressPlaceholderReasoning && isPlaceholderReasoning(text) {
						// placeholder-only delta carries no information — skip
					} else {
						callback.OnText(text, true)
					}
				}
			}
		case "toolUseEvent":
			currentToolUse = handleToolUseEvent(event, currentToolUse, callback)
		case "meteringEvent":
			if usage, ok := event["usage"].(float64); ok {
				totalCredits += usage
			}
		case "contextUsageEvent":
			if pct, ok := event["contextUsagePercentage"].(float64); ok {
				if callback.OnContextUsage != nil {
					callback.OnContextUsage(pct)
				}
			}
		}
	}

	if currentToolUse != nil {
		finishToolUse(currentToolUse, callback)
	}

	if callback.OnCredits != nil && totalCredits > 0 {
		callback.OnCredits(totalCredits)
	}

	if callback.OnComplete != nil {
		callback.OnComplete(inputTokens, outputTokens)
	}
	return nil
}

func updateTokensFromEvent(event map[string]interface{}, currentInputTokens, currentOutputTokens int) (int, int) {
	candidates := []map[string]interface{}{event}
	collectUsageMaps(event, &candidates)

	inputTokens := currentInputTokens
	outputTokens := currentOutputTokens

	for _, usage := range candidates {
		if usage == nil {
			continue
		}

		if v, ok := readTokenNumber(usage,
			"outputTokens", "completionTokens", "totalOutputTokens",
			"output_tokens", "completion_tokens", "total_output_tokens",
		); ok {
			outputTokens = v
		}

		if v, ok := readTokenNumber(usage,
			"inputTokens", "promptTokens", "totalInputTokens",
			"input_tokens", "prompt_tokens", "total_input_tokens",
		); ok {
			inputTokens = v
			continue
		}

		uncached, _ := readTokenNumber(usage, "uncachedInputTokens", "uncached_input_tokens")
		cacheRead, _ := readTokenNumber(usage, "cacheReadInputTokens", "cache_read_input_tokens")
		cacheWrite, _ := readTokenNumber(usage, "cacheWriteInputTokens", "cache_write_input_tokens", "cacheCreationInputTokens", "cache_creation_input_tokens")
		if uncached+cacheRead+cacheWrite > 0 {
			inputTokens = uncached + cacheRead + cacheWrite
			continue
		}

		total, ok := readTokenNumber(usage, "totalTokens", "total_tokens")
		if ok && total > 0 {
			candidateOutput := outputTokens
			if v, vok := readTokenNumber(usage,
				"outputTokens", "completionTokens", "totalOutputTokens",
				"output_tokens", "completion_tokens", "total_output_tokens",
			); vok {
				candidateOutput = v
			}
			if total-candidateOutput > 0 {
				inputTokens = total - candidateOutput
			}
		}
	}

	return inputTokens, outputTokens
}

// getContextWindowSize returns the context window size (in tokens) for a model.
//
// Per Kiro's ListAvailableModels, the 1M-token context window applies to
// Claude 4.6 and newer (sonnet-4.6, opus-4.6, opus-4.7, opus-4.8, and future
// 4.x releases), while 4.5 and earlier (opus-4.5, sonnet-4.5, sonnet-4,
// haiku-4.5) use a 200K window. This value is used to convert the upstream
// contextUsagePercentage into an absolute input-token count that clients rely
// on to decide when to compact; an undersized window under-reports tokens and
// prevents clients from compacting in time.
//
// An upstream-DECLARED limit wins over the name heuristic below. Kiro reports
// maxInputTokens per model in ListAvailableModels, which is authoritative and
// arrives without a code change when a new flagship ships; the version regex can
// only guess from the name and necessarily lags the product. The heuristic
// remains the fallback for models upstream said nothing about (and for every
// unit test that calls this without a populated registry).
func getContextWindowSize(model string) int {
	if declared, ok := declaredModelInputLimit(model); ok {
		return declared
	}
	if isLargeContextModel(model) {
		return 1_000_000
	}
	return 200_000
}

// claudeVersionExtractor matches "claude-<family>-<major>[.<minor>]" in dot or
// dash form and is used to classify 1M-window models by version.
//
// Two details are load-bearing:
//
//   - The minor group is OPTIONAL, so a bare-major flagship name such as
//     "claude-opus-5" is recognised. Previously the minor was required, so
//     "claude-opus-5" fell through to a substring fallback that matched nothing
//     and silently reported a 200K window — under-reporting a flagship model's
//     context by 5x.
//   - The minor group is capped at TWO digits with a trailing boundary, so a
//     dated snapshot ("claude-sonnet-4-20250514") does not parse as minor
//     20250514. With an unbounded minor that date compared as ">= 6" and
//     wrongly promoted a 200K model to a 1M window.
//
// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): both sides fixed the same bug
// (upstream's #140 — major-only ids classifying as 200K) by making the minor
// group optional. The fork's pattern is kept because it ALSO bounds the minor to
// two digits with a trailing boundary; upstream's unbounded `(\d+)` still
// misparses a dated snapshot id as a huge minor and over-promotes it to 1M.
var claudeVersionExtractor = regexp.MustCompile(`claude-(?:opus|sonnet|haiku)-(\d+)(?:[.-](\d{1,2})(?:\b|_|$))?`)

// isLargeContextModel reports whether a model uses the 1M-token context window.
//
// Policy: Claude 4.6+ within the 4.x line, and every major >= 5 regardless of
// minor. A bare major (no minor component) is treated as the ".0" release of
// that line, which is what makes "claude-opus-5" a 1M model. Defaulting an
// unknown NEWER major to the large window is deliberate: under-reporting the
// window makes clients compact too late and overflow the request, whereas
// over-reporting merely makes them compact slightly early.
func isLargeContextModel(model string) bool {
	m := strings.ToLower(model)
	if match := claudeVersionExtractor.FindStringSubmatch(m); match != nil {
		major, errMaj := strconv.Atoi(match[1])
		// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): the two sides express the
		// SAME classification with inverted control flow — upstream nests the whole
		// decision inside `if errMaj == nil`, the fork guards and falls through to
		// the equivalent ladder below (major>4 -> 1M, major<4 -> 200K, else minor
		// >= 6). The fork's shape is kept so the ladder stays un-nested; behaviour
		// is identical, including treating a bare major as ".0".
		if errMaj != nil {
			return false
		}
		// Any major beyond the 4.x line is a large-context model.
		if major > 4 {
			return true
		}
		if major < 4 {
			return false
		}
		// Within 4.x the minor decides. A bare "claude-opus-4" means 4.0.
		if match[2] == "" {
			return false
		}
		minor, errMin := strconv.Atoi(match[2])
		if errMin != nil {
			return false
		}
		return minor >= 6
	}
	// Fallback for non-standard identifiers that still carry a recognisable
	// 4.6+ version tag (e.g. a vendor-prefixed id the regex above misses).
	for _, tag := range []string{"4.6", "4-6", "4.7", "4-7", "4.8", "4-8", "4.9", "4-9"} {
		if strings.Contains(m, tag) {
			return true
		}
	}
	return false
}

func collectUsageMaps(v interface{}, out *[]map[string]interface{}) {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, child := range t {
			lk := strings.ToLower(k)
			if lk == "usage" || lk == "tokenusage" || lk == "token_usage" {
				if m, ok := child.(map[string]interface{}); ok {
					*out = append(*out, m)
				}
			}
			collectUsageMaps(child, out)
		}
	case []interface{}:
		for _, child := range t {
			collectUsageMaps(child, out)
		}
	}
}

func readTokenNumber(m map[string]interface{}, keys ...string) (int, bool) {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch n := v.(type) {
		case float64:
			return int(n), true
		case int:
			return n, true
		case int64:
			return int(n), true
		case json.Number:
			if parsed, err := n.Int64(); err == nil {
				return int(parsed), true
			}
		case string:
			if parsed, err := strconv.Atoi(n); err == nil {
				return parsed, true
			}
			if parsed, err := strconv.ParseFloat(n, 64); err == nil {
				return int(parsed), true
			}
		}
	}
	return 0, false
}

// ==================== Tool Use Handling ====================

type toolUseState struct {
	ToolUseID   string
	Name        string
	InputBuffer strings.Builder
	GeneratedID bool
}

func handleToolUseEvent(event map[string]interface{}, current *toolUseState, callback *KiroStreamCallback) *toolUseState {
	toolUseID := firstStringField(event, "toolUseId", "toolUseID", "tool_use_id", "id")
	name := firstStringField(event, "name", "toolName", "tool_name")
	isStop := firstBoolField(event, "stop", "isStop", "done")

	if toolUseID != "" && name != "" {
		if current == nil {
			current = &toolUseState{ToolUseID: toolUseID, Name: name}
		} else if current.ToolUseID != toolUseID {
			if current.GeneratedID && current.Name == name {
				current.ToolUseID = toolUseID
				current.GeneratedID = false
			} else {
				finishToolUse(current, callback)
				current = &toolUseState{ToolUseID: toolUseID, Name: name}
			}
		}
	} else if name != "" && current == nil {
		current = &toolUseState{ToolUseID: "toolu_" + uuid.New().String(), Name: name, GeneratedID: true}
	} else if name != "" && current != nil && current.Name != name {
		finishToolUse(current, callback)
		current = &toolUseState{ToolUseID: "toolu_" + uuid.New().String(), Name: name, GeneratedID: true}
	} else if toolUseID != "" && current != nil && current.ToolUseID != toolUseID {
		// A fragment carrying a DIFFERENT tool-use id but NO name still belongs to
		// another call. Every branch above requires name != "", so such a fragment
		// used to fall through to the input-accumulation block below and be spliced
		// into the currently-open call's argument buffer: call A's JSON was
		// corrupted with call B's arguments and call B was never emitted at all.
		// This is precisely the shape parallel tool use produces.
		//
		// A generated id means we never saw the real one, so adopt the id rather
		// than splitting a call that is actually the same one.
		if current.GeneratedID {
			current.ToolUseID = toolUseID
			current.GeneratedID = false
		} else {
			previousName := current.Name
			finishToolUse(current, callback)
			// The name is absent on this fragment; carry the previous call's name
			// forward so the new call is still emittable (finishToolUse drops a
			// state with an empty Name).
			current = &toolUseState{ToolUseID: toolUseID, Name: previousName}
		}
	}

	if current != nil {
		if input, ok := event["input"].(string); ok {
			current.InputBuffer.WriteString(input)
		} else if inputObj, ok := event["input"].(map[string]interface{}); ok {
			data, _ := json.Marshal(inputObj)
			current.InputBuffer.Reset()
			current.InputBuffer.Write(data)
		}
	}

	if isStop && current != nil {
		finishToolUse(current, callback)
		return nil
	}

	return current
}

func finishToolUse(state *toolUseState, callback *KiroStreamCallback) {
	if state == nil || state.Name == "" || callback == nil || callback.OnToolUse == nil {
		return
	}
	if state.ToolUseID == "" {
		state.ToolUseID = "toolu_" + uuid.New().String()
	}
	var input map[string]interface{}
	if state.InputBuffer.Len() > 0 {
		json.Unmarshal([]byte(state.InputBuffer.String()), &input)
	}
	if input == nil {
		input = make(map[string]interface{})
	}
	callback.OnToolUse(KiroToolUse{
		ToolUseID: state.ToolUseID,
		Name:      state.Name,
		Input:     input,
	})
}

func firstStringField(m map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func firstBoolField(m map[string]interface{}, keys ...string) bool {
	for _, key := range keys {
		if v, ok := m[key].(bool); ok {
			return v
		}
	}
	return false
}

// maxLoggedExceptionType bounds how much of an upstream-supplied failure label
// reaches the log.
//
// The label comes from an event-stream string header, which the 16-bit length
// field allows to be ~64 KiB. It is entirely upstream-controlled, so logging it
// verbatim let a hostile or malfunctioning upstream write 60 KB — including
// newlines, which can forge additional log lines — into the operator's log for
// every failed frame. 200 bytes is far more than any real exception type
// ("ThrottlingException", "ValidationException") and keeps the log readable.
const maxLoggedExceptionType = 200

// truncateForLog bounds a string for logging and marks it when shortened, so a
// truncated value is never mistaken for the whole one.
func truncateForLog(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}

// isUpstreamFailureFrame reports whether an event-stream frame signals an
// upstream failure rather than carrying content.
//
// The predicate deliberately matches the sibling Bedrock reader
// (bedrock_eventstream.go), which treats `:message-type` of "exception" OR
// "error", the same two values on `:event-type`, and any non-empty
// `:exception-type` as a failure. Matching only "exception" — as this did
// initially — left `:message-type: error` frames silently discarded on the Kiro
// path while the Bedrock path caught them, so the same upstream condition was
// observable on one surface and invisible on the other.
func isUpstreamFailureFrame(headers []byte, eventType string) bool {
	messageType := extractHeaderString(headers, ":message-type")
	if messageType == "exception" || messageType == "error" {
		return true
	}
	if eventType == "exception" || eventType == "error" {
		return true
	}
	return extractHeaderString(headers, ":exception-type") != ""
}

// upstreamFailureLabel returns the most specific available name for a failure
// frame, preferring the dedicated header over the generic ones. Same precedence
// as the Bedrock reader so the two surfaces report a given frame identically.
func upstreamFailureLabel(headers []byte, eventType string) string {
	if v := extractHeaderString(headers, ":exception-type"); v != "" {
		return v
	}
	if eventType != "" {
		return eventType
	}
	return extractHeaderString(headers, ":message-type")
}

// extractEventType extracts the event type string from AWS Event Stream message headers.
func extractEventType(headers []byte) string {
	offset := 0
	for offset < len(headers) {
		if offset >= len(headers) {
			break
		}
		nameLen := int(headers[offset])
		offset++
		if offset+nameLen > len(headers) {
			break
		}
		name := string(headers[offset : offset+nameLen])
		offset += nameLen
		if offset >= len(headers) {
			break
		}
		valueType := headers[offset]
		offset++

		if valueType == 7 { // String
			if offset+2 > len(headers) {
				break
			}
			valueLen := int(headers[offset])<<8 | int(headers[offset+1])
			offset += 2
			if offset+valueLen > len(headers) {
				break
			}
			value := string(headers[offset : offset+valueLen])
			offset += valueLen
			if name == ":event-type" {
				return value
			}
			continue
		}

		// Skip other value types by their fixed byte widths.
		skipSizes := map[byte]int{0: 0, 1: 0, 2: 1, 3: 2, 4: 4, 5: 8, 8: 8, 9: 16}
		if valueType == 6 {
			if offset+2 > len(headers) {
				break
			}
			l := int(headers[offset])<<8 | int(headers[offset+1])
			offset += 2 + l
		} else if skip, ok := skipSizes[valueType]; ok {
			offset += skip
		} else {
			break
		}
	}
	return ""
}
