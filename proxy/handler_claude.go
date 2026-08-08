package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

func validateClaudeRequestShape(req *ClaudeRequest) string {
	if len(req.Messages) == 0 {
		return "messages must not be empty"
	}
	if msg := validateClaudeThinkingConfig(req.Thinking, req.MaxTokens); msg != "" {
		return msg
	}

	hasUserContext := false
	lastRole := ""
	for _, msg := range req.Messages {
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			continue
		}
		lastRole = role
		if role != "user" {
			continue
		}

		text, images, toolResults := extractClaudeUserContent(msg.Content)
		if normalizeUserContent(text, len(images) > 0) != "" || len(toolResults) > 0 {
			hasUserContext = true
		}
	}

	// A trailing assistant message (reasoning/-thinking prefill or a replayed
	// final turn) is representable: the translator folds it into history and
	// generates against a continuation nudge / synthetic user turn. Only reject
	// shapes the translator cannot represent (no user context at all).
	if !hasUserContext {
		return "at least one non-empty user message is required"
	}
	_ = lastRole
	return ""
}
func validateClaudeThinkingConfig(thinking *ClaudeThinkingConfig, maxTokens int) string {
	if thinking == nil {
		return ""
	}

	kind := strings.ToLower(strings.TrimSpace(thinking.Type))
	switch kind {
	case "enabled":
		if maxTokens == 0 {
			return "thinking.type enabled cannot be used with max_tokens=0"
		}
		if thinking.BudgetTokens <= 0 {
			return "thinking.budget_tokens is required when thinking.type is enabled"
		}
		if thinking.BudgetTokens < 1024 {
			return "thinking.budget_tokens must be at least 1024"
		}
		if maxTokens > 0 && thinking.BudgetTokens >= maxTokens {
			return "thinking.budget_tokens must be less than max_tokens"
		}
	case "adaptive":
		if thinking.BudgetTokens != 0 {
			return "thinking.budget_tokens is not supported when thinking.type is adaptive"
		}
	case "disabled":
		if thinking.BudgetTokens != 0 {
			return "thinking.budget_tokens is not supported when thinking.type is disabled"
		}
	default:
		return "thinking.type must be one of: enabled, adaptive, disabled"
	}

	display := strings.ToLower(strings.TrimSpace(thinking.Display))
	if display != "" && display != "summarized" && display != "omitted" {
		return "thinking.display must be one of: summarized, omitted"
	}
	if kind == "disabled" && display != "" {
		return "thinking.display is not supported when thinking.type is disabled"
	}

	return ""
}

type claudeThinkingResponseOptions struct {
	Format      string
	OmitDisplay bool
}

func resolveClaudeThinkingResponseOptions(thinking *ClaudeThinkingConfig, defaultFormat string) claudeThinkingResponseOptions {
	opts := claudeThinkingResponseOptions{Format: defaultFormat}
	if opts.Format == "" {
		opts.Format = "thinking"
	}
	if thinking == nil {
		return opts
	}

	display := strings.ToLower(strings.TrimSpace(thinking.Display))
	switch display {
	case "summarized":
		opts.Format = "thinking"
	case "omitted":
		opts.Format = "thinking"
		opts.OmitDisplay = true
	}

	return opts
}

// authenticateForClaude runs authenticate and writes a Claude-style error on failure.
// Returns the request with the matched API key injected into context, or nil if auth failed.
func (h *Handler) authenticateForClaude(w http.ResponseWriter, r *http.Request) *http.Request {
	entry, err := h.authenticate(r)
	if err != nil {
		ae, _ := err.(*authError)
		if ae != nil && ae.notice {
			return withLimitNotice(r)
		}
		if ae == nil {
			ae = newAuthError(http.StatusUnauthorized, "authentication_error", err.Error())
		}
		if ae.retryAfter > 0 {
			w.Header().Set("Retry-After", strconv.FormatInt(ae.retryAfter, 10))
		}
		// Rejected before any account was chosen: log it so refused traffic is
		// visible instead of silently dying in middleware.
		h.recordRejection("claude", rejectedApiKeyLabel(r), ae.message, ae.status)
		h.sendClaudeError(w, ae.status, ae.code, ae.message)
		return nil
	}
	return withApiKeyContext(r, entry)
}

