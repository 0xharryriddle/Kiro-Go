package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"kiro-go/config"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

func validateOpenAIRequestShape(req *OpenAIRequest) string {
	if len(req.Messages) == 0 {
		return "messages must not be empty"
	}

	hasNonSystem := false
	hasUserContext := false
	lastRole := ""
	for _, msg := range req.Messages {
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			continue
		}
		if role != "system" {
			hasNonSystem = true
			lastRole = role
		}

		if role != "user" {
			continue
		}
		text, images := extractOpenAIUserContent(msg.Content)
		if normalizeUserContent(text, len(images) > 0) != "" {
			hasUserContext = true
		}
	}

	if !hasNonSystem {
		return "at least one non-system message is required"
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

// authenticateForOpenAI runs authenticate and writes an OpenAI-style error on failure.
func (h *Handler) authenticateForOpenAI(w http.ResponseWriter, r *http.Request) *http.Request {
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
		h.recordRejection("openai", rejectedApiKeyLabel(r), ae.message, ae.status)
		h.sendOpenAIError(w, ae.status, ae.code, ae.message)
		return nil
	}
	return withApiKeyContext(r, entry)
}

// sendOpenAINotice returns the limit-notice text as a normal (HTTP 200) assistant reply
// in OpenAI chat-completions shape. No upstream call, no billing.
func (h *Handler) sendOpenAINotice(w http.ResponseWriter, model string, stream bool, msg string) {
	outTok := noticeOutputTokens(msg)
	if !stream {
		thinkingFormat := config.GetThinkingConfig().OpenAIFormat
		resp := KiroToOpenAIResponseWithReasoning(msg, "", nil, 1, outTok, model, thinkingFormat)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(resp)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		thinkingFormat := config.GetThinkingConfig().OpenAIFormat
		resp := KiroToOpenAIResponseWithReasoning(msg, "", nil, 1, outTok, model, thinkingFormat)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(resp)
		return
	}

	chatID := "chatcmpl-" + uuid.New().String()
	created := time.Now().Unix()
	deltaChunk := map[string]interface{}{
		"id":      chatID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]interface{}{{
			"index":         0,
			"delta":         map[string]string{"content": msg},
			"finish_reason": nil,
		}},
	}
	data, _ := json.Marshal(deltaChunk)
	fmt.Fprintf(w, "data: %s\n\n", string(data))

	finalChunk := map[string]interface{}{
		"id":      chatID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]interface{}{{
			"index":         0,
			"delta":         map[string]interface{}{},
			"finish_reason": "stop",
		}},
		"usage": map[string]int{
			"prompt_tokens":     1,
			"completion_tokens": outTok,
			"total_tokens":      1 + outTok,
		},
	}
	data, _ = json.Marshal(finalChunk)
	fmt.Fprintf(w, "data: %s\n\n", string(data))
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// handleOpenAIChat OpenAI API 处理
func (h *Handler) handleOpenAIChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	body, err := readLimitedRequestBody(w, r)
	if err != nil {
		if isRequestBodyTooLarge(err) {
			h.sendOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "Request body exceeds the configured limit")
			return
		}
		h.sendOpenAIError(w, 400, "invalid_request_error", "Failed to read request body")
		return
	}

	var req OpenAIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", "Invalid JSON")
		return
	}
	if msg := validateOpenAIRequestShape(&req); msg != "" {
		h.sendOpenAIError(w, 400, "invalid_request_error", msg)
		return
	}

	// Valid-but-blocked key: render the limit-notice as a normal assistant reply.
	if limitNoticeRequested(r.Context()) {
		h.sendOpenAINotice(w, req.Model, req.Stream, config.GetLimitNoticeMessage())
		return
	}

	apiKeyID := apiKeyIDFromContext(r.Context())

	// 解析模型和 thinking 模式
	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := ParseModelAndThinking(req.Model, thinkingCfg.Suffix)
	// Apply global/per-key model override (ForceModel > per-key Model > client model).
	req.Model = applyModelOverride(actualModel, apiKeyID, thinkingCfg.Suffix)
	estimatedInputTokens := estimateOpenAIRequestInputTokens(&req)

	kiroPayload := OpenAIToKiro(&req, thinking)

	// forwarded marks a request that already passed through one Kiro-Go pool, so a
	// custom_api account cannot add another hop (loop guard, see forwardToUpstream).
	forwarded := r.Header.Get(forwardHeader) != ""
	if req.Stream {
		// Streaming returns here: the response cache below is non-stream only.
		h.handleOpenAIStream(w, kiroPayload, req.Model, thinking, estimatedInputTokens, apiKeyID, body, forwarded)
		return
	}

	// F5: response cache (opt-in, exact-match, non-stream/tool-free/non-thinking).
	// Serve a fresh cached body on hit; otherwise capture the response and store it.
	var cacheKey string
	if config.GetResponseCacheEnabled() && isCacheableOpenAIRequest(&req, thinking) {
		if norm, err := json.Marshal(&req); err == nil {
			// Namespace the cache by API-key identity so one tenant's cached
			// response is never served to a different key.
			cacheKey = responseCacheKey(apiKeyID, "openai", norm)
			if cached, ok := h.responseCache.Get(cacheKey, time.Now().Unix()); ok {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.Header().Set("X-Kiro-Cache", "hit")
				_, _ = w.Write(cached)
				// A cache hit still consumes the tenant's quota: attribute the
				// cached response's usage to the key so cache hits cannot bypass
				// token/credit accounting or the RPM/TPM windows.
				in, out := usageFromCachedOpenAIBody(cached)
				// See the Claude cache-hit site: nil account, ~0 duration.
				h.recordSuccessForApiKey(apiKeyID, in, out, 0, req.Model, nil, "openai", time.Now())
				return
			}
		}
	}
	if cacheKey != "" {
		cw := newCaptureWriter(w)
		h.handleOpenAINonStream(cw, kiroPayload, req.Model, thinking, estimatedInputTokens, apiKeyID, body, forwarded)
		if cw.status == http.StatusOK && len(cw.buf) > 0 {
			h.responseCache.Set(cacheKey, cw.buf, config.GetResponseCacheTTLSeconds(), time.Now().Unix())
		}
		return
	}
	h.handleOpenAINonStream(w, kiroPayload, req.Model, thinking, estimatedInputTokens, apiKeyID, body, forwarded)
}