// handleCountTokens Token 计数（Claude Code 会调用）
func (h *Handler) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	body, err := readLimitedRequestBody(w, r)
	if err != nil {
		if isRequestBodyTooLarge(err) {
			h.sendClaudeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "Request body exceeds the configured limit")
			return
		}
		h.sendClaudeError(w, 400, "invalid_request_error", "Failed to read request body")
		return
	}

	var req ClaudeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", "Invalid JSON")
		return
	}
	if msg := validateClaudeThinkingConfig(req.Thinking, req.MaxTokens); msg != "" {
		h.sendClaudeError(w, 400, "invalid_request_error", msg)
		return
	}

	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := resolveClaudeThinkingMode(req.Model, req.Thinking, thinkingCfg.Suffix)
	req.Model = actualModel
	effectiveReq := cloneClaudeRequestForThinking(&req, thinking)

	estimatedTokens := estimateClaudeRequestInputTokens(effectiveReq)
	if estimatedTokens < 1 {
		estimatedTokens = 1
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]int{"input_tokens": estimatedTokens})
}

// noticeOutputTokens returns a rough output-token count for the limit-notice reply so the
// synthetic usage block looks plausible to clients. No billing is attached to it.
func noticeOutputTokens(msg string) int {
	n := len([]rune(msg)) / 4
	if n < 1 {
		n = 1
	}
	return n
}

// sendClaudeNotice returns the limit-notice text as a normal (HTTP 200) assistant reply
// in Claude shape. Used when a valid key is over-limit/disabled/expired so coding clients
// show the message in the chat window instead of erroring out. No upstream call, no billing.
func (h *Handler) sendClaudeNotice(w http.ResponseWriter, model string, stream bool, msg string) {
	outTok := noticeOutputTokens(msg)
	if !stream {
		resp := KiroToClaudeResponse(msg, "", false, nil, 1, outTok, model)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(resp)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		resp := KiroToClaudeResponse(msg, "", false, nil, 1, outTok, model)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(resp)
		return
	}

	msgID := "msg_" + uuid.New().String()
	h.sendSSE(w, flusher, "message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"content":       []interface{}{},
			"model":         model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]int{"input_tokens": 1, "output_tokens": 0},
		},
	})
	h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
		"type":          "content_block_start",
		"index":         0,
		"content_block": map[string]string{"type": "text", "text": ""},
	})
	h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]string{"type": "text_delta", "text": msg},
	})
	h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
		"type":  "content_block_stop",
		"index": 0,
	})
	h.sendSSE(w, flusher, "message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": "end_turn"},
		"usage": map[string]int{"input_tokens": 1, "output_tokens": outTok},
	})
	h.sendSSE(w, flusher, "message_stop", map[string]interface{}{
		"type": "message_stop",
	})
}

// handleClaudeMessages Claude API 处理
func (h *Handler) handleClaudeMessages(w http.ResponseWriter, r *http.Request) {
	h.handleClaudeMessagesInternal(w, r)
}
func (h *Handler) handleClaudeMessagesInternal(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	// 读取请求
	body, err := readLimitedRequestBody(w, r)
	if err != nil {
		if isRequestBodyTooLarge(err) {
			h.sendClaudeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "Request body exceeds the configured limit")
			return
		}
		h.sendClaudeError(w, 400, "invalid_request_error", "Failed to read request body")
		return
	}

	var req ClaudeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", "Invalid JSON: "+err.Error())
		return
	}
	if msg := validateClaudeRequestShape(&req); msg != "" {
		h.sendClaudeError(w, 400, "invalid_request_error", msg)
		return
	}

	// Valid-but-blocked key: render the limit-notice as a normal assistant reply.
	if limitNoticeRequested(r.Context()) {
		h.sendClaudeNotice(w, req.Model, req.Stream, config.GetLimitNoticeMessage())
		return
	}

	apiKeyID := apiKeyIDFromContext(r.Context())

	// 解析模型和 thinking 模式
	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := resolveClaudeThinkingMode(req.Model, req.Thinking, thinkingCfg.Suffix)
	// Apply global/per-key model override (ForceModel > per-key Model > client model).
	req.Model = applyModelOverride(actualModel, apiKeyID, thinkingCfg.Suffix)
	effectiveReq := cloneClaudeRequestForThinking(&req, thinking)
	thinkingResponseOpts := resolveClaudeThinkingResponseOptions(req.Thinking, thinkingCfg.ClaudeFormat)
	estimatedInputTokens := estimateClaudeRequestInputTokens(effectiveReq)
	cacheProfile := h.promptCache.BuildClaudeProfile(effectiveReq, estimatedInputTokens)

	// Pure native web_search: relay via Kiro MCP (generateAssistantResponse does not run it).
	if hasWebSearchTool(&req) {
		h.handleWebSearchRequest(w, &req, estimatedInputTokens, apiKeyID)
		return
	}

	// Mixed tools including native web_search: agentic loop digests web_search internally
	// and returns client tool_use blocks as-is.
	if hasWebSearchAmongTools(&req) {
		logger.Infof("[WebSearch] Mixed tools with native web_search, entering agentic loop")
		h.runWebSearchLoop(w, &req, thinking, estimatedInputTokens, apiKeyID)
		return
	}

	// 转换请求
	kiroPayload := ClaudeToKiro(&req, thinking)

	// Stream or non-stream.
	// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): apiKeyID is now resolved earlier
	// in this function (upstream's web-search branches need it), so this is a reuse
	// rather than a second := declaration.
	// forwarded marks a request that already passed through one Kiro-Go pool, so a
	// custom_api account cannot add another hop (loop guard, see forwardToUpstream).
	forwarded := r.Header.Get(forwardHeader) != ""
	if req.Stream {
		// Streaming returns here: the response cache below is non-stream only.
		h.handleClaudeStream(w, kiroPayload, req.Model, thinking, thinkingResponseOpts, estimatedInputTokens, cacheProfile, apiKeyID, body, forwarded)
		return
	}

	// F5: response cache (opt-in, exact-match, non-stream/tool-free/non-thinking).
	var cacheKey string
	if config.GetResponseCacheEnabled() && isCacheableClaudeRequest(&req, thinking) {
		if norm, err := json.Marshal(&req); err == nil {
			// Namespace the cache by API-key identity so one tenant's cached
			// response is never served to a different key.
			cacheKey = responseCacheKey(apiKeyID, "claude", norm)
			if cached, ok := h.responseCache.Get(cacheKey, time.Now().Unix()); ok {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.Header().Set("X-Kiro-Cache", "hit")
				_, _ = w.Write(cached)
				// A cache hit still consumes the tenant's quota: attribute the
				// cached response's usage to the key so cache hits cannot bypass
				// token/credit accounting or the RPM/TPM windows.
				in, out := usageFromCachedClaudeBody(cached)
				// account is nil and the duration is ~0 on purpose: a cache hit
				// is served from memory, so no upstream account did the work and
				// attributing one would misreport which credential was charged.
				h.recordSuccessForApiKey(apiKeyID, in, out, 0, req.Model, nil, "claude", time.Now())
				// ...and log it. Previously this path updated the counters
				// without emitting any record, so logCount could never be
				// reconciled against totalRequests.
				ctr := newTraceRecorder("claude", req.Model, false, apiKeyID)
				ctr.noteUsage(in, out, 0, 0, 0)
				ctr.markCacheHit()
				h.emitTrace(ctr, outcomeCacheHit, http.StatusOK)
				return
			}
		}
	}
	if cacheKey != "" {
		cw := newCaptureWriter(w)
		h.handleClaudeNonStream(cw, kiroPayload, req.Model, thinking, thinkingResponseOpts, estimatedInputTokens, cacheProfile, apiKeyID, body, forwarded)
		if cw.status == http.StatusOK && len(cw.buf) > 0 {
			h.responseCache.Set(cacheKey, cw.buf, config.GetResponseCacheTTLSeconds(), time.Now().Unix())
		}
		return
	}
	h.handleClaudeNonStream(w, kiroPayload, req.Model, thinking, thinkingResponseOpts, estimatedInputTokens, cacheProfile, apiKeyID, body, forwarded)
}