// handleOpenAIStream OpenAI 流式响应
func (h *Handler) handleOpenAIStream(w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, estimatedInputTokens int, apiKeyID string, rawBody []byte, forwarded bool) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendOpenAIError(w, 500, "server_error", "Streaming not supported")
		return
	}

	// 获取 thinking 输出格式配置
	thinkingFormat := config.GetThinkingConfig().OpenAIFormat

	chatID := "chatcmpl-" + uuid.New().String()
	startedAt := time.Now()
	excluded := make(map[string]bool)
	var lastErr error
	// The trace recorder owns request-level timing from here on.
	tr := newTraceRecorder("openai", model, true, apiKeyID)

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

		// Native Bedrock accounts serve the OpenAI wire format by converting the
		// request to Anthropic Messages, invoking Bedrock, and converting the
		// Anthropic SSE back to OpenAI chunks. Same passthrough/failover contract
		// as custom_api: success ends the request; a pre-stream error fails over.
		if account.IsBedrock() {
			if bErr := h.invokeBedrockOpenAIStream(w, flusher, forwardParams{
				account: account, body: rawBody, endpoint: "openai", streaming: true,
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
			if fwdErr := h.forwardToUpstream(w, flusher, forwardParams{
				account: account, body: rawBody, endpoint: "openai", streaming: true,
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

		var toolCalls []ToolCall
		var toolCallIndex int
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var rawContentBuilder strings.Builder
		var rawReasoningBuilder strings.Builder
		var textBuffer string
		var inThinkingBlock bool
		var dropTagThinking bool
		var thinkingSource thinkingStreamSource
		var thinkingStarted bool
		var eventThinkingOpen bool
		responseStarted := false

		sendChunk := func(content string, thinkingState int) {
			if content == "" && thinkingState == 2 {
				return
			}
			// First byte actually emitted to the client: this is the
			// time-to-first-token an operator cares about, distinct from
			// total request duration.
			tr.markFirstByte()

			var chunk map[string]interface{}

			if thinkingState > 0 {
				if !thinking {
					return
				}
				switch thinkingFormat {
				case "thinking":
					var text string
					switch thinkingState {
					case 1:
						text = "<thinking>" + content
					case 2:
						text = content
					case 3:
						text = content + "</thinking>"
					}
					if text == "" {
						return
					}
					chunk = map[string]interface{}{
						"id":      chatID,
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   model,
						"choices": []map[string]interface{}{{
							"index":         0,
							"delta":         map[string]string{"content": text},
							"finish_reason": nil,
						}},
					}
				case "think":
					var text string
					switch thinkingState {
					case 1:
						text = "<think>" + content
					case 2:
						text = content
					case 3:
						text = content + "</think>"
					}
					if text == "" {
						return
					}
					chunk = map[string]interface{}{
						"id":      chatID,
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   model,
						"choices": []map[string]interface{}{{
							"index":         0,
							"delta":         map[string]string{"content": text},
							"finish_reason": nil,
						}},
					}
				default:
					if content == "" {
						return
					}
					chunk = map[string]interface{}{
						"id":      chatID,
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   model,
						"choices": []map[string]interface{}{{
							"index":         0,
							"delta":         map[string]string{"reasoning_content": content},
							"finish_reason": nil,
						}},
					}
				}
			} else {
				if content == "" {
					return
				}
				chunk = map[string]interface{}{
					"id":      chatID,
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   model,
					"choices": []map[string]interface{}{{
						"index":         0,
						"delta":         map[string]string{"content": content},
						"finish_reason": nil,
					}},
				}
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", string(data))
			flusher.Flush()
			responseStarted = true
		}

		processText := func(text string, isThinking bool, forceFlush bool) {
			if isThinking && !thinking {
				return
			}

			if isThinking {
				if !allowReasoningSource(&thinkingSource) {
					return
				}
				if !thinkingStarted {
					sendChunk(text, 1)
					thinkingStarted = true
					eventThinkingOpen = true
				} else {
					sendChunk(text, 2)
				}
				return
			}

			if eventThinkingOpen {
				sendChunk("", 3)
				eventThinkingOpen = false
				thinkingStarted = false
			}

			textBuffer += text

			for {
				if !inThinkingBlock {
					thinkingStart := strings.Index(textBuffer, "<thinking>")
					if thinkingStart != -1 {
						if thinkingStart > 0 {
							sendChunk(textBuffer[:thinkingStart], 0)
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
							sendChunk(string(runes[:safeLen]), 0)
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
								sendChunk(content, 1)
								sendChunk("", 3)
							} else {
								sendChunk(content, 3)
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
									sendChunk(textBuffer, 1)
									sendChunk("", 3)
								} else {
									sendChunk(textBuffer, 3)
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
										sendChunk(string(runes[:safeLen]), 1)
										thinkingStarted = true
									} else {
										sendChunk(string(runes[:safeLen]), 2)
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
					rawReasoningBuilder.WriteString(text)
				} else {
					rawContentBuilder.WriteString(text)
				}
				processText(text, isThinking, false)
			},
			OnToolUse: func(tu KiroToolUse) {
				// A tool-call chunk is a first byte to the client just as much
				// as a text delta. Hooking only text emission left every
				// tool-call-only stream with no TTFB at all.
				tr.markFirstByte()
				processText("", false, true)

				args, _ := json.Marshal(tu.Input)
				rawContentBuilder.WriteString(tu.Name)
				rawContentBuilder.Write(args)
				tc := ToolCall{ID: tu.ToolUseID, Type: "function"}
				tc.Function.Name = tu.Name
				tc.Function.Arguments = string(args)
				toolCalls = append(toolCalls, tc)

				chunk := map[string]interface{}{
					"id":      chatID,
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   model,
					"choices": []map[string]interface{}{{
						"index": 0,
						"delta": map[string]interface{}{
							"tool_calls": []map[string]interface{}{{
								"index": toolCallIndex,
								"id":    tu.ToolUseID,
								"type":  "function",
								"function": map[string]string{
									"name":      tu.Name,
									"arguments": string(args),
								},
							}},
						},
						"finish_reason": nil,
					}},
				}
				toolCallIndex++
				data, _ := json.Marshal(chunk)
				fmt.Fprintf(w, "data: %s\n\n", string(data))
				flusher.Flush()
				responseStarted = true
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
			if !responseStarted {
				continue
			}
			h.recordFailureForApiKey(apiKeyID, "openai", model, 0, err.Error(), startedAt)
			// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): the fork's client-facing
			// terminator is restored here. The merge kept only the bookkeeping call
			// above and then `return`ed, which is the exact defect the fork had
			// already fixed: the stream simply stopped writing, so a client that had
			// already received content saw the connection end with no error payload,
			// no finish_reason and no [DONE] sentinel — a partial answer that looked
			// COMPLETE. Emit an explicit error chunk, a finish_reason and [DONE] so
			// the failure is unambiguous (mirrors handleClaudeStream's error SSE and
			// handleResponsesStream's response.failed).
			errChunk := map[string]interface{}{
				"id":      chatID,
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   model,
				"choices": []map[string]interface{}{{
					"index":         0,
					"delta":         map[string]interface{}{},
					"finish_reason": "error",
				}},
				// The error object tells the client WHY the stream ended; a bare
				// finish_reason:"error" is indistinguishable from a normal stop
				// for clients that only read the delta.
				"error": map[string]string{
					"type":    errorTypeForOpenAIStatus(statusForUpstreamError(err)),
					"message": err.Error(),
				},
			}
			errData, _ := json.Marshal(errChunk)
			fmt.Fprintf(w, "data: %s\n\n", string(errData))
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}
		tr.endAttempt(att, nil)

		processText("", false, true)
		if eventThinkingOpen {
			sendChunk("", 3)
		}

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		outputContent, extractedReasoning := extractThinkingFromContent(rawContentBuilder.String())
		reasoningOutput := rawReasoningBuilder.String()
		if thinking && reasoningOutput == "" && extractedReasoning != "" {
			reasoningOutput = extractedReasoning
		}
		if !thinking {
			reasoningOutput = ""
		}
		outputTokens = estimateApproxTokens(outputContent) + estimateApproxTokens(reasoningOutput)
		for _, tc := range toolCalls {
			outputTokens += estimateApproxTokens(tc.Function.Name)
			outputTokens += estimateApproxTokens(tc.Function.Arguments)
		}

		h.recordSuccessForApiKey(apiKeyID, inputTokens, outputTokens, credits, model, account, "openai", startedAt)
		h.pool.RecordSuccess(account.ID)
		h.pool.RecordLatency(account.ID, float64(time.Since(tr.startedAt).Milliseconds()))
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		logSuspiciousReq("openai", model, inputTokens, outputTokens, len(toolCalls) > 0)

		finishReason := "stop"
		if len(toolCalls) > 0 {
			finishReason = "tool_calls"
		}
		tr.noteUsage(inputTokens, outputTokens, 0, 0, credits)
		tr.noteResponseShape(finishReason, model, len(toolCalls))
		tr.noteResponseText(outputContent)
		h.emitTrace(tr, outcomeSuccess, http.StatusOK)

		chunk := map[string]interface{}{
			"id":      chatID,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]interface{}{{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": finishReason,
			}},
			"usage": map[string]int{
				"prompt_tokens":     inputTokens,
				"completion_tokens": outputTokens,
				"total_tokens":      inputTokens + outputTokens,
			},
		}
		data, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", string(data))
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}

	if lastErr == nil {
		h.sendOpenAIError(w, 503, "server_error", "No available accounts")
		return
	}

	h.recordFailure()
	status := statusForUpstreamError(lastErr)
	applyRetryAfterHeader(w, lastErr)
	h.sendOpenAIError(w, status, errorTypeForOpenAIStatus(status), lastErr.Error())
}

// handleOpenAINonStream OpenAI 非流式响应
func (h *Handler) handleOpenAINonStream(w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, estimatedInputTokens int, apiKeyID string, rawBody []byte, forwarded bool) {
	startedAt := time.Now()
	excluded := make(map[string]bool)
	var lastErr error
	// The trace recorder owns request-level timing from here on.
	tr := newTraceRecorder("openai", model, false, apiKeyID)

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

		// Native Bedrock accounts serve OpenAI by converting to Anthropic, invoking
		// Bedrock, and converting the Anthropic JSON response back to an OpenAI
		// chat.completion. Success ends the request; a pre-reply error fails over.
		if account.IsBedrock() {
			if bErr := h.invokeBedrockOpenAINonStream(w, forwardParams{
				account: account, body: rawBody, endpoint: "openai", streaming: false,
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
				account: account, body: rawBody, endpoint: "openai", streaming: false,
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

		var content string
		var reasoningContent string
		var toolUses []KiroToolUse
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if isThinking {
					reasoningContent += text
				} else {
					content += text
				}
			},
			OnToolUse:  func(tu KiroToolUse) { toolUses = append(toolUses, tu) },
			OnComplete: func(inTok, outTok int) { inputTokens = inTok; outputTokens = outTok },
			OnCredits:  func(c float64) { credits = c },
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

		finalContent, extractedReasoning := extractThinkingFromContent(content)
		if thinking && reasoningContent == "" && extractedReasoning != "" {
			reasoningContent = extractedReasoning
		} else if !thinking {
			reasoningContent = ""
		}
		// Defensive: reasoning may also arrive embedded as <thinking>...</thinking>
		// (a different channel than reasoningContentEvent). Drop it too when it is
		// only an upstream redaction placeholder and suppression is enabled.
		if config.GetThinkingConfig().SuppressPlaceholderReasoning && isPlaceholderReasoning(reasoningContent) {
			reasoningContent = ""
		}

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		outputTokens = estimateOpenAIOutputTokens(finalContent, reasoningContent, toolUses)

		h.recordSuccessForApiKey(apiKeyID, inputTokens, outputTokens, credits, model, account, "openai", startedAt)
		h.pool.RecordSuccess(account.ID)
		h.pool.RecordLatency(account.ID, float64(time.Since(tr.startedAt).Milliseconds()))
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		finishReason := "stop"
		if len(toolUses) > 0 {
			finishReason = "tool_calls"
		}
		tr.noteUsage(inputTokens, outputTokens, 0, 0, credits)
		tr.noteResponseShape(finishReason, model, len(toolUses))
		tr.noteResponseText(finalContent)
		h.emitTrace(tr, outcomeSuccess, http.StatusOK)
		logSuspiciousReq("openai", model, inputTokens, outputTokens, len(toolUses) > 0)

		thinkingFormat := config.GetThinkingConfig().OpenAIFormat
		resp := KiroToOpenAIResponseWithReasoning(finalContent, reasoningContent, toolUses, inputTokens, outputTokens, model, thinkingFormat)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(resp)
		return
	}

	if lastErr == nil {
		h.recordFailureForApiKey(apiKeyID, "openai", model, 503, "No available accounts", startedAt)
		h.sendOpenAIError(w, 503, "server_error", "No available accounts")
		return
	}

	status := statusForUpstreamError(lastErr)
	applyRetryAfterHeader(w, lastErr)
	h.recordFailureForApiKey(apiKeyID, "openai", model, status, lastErr.Error(), startedAt)
	h.sendOpenAIError(w, status, errorTypeForOpenAIStatus(status), lastErr.Error())
}
func (h *Handler) sendOpenAIError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"type":    errType,
			"message": message,
		},
	})
}