// handleClaudeStream Claude 流式响应
func (h *Handler) handleClaudeStream(w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, thinkingOpts claudeThinkingResponseOptions, estimatedInputTokens int, cacheProfile *promptCacheProfile, apiKeyID string, rawBody []byte, forwarded bool) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendClaudeError(w, 500, "api_error", "Streaming not supported")
		return
	}

	// 获取 thinking 输出格式配置
	thinkingFormat := thinkingOpts.Format

	// The trace recorder owns request-level timing from here on.
	tr := newTraceRecorder("claude", model, true, apiKeyID)
	startedAt := time.Now()
	msgID := "msg_" + uuid.New().String()
	startInputTokens := estimatedInputTokens
	excluded := make(map[string]bool)
	var lastErr error
	messageStarted := false
	var messageStartUsage promptCacheUsage

	ensureMessageStart := func() {
		if messageStarted {
			return
		}
		h.sendSSE(w, flusher, "message_start", map[string]interface{}{
			"type": "message_start",
			"message": map[string]interface{}{
				"id":            msgID,
				"type":          "message",
				"role":          "assistant",
				"content":       []interface{}{},
				"model":         model,
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage":         buildClaudeUsageMap(startInputTokens, 0, messageStartUsage, cacheProfile != nil),
			},
		})
		messageStarted = true
	}

	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		account := h.nextAccountForKey(apiKeyID, model, excluded)
		if account == nil {
			break
		}
		att := tr.beginAttempt(account)
		if err := h.ensureValidToken(account); err != nil {
			lastErr = err
			excluded[account.ID] = true
			tr.endAttempt(att, err)
			h.handleAccountFailure(account, err)
			continue
		}
		// Custom API accounts are transparent proxies to another Kiro-Go pool: forward
		// the raw request instead of translating to Kiro. A successful forward ends the
		// request; any pre-reply failure falls over to the next account like a Kiro error.
		if account.IsCustomApi() {
			// Already forwarded once: don't add another hop, and don't penalize this
			// healthy account (loop-guard is not a failure) — just skip it. The account
			// is excluded, so `attempt--` cannot loop forever; it only avoids spending a
			// real retry on an ineligible account.
			if forwarded {
				excluded[account.ID] = true
				attempt--
				continue
			}
			if fwdErr := h.forwardToUpstream(w, flusher, forwardParams{
				account: account, body: rawBody, endpoint: "anthropic", streaming: true,
				model: model, apiKeyID: apiKeyID, forwarded: forwarded,
				trace: tr, attempt: att,
			}); fwdErr != nil {
				lastErr = fwdErr
				excluded[account.ID] = true
				h.notePassthroughFailedAttempt(forwardParams{trace: tr, attempt: att}, fwdErr)
				h.handleAccountFailure(account, fwdErr)
				continue
			}
			return
		}
		// Native Bedrock accounts call the Bedrock Runtime invoke endpoint directly
		// and re-emit the native Anthropic events. Like custom_api this is a
		// transparent passthrough that ends the request on success; a pre-stream
		// failure falls over to the next account.
		if account.IsBedrock() {
			if bErr := h.invokeBedrockStream(w, flusher, forwardParams{
				account: account, body: rawBody, endpoint: "anthropic", streaming: true,
				model: model, apiKeyID: apiKeyID, forwarded: forwarded,
				trace: tr, attempt: att,
			}); bErr != nil {
				lastErr = bErr
				excluded[account.ID] = true
				h.notePassthroughFailedAttempt(forwardParams{trace: tr, attempt: att}, bErr)
				// A throttle-cooldown skip is advisory (per-model, short); don't
				// escalate it into an account-wide failure/cooldown.
				if !errors.Is(bErr, errBedrockThrottled) {
					h.handleAccountFailure(account, bErr)
				}
				continue
			}
			return
		}
		cacheUsage := h.promptCache.Compute(account.ID, cacheProfile)
		messageStartUsage = cacheUsage

		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var toolUses []KiroToolUse
		var nextContentIndex int
		var rawContentBuilder strings.Builder
		var rawThinkingBuilder strings.Builder
		activeBlockIndex := -1
		activeBlockType := ""

		closeActiveBlock := func() {
			if activeBlockIndex < 0 {
				return
			}
			h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
				"type":  "content_block_stop",
				"index": activeBlockIndex,
			})
			activeBlockIndex = -1
			activeBlockType = ""
		}

		startContentBlock := func(blockType string) {
			if activeBlockType == blockType {
				return
			}
			ensureMessageStart()
			closeActiveBlock()

			idx := nextContentIndex
			nextContentIndex++

			if blockType == "thinking" {
				h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": idx,
					"content_block": map[string]string{
						"type":     "thinking",
						"thinking": "",
					},
				})
			} else {
				h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": idx,
					"content_block": map[string]string{
						"type": "text",
						"text": "",
					},
				})
			}

			activeBlockIndex = idx
			activeBlockType = blockType
		}

		var textBuffer string
		var inThinkingBlock bool
		var dropTagThinking bool
		var thinkingSource thinkingStreamSource
		var thinkingStarted bool
		var eventThinkingOpen bool

		sendText := func(text string, thinkingState int) {
			if thinkingState == 0 {
				if text == "" {
					return
				}
				// First byte actually emitted to the client: this is the
				// time-to-first-token an operator cares about, distinct from
				// total request duration.
				tr.markFirstByte()
				startContentBlock("text")
				h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": activeBlockIndex,
					"delta": map[string]string{"type": "text_delta", "text": text},
				})
				return
			}

			if !thinking {
				return
			}

			switch thinkingFormat {
			case "think":
				var outputText string
				switch thinkingState {
				case 1:
					outputText = "<think>" + text
				case 2:
					outputText = text
				case 3:
					outputText = text + "</think>"
				}
				if outputText == "" {
					return
				}
				startContentBlock("text")
				h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": activeBlockIndex,
					"delta": map[string]string{"type": "text_delta", "text": outputText},
				})
			case "reasoning_content":
				if text == "" {
					return
				}
				startContentBlock("text")
				h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": activeBlockIndex,
					"delta": map[string]string{"type": "text_delta", "text": text},
				})
			default:
				if thinkingOpts.OmitDisplay {
					if thinkingState == 1 {
						startContentBlock("thinking")
						return
					}
					if thinkingState == 3 {
						if activeBlockType != "thinking" {
							startContentBlock("thinking")
						}
						closeActiveBlock()
					}
					return
				}
				if thinkingState == 3 && text == "" {
					if activeBlockType == "thinking" {
						closeActiveBlock()
					}
					return
				}
				if text != "" {
					startContentBlock("thinking")
					h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
						"type":  "content_block_delta",
						"index": activeBlockIndex,
						"delta": map[string]string{"type": "thinking_delta", "thinking": text},
					})
				}
				if thinkingState == 3 && activeBlockType == "thinking" {
					closeActiveBlock()
				}
			}
		}

		processClaudeText := func(text string, isThinking bool, forceFlush bool) {
			if isThinking && !thinking {
				return
			}

			if isThinking {
				if !allowReasoningSource(&thinkingSource) {
					return
				}
				if !thinkingStarted {
					sendText(text, 1)
					thinkingStarted = true
					eventThinkingOpen = true
				} else {
					sendText(text, 2)
				}
				return
			}

			if eventThinkingOpen {
				sendText("", 3)
				eventThinkingOpen = false
				thinkingStarted = false
			}

			textBuffer += text

			for {
				if !inThinkingBlock {
					thinkingStart := strings.Index(textBuffer, "<thinking>")
					if thinkingStart != -1 {
						if thinkingStart > 0 {
							sendText(textBuffer[:thinkingStart], 0)
						}
						textBuffer = textBuffer[thinkingStart+10:]
						inThinkingBlock = true
						dropTagThinking = !allowTagSource(&thinkingSource)
						thinkingStarted = false
					} else if forceFlush || len([]rune(textBuffer)) > 50 {
						runes := []rune(textBuffer)
						safeLen := len(runes)
						if !forceFlush {
							safeLen = max(0, len(runes)-15)
						}
						if safeLen > 0 {
							sendText(string(runes[:safeLen]), 0)
							textBuffer = string(runes[safeLen:])
						}
						break
					} else {
						break
					}
				} else {
					thinkingEnd := strings.Index(textBuffer, "</thinking>")
					if thinkingEnd != -1 {
						content := textBuffer[:thinkingEnd]
						if !dropTagThinking {
							if !thinkingStarted {
								sendText(content, 1)
								sendText("", 3)
							} else {
								sendText(content, 3)
							}
						}
						textBuffer = textBuffer[thinkingEnd+11:]
						inThinkingBlock = false
						dropTagThinking = false
						thinkingStarted = false
					} else if forceFlush {
						if textBuffer != "" {
							if !dropTagThinking {
								if !thinkingStarted {
									sendText(textBuffer, 1)
									sendText("", 3)
								} else {
									sendText(textBuffer, 3)
								}
							}
							textBuffer = ""
						}
						inThinkingBlock = false
						dropTagThinking = false
						thinkingStarted = false
						break
					} else {
						runes := []rune(textBuffer)
						if len(runes) > 20 {
							safeLen := len(runes) - 15
							if safeLen > 0 {
								if !dropTagThinking {
									if !thinkingStarted {
										sendText(string(runes[:safeLen]), 1)
										thinkingStarted = true
									} else {
										sendText(string(runes[:safeLen]), 2)
									}
								}
								textBuffer = string(runes[safeLen:])
							}
						}
						break
					}
				}
			}
		}

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if text == "" {
					return
				}
				if isThinking {
					rawThinkingBuilder.WriteString(text)
				} else {
					rawContentBuilder.WriteString(text)
				}
				processClaudeText(text, isThinking, false)
			},
			OnToolUse: func(tu KiroToolUse) {
				// A tool-call chunk is a first byte to the client just as much
				// as a text delta. Hooking only text emission left every
				// tool-call-only stream with no TTFB at all.
				tr.markFirstByte()
				processClaudeText("", false, true)
				rawContentBuilder.WriteString(tu.Name)
				if b, err := json.Marshal(tu.Input); err == nil {
					rawContentBuilder.Write(b)
				}

				toolUses = append(toolUses, tu)
				ensureMessageStart()
				closeActiveBlock()

				idx := nextContentIndex
				nextContentIndex++

				h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": idx,
					"content_block": map[string]interface{}{
						"type":  "tool_use",
						"id":    tu.ToolUseID,
						"name":  tu.Name,
						"input": map[string]interface{}{},
					},
				})

				inputJSON, _ := json.Marshal(tu.Input)
				h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": idx,
					"delta": map[string]interface{}{
						"type":         "input_json_delta",
						"partial_json": string(inputJSON),
					},
				})

				h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
					"type":  "content_block_stop",
					"index": idx,
				})
			},
			OnComplete: func(inTok, outTok int) {
				inputTokens = inTok
				outputTokens = outTok
			},
			OnCredits: func(c float64) {
				credits = c
			},
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
			},
		}

		// Marshal the outbound payload BEFORE dispatch: CallKiroAPI mutates it
		// in place per endpoint (Origin, ProfileArn), so capturing afterwards
		// would record post-dispatch state rather than what was sent. No-op
		// unless the capture mode allows bodies.
		tr.noteRequestPayload(payload)
		var diag KiroCallDiagnostics
		err := CallKiroAPIWithDiagnostics(account, payload, callback, &diag)
		tr.applyDiagnostics(att, &diag)
		if err != nil {
			lastErr = err
			excluded[account.ID] = true
			tr.endAttempt(att, err)
			h.handleAccountFailure(account, err)
			if !messageStarted {
				continue
			}
			h.recordFailureForApiKey(apiKeyID, "claude", model, 0, err.Error(), startedAt)
			// Classify the error type from the authoritative upstream status.
			//
			// This was hardcoded to "api_error" for every failure, while the
			// OpenAI stream classified the SAME error via
			// errorTypeForOpenAIStatus. That asymmetry is client-visible and
			// consequential: an Anthropic consumer keys its retry policy off
			// error.type, so a rate limit reported as api_error invites an
			// immediate retry into an exhausted account instead of a backoff,
			// and a revoked credential reported as api_error looks transient so
			// the client retries forever instead of surfacing "re-authenticate".
			midStreamStatus := statusForUpstreamError(err)
			h.sendSSE(w, flusher, "error", map[string]interface{}{
				"type": "error",
				"error": map[string]string{
					"type":    claudeErrorTypeForStatus(midStreamStatus),
					"message": err.Error(),
				},
			})
			// Terminate the SSE message properly. Emitting `error` and returning
			// left any open content_block unclosed and never sent message_delta or
			// message_stop, so a client that had already received message_start
			// was left with a half-open message: strict Anthropic SSE consumers
			// either hang waiting for the close or raise a protocol error instead
			// of surfacing the upstream failure. stop_reason is reported as
			// "error" so the client can tell this apart from a normal end_turn.
			closeActiveBlock()
			h.sendSSE(w, flusher, "message_delta", map[string]interface{}{
				"type": "message_delta",
				"delta": map[string]interface{}{
					"stop_reason": "error",
				},
				"usage": buildClaudeUsageMap(inputTokens, outputTokens, cacheUsage, cacheProfile != nil),
			})
			h.sendSSE(w, flusher, "message_stop", map[string]interface{}{
				"type": "message_stop",
			})
			return
		}
		tr.endAttempt(att, nil)

		processClaudeText("", false, true)
		if eventThinkingOpen {
			sendText("", 3)
		}
		closeActiveBlock()

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		// Re-anchor the cache split to the real upstream input total so
		// input+creation+read stays consistent (cacheUsage was computed against
		// the pre-call token estimate).
		if cacheProfile != nil && realInputTokens > 0 {
			cacheUsage = cacheUsage.splitAgainstTotal(cacheProfile.TotalInputTokens, inputTokens)
		}
		outputContent, extractedReasoning := extractThinkingFromContent(rawContentBuilder.String())
		thinkingOutput := rawThinkingBuilder.String()
		if thinking && thinkingOutput == "" && extractedReasoning != "" {
			thinkingOutput = extractedReasoning
		}
		if !thinking {
			thinkingOutput = ""
		}
		outputTokens = estimateClaudeOutputTokens(outputContent, thinkingOutput, toolUses)

		h.recordSuccessForApiKey(apiKeyID, inputTokens, outputTokens, credits, model, account, "claude", startedAt)
		h.pool.RecordSuccess(account.ID)
		h.pool.RecordLatency(account.ID, float64(time.Since(tr.startedAt).Milliseconds()))
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		h.promptCache.Update(account.ID, cacheProfile)
		logSuspiciousReq("claude", model, inputTokens, outputTokens, len(toolUses) > 0)

		stopReason := "end_turn"
		if len(toolUses) > 0 {
			stopReason = "tool_use"
		}
		tr.noteUsage(inputTokens, outputTokens, cacheUsage.CacheReadInputTokens, cacheUsage.CacheCreationInputTokens, credits)
		tr.noteResponseShape(stopReason, model, len(toolUses))
		tr.noteResponseText(outputContent)
		h.emitTrace(tr, outcomeSuccess, http.StatusOK)

		ensureMessageStart()
		h.sendSSE(w, flusher, "message_delta", map[string]interface{}{
			"type": "message_delta",
			"delta": map[string]interface{}{
				"stop_reason": stopReason,
			},
			"usage": buildClaudeUsageMap(inputTokens, outputTokens, cacheUsage, cacheProfile != nil),
		})

		h.sendSSE(w, flusher, "message_stop", map[string]interface{}{
			"type": "message_stop",
		})
		return
	}

	if lastErr == nil {
		h.recordFailureForApiKey(apiKeyID, "claude", model, 503, "No available accounts", startedAt)
		h.sendClaudeError(w, 503, "api_error", "No available accounts")
		return
	}

	status := statusForUpstreamError(lastErr)
	applyRetryAfterHeader(w, lastErr)
	h.recordFailureForApiKey(apiKeyID, "claude", model, status, lastErr.Error(), startedAt)
	h.sendClaudeError(w, status, "api_error", lastErr.Error())
}
func (h *Handler) sendSSE(w http.ResponseWriter, flusher http.Flusher, event string, data interface{}) {
	jsonData, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(jsonData))
	flusher.Flush()
}

// handleClaudeNonStream Claude 非流式响应
func (h *Handler) handleClaudeNonStream(w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, thinkingOpts claudeThinkingResponseOptions, estimatedInputTokens int, cacheProfile *promptCacheProfile, apiKeyID string, rawBody []byte, forwarded bool) {
	startedAt := time.Now()
	excluded := make(map[string]bool)
	var lastErr error
	// The trace recorder owns request-level timing from here on.
	tr := newTraceRecorder("claude", model, false, apiKeyID)

	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		account := h.nextAccountForKey(apiKeyID, model, excluded)
		if account == nil {
			break
		}
		att := tr.beginAttempt(account)
		if err := h.ensureValidToken(account); err != nil {
			lastErr = err
			excluded[account.ID] = true
			tr.endAttempt(att, err)
			h.handleAccountFailure(account, err)
			continue
		}
		// Custom API accounts proxy to another Kiro-Go pool (see handleClaudeStream).
		if account.IsCustomApi() {
			// Already forwarded once: don't add another hop, and don't penalize this
			// healthy account (loop-guard is not a failure) — just skip it. The account
			// is excluded, so `attempt--` cannot loop forever; it only avoids spending a
			// real retry on an ineligible account.
			if forwarded {
				excluded[account.ID] = true
				attempt--
				continue
			}
			if fwdErr := h.forwardToUpstream(w, nil, forwardParams{
				account: account, body: rawBody, endpoint: "anthropic", streaming: false,
				model: model, apiKeyID: apiKeyID, forwarded: forwarded,
				trace: tr, attempt: att,
			}); fwdErr != nil {
				lastErr = fwdErr
				excluded[account.ID] = true
				h.notePassthroughFailedAttempt(forwardParams{trace: tr, attempt: att}, fwdErr)
				h.handleAccountFailure(account, fwdErr)
				continue
			}
			return
		}
		// Native Bedrock non-streaming invoke (see streaming counterpart above).
		if account.IsBedrock() {
			if bErr := h.invokeBedrockNonStream(w, forwardParams{
				account: account, body: rawBody, endpoint: "anthropic", streaming: false,
				model: model, apiKeyID: apiKeyID, forwarded: forwarded,
				trace: tr, attempt: att,
			}); bErr != nil {
				lastErr = bErr
				excluded[account.ID] = true
				h.notePassthroughFailedAttempt(forwardParams{trace: tr, attempt: att}, bErr)
				if !errors.Is(bErr, errBedrockThrottled) {
					h.handleAccountFailure(account, bErr)
				}
				continue
			}
			return
		}
		cacheUsage := h.promptCache.Compute(account.ID, cacheProfile)

		var content string
		var thinkingContent string
		var toolUses []KiroToolUse
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if isThinking {
					thinkingContent += text
				} else {
					content += text
				}
			},
			OnToolUse: func(tu KiroToolUse) {
				toolUses = append(toolUses, tu)
			},
			OnComplete: func(inTok, outTok int) {
				inputTokens = inTok
				outputTokens = outTok
			},
			OnCredits: func(c float64) {
				credits = c
			},
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
			},
		}

		// Marshal the outbound payload BEFORE dispatch: CallKiroAPI mutates it
		// in place per endpoint (Origin, ProfileArn), so capturing afterwards
		// would record post-dispatch state rather than what was sent. No-op
		// unless the capture mode allows bodies.
		tr.noteRequestPayload(payload)
		var diag KiroCallDiagnostics
		err := CallKiroAPIWithDiagnostics(account, payload, callback, &diag)
		tr.applyDiagnostics(att, &diag)
		if err != nil {
			lastErr = err
			excluded[account.ID] = true
			tr.endAttempt(att, err)
			h.handleAccountFailure(account, err)
			continue
		}
		tr.endAttempt(att, nil)

		thinkingFormat := thinkingOpts.Format
		finalContent, extractedReasoning := extractThinkingFromContent(content)
		rawThinkingContent := thinkingContent
		if thinking && rawThinkingContent == "" && extractedReasoning != "" {
			rawThinkingContent = extractedReasoning
		}
		if !thinking {
			rawThinkingContent = ""
		}
		// Defensive: withhold reasoning that is only an upstream redaction
		// placeholder ("...") when suppression is enabled (real CoT passes through).
		if config.GetThinkingConfig().SuppressPlaceholderReasoning && isPlaceholderReasoning(rawThinkingContent) {
			rawThinkingContent = ""
		}

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		// Re-anchor the cache split to the real upstream input total so
		// input+creation+read stays consistent (cacheUsage was computed against
		// the pre-call token estimate).
		if cacheProfile != nil && realInputTokens > 0 {
			cacheUsage = cacheUsage.splitAgainstTotal(cacheProfile.TotalInputTokens, inputTokens)
		}
		outputTokens = estimateClaudeOutputTokens(finalContent, rawThinkingContent, toolUses)

		h.recordSuccessForApiKey(apiKeyID, inputTokens, outputTokens, credits, model, account, "claude", startedAt)
		h.pool.RecordSuccess(account.ID)
		h.pool.RecordLatency(account.ID, float64(time.Since(tr.startedAt).Milliseconds()))
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		h.promptCache.Update(account.ID, cacheProfile)
		stopReason := "end_turn"
		if len(toolUses) > 0 {
			stopReason = "tool_use"
		}
		tr.noteUsage(inputTokens, outputTokens, cacheUsage.CacheReadInputTokens, cacheUsage.CacheCreationInputTokens, credits)
		tr.noteResponseShape(stopReason, model, len(toolUses))
		tr.noteResponseText(finalContent)
		h.emitTrace(tr, outcomeSuccess, http.StatusOK)
		logSuspiciousReq("claude", model, inputTokens, outputTokens, len(toolUses) > 0)

		responseThinkingContent := rawThinkingContent
		includeEmptyThinkingBlock := thinking && thinkingOpts.OmitDisplay && rawThinkingContent != ""
		if includeEmptyThinkingBlock {
			responseThinkingContent = ""
		}

		if thinking && responseThinkingContent != "" {
			switch thinkingFormat {
			case "think":
				finalContent = "<think>" + responseThinkingContent + "</think>" + finalContent
				responseThinkingContent = ""
			case "reasoning_content":
				finalContent = responseThinkingContent + finalContent
				responseThinkingContent = ""
			default:
			}
		}

		resp := KiroToClaudeResponse(finalContent, responseThinkingContent, includeEmptyThinkingBlock, toolUses, inputTokens, outputTokens, model)
		resp.Usage.InputTokens = billedClaudeInputTokens(inputTokens, cacheUsage)
		resp.Usage.CacheCreationInputTokens = cacheUsage.CacheCreationInputTokens
		resp.Usage.CacheReadInputTokens = cacheUsage.CacheReadInputTokens
		if cacheProfile != nil {
			resp.Usage.CacheCreation = &ClaudeCacheCreationUsage{
				Ephemeral5mInputTokens: cacheUsage.CacheCreation5mInputTokens,
				Ephemeral1hInputTokens: cacheUsage.CacheCreation1hInputTokens,
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(resp)
		return
	}

	if lastErr == nil {
		h.recordFailureForApiKey(apiKeyID, "claude", model, 503, "No available accounts", startedAt)
		h.sendClaudeError(w, 503, "api_error", "No available accounts")
		return
	}

	status := statusForUpstreamError(lastErr)
	applyRetryAfterHeader(w, lastErr)
	h.recordFailureForApiKey(apiKeyID, "claude", model, status, lastErr.Error(), startedAt)
	h.sendClaudeError(w, status, "api_error", lastErr.Error())
}
func (h *Handler) sendClaudeError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": message,
		},
	})
}
