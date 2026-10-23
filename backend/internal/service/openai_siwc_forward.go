package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

// SIWC 转发：上游是 api.openai.com/v1/responses，不是 chatgpt.com/backend-api/codex。
// 与 Codex 路径的差异全部收口在本文件：
//   - 不发 chatgpt-account-id / x-codex-* / 路由提示，不做指纹收敛，不用 uTLS；
//   - 请求体强制 store=false、stream=true，删掉 SIWC 上游拒绝的字段，工具按白名单放行；
//   - compaction_trigger / tool_search 之类上游不支持的能力在网关直接 400，不打上游。
const (
	// openAISIWCIdentityModeExtraKey 控制 SIWC 出站身份：codex（默认，与 cpa 线上一致）或 neutral。
	// 留一个开关是为了降智对照实验可以不发版切换。
	openAISIWCIdentityModeExtraKey = "siwc_identity_mode"
	openAISIWCIdentityModeCodex    = "codex"
	openAISIWCIdentityModeNeutral  = "neutral"
	// openAISIWCNeutralUserAgent 是 neutral 模式的 UA（照 CLIProxyAPI 的 "<name> (os; arch)" 形态）。
	openAISIWCNeutralUserAgent = "sub2api (siwc)"
	// openAISIWCSessionHashLen 会话头取哈希前 32 个十六进制字符。
	openAISIWCSessionHashLen = 32
)

// openAISIWCAllowedTools 是 SIWC 上游放行的工具类型；其它类型（如 image_generation）会被上游拒绝。
var openAISIWCAllowedTools = map[string]bool{
	"web_search":                true,
	"web_search_preview":        true,
	"function":                  true,
	"custom":                    true,
	"namespace":                 true,
	"programmatic_tool_calling": true,
}

// openAISIWCStrippedFields 是 SIWC 上游不接受、必须从请求体删除的顶层字段。
var openAISIWCStrippedFields = []string{
	"prompt_cache_retention",
	"prompt_cache_options",
	"max_output_tokens",
	"max_completion_tokens",
	"max_tokens",
	"temperature",
	"previous_response_id",
	"safety_identifier",
}

// openAISIWCPassthroughHeaders 只透传这两个低风险客户端头；originator / session_id / x-codex-* 全部丢弃。
var openAISIWCPassthroughHeaders = map[string]bool{
	"accept-language": true,
	"content-type":    true,
}

// openAISIWCUnsupportedCapabilityError 表示请求用到了 SIWC 上游不支持的能力。
type openAISIWCUnsupportedCapabilityError struct {
	Capability string
}

func (e *openAISIWCUnsupportedCapabilityError) Error() string {
	return fmt.Sprintf("capability %q is not supported when using Sign in with ChatGPT", e.Capability)
}

// IsOpenAISIWCUnsupportedCapabilityError 判断错误是否为 SIWC 能力不支持。
func IsOpenAISIWCUnsupportedCapabilityError(err error) bool {
	var target *openAISIWCUnsupportedCapabilityError
	return errors.As(err, &target)
}

// detectOpenAISIWCUnsupportedCapability 扫描请求体，命中上游拒绝的能力就返回错误。
// 只看 input[] 的 type 与 compact 路径，不展开大 map。
func detectOpenAISIWCUnsupportedCapability(c *gin.Context, body []byte) error {
	if c != nil && isOpenAIResponsesCompactPath(c) {
		return &openAISIWCUnsupportedCapabilityError{Capability: "responses_compact"}
	}
	if len(body) == 0 {
		return nil
	}
	var found string
	gjson.GetBytes(body, "input").ForEach(func(_, item gjson.Result) bool {
		itemType := strings.TrimSpace(item.Get("type").String())
		switch {
		case itemType == "compaction_trigger":
			found = itemType
		case strings.HasPrefix(itemType, "tool_search"):
			found = itemType
		}
		return found == ""
	})
	if found != "" {
		return &openAISIWCUnsupportedCapabilityError{Capability: found}
	}
	return nil
}

// normalizeOpenAISIWCRequestBody 把请求体改写成 SIWC 上游接受的形态。
func normalizeOpenAISIWCRequestBody(body []byte) ([]byte, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return body, nil
	}
	var decoded map[string]any
	if err := decodeOpenAIJSONUseNumber(body, &decoded); err != nil {
		return nil, fmt.Errorf("decode siwc request body: %w", err)
	}
	decoded["store"] = false
	decoded["stream"] = true
	// SIWC 上游要求 input 为数组（"Input must be a list"），字符串形式转成单条 user 消息。
	if text, ok := decoded["input"].(string); ok {
		decoded["input"] = []any{map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "input_text", "text": text}},
		}}
	}
	sanitizeOpenAISIWCInputItems(decoded)
	for _, field := range openAISIWCStrippedFields {
		delete(decoded, field)
	}
	if rawTools, ok := decoded["tools"].([]any); ok {
		kept := make([]any, 0, len(rawTools))
		removed := map[string]bool{}
		for _, raw := range rawTools {
			tool, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			toolType := strings.TrimSpace(fmt.Sprint(tool["type"]))
			if openAISIWCAllowedTools[toolType] {
				kept = append(kept, tool)
				continue
			}
			if name, ok := tool["name"].(string); ok && strings.TrimSpace(name) != "" {
				removed[strings.TrimSpace(name)] = true
			}
			removed[toolType] = true
		}
		if len(kept) == 0 {
			delete(decoded, "tools")
			delete(decoded, "tool_choice")
		} else {
			decoded["tools"] = kept
			// tool_choice 指向被剔除的工具时回落 auto，避免上游 400。
			if choice, ok := decoded["tool_choice"].(map[string]any); ok {
				choiceType := strings.TrimSpace(fmt.Sprint(choice["type"]))
				choiceName, _ := choice["name"].(string)
				if removed[choiceType] || (choiceName != "" && removed[strings.TrimSpace(choiceName)]) {
					decoded["tool_choice"] = "auto"
				}
			}
		}
	}
	return marshalOpenAIUpstreamJSON(decoded)
}

// openAISIWCSessionID 生成 SIWC 的稳定会话标识：同一 API Key、同一凭据、同一 prompt_cache_key
// 落到同一个值，让上游缓存命中；没有 prompt_cache_key 时每个请求独立。
func openAISIWCSessionID(apiKeyID int64, account *Account, promptCacheKey string) string {
	promptCacheKey = strings.TrimSpace(promptCacheKey)
	if promptCacheKey == "" {
		return uuid.NewString()
	}
	clientID := ""
	if account != nil {
		clientID = account.GetOpenAISIWCClientID()
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("siwc-session-v1|%d|%s|%s", apiKeyID, clientID, promptCacheKey)))
	return hex.EncodeToString(sum[:])[:openAISIWCSessionHashLen]
}

// resolveOpenAISIWCIdentity 返回 SIWC 出站的 User-Agent 与 originator（空表示不发）。
func resolveOpenAISIWCIdentity(account *Account) (userAgent, originator string) {
	mode := openAISIWCIdentityModeCodex
	if account != nil {
		if configured := strings.ToLower(strings.TrimSpace(account.GetExtraString(openAISIWCIdentityModeExtraKey))); configured != "" {
			mode = configured
		}
	}
	switch mode {
	case openAISIWCIdentityModeNeutral:
		userAgent, originator = openAISIWCNeutralUserAgent, ""
	default:
		// 与 cpa 线上 6 天稳定运行的做法一致：带 Codex 规范身份，但不带 chatgpt-account-id 等账号头。
		userAgent, originator = CodexCanonicalAuthIdentity()
	}
	if account != nil {
		if custom := strings.TrimSpace(account.GetOpenAIUserAgent()); custom != "" {
			userAgent = custom
		}
	}
	return userAgent, originator
}

// buildOpenAISIWCUpstreamRequest 构造发往 api.openai.com/v1/responses 的请求。
// 与 buildUpstreamRequest 的 Codex 路径完全分离：不调用任何 chatgpt-account-id /
// 指纹 / 路由提示 / beta features 的辅助函数。
func (s *OpenAIGatewayService) buildOpenAISIWCUpstreamRequest(ctx context.Context, c *gin.Context, account *Account, body []byte, token string, promptCacheKey string) (*http.Request, error) {
	if account == nil || !account.IsOpenAISIWC() {
		return nil, errors.New("not an openai siwc account")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("siwc access token is empty")
	}
	if err := detectOpenAISIWCUnsupportedCapability(c, body); err != nil {
		return nil, err
	}
	normalizedBody, err := normalizeOpenAISIWCRequestBody(body)
	if err != nil {
		return nil, err
	}

	targetURL := openaiPlatformAPIURL
	if c != nil {
		targetURL = appendOpenAIResponsesRequestPathSuffix(targetURL, openAIResponsesRequestPathSuffix(c))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(normalizedBody))
	if err != nil {
		return nil, err
	}
	// 复用 OpenAI 平台的连接池 / 超时 profile（该 profile 不带 uTLS 指纹）。
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))

	// 只透传低风险客户端头。
	if c != nil && c.Request != nil {
		for key, values := range c.Request.Header {
			if !openAISIWCPassthroughHeaders[strings.ToLower(key)] {
				continue
			}
			for _, value := range values {
				req.Header.Add(key, value)
			}
		}
	}

	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	userAgent, originator := resolveOpenAISIWCIdentity(account)
	req.Header.Set("User-Agent", userAgent)
	if originator != "" {
		req.Header.Set("originator", originator)
	}

	var apiKeyID int64
	if c != nil {
		apiKeyID = getAPIKeyIDFromContext(c)
	}
	sessionID := openAISIWCSessionID(apiKeyID, account, promptCacheKey)
	req.Header.Set("session_id", sessionID)
	req.Header.Set("Thread-Id", sessionID)
	req.Header.Set("X-Client-Request-Id", sessionID)

	// 账号级请求头覆写对 OAuth 路径本就是 no-op，这里不调用，避免把 Codex 头带进来。
	return req, nil
}

// writeOpenAISIWCUnsupportedCapabilityResponse 把能力不支持错误写成 400，不计账号错误。
func writeOpenAISIWCUnsupportedCapabilityResponse(c *gin.Context, err error) {
	if c == nil {
		return
	}
	message := err.Error()
	setOpsUpstreamError(c, http.StatusBadRequest, message, "")
	c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
		"type":    "invalid_request_error",
		"code":    "subscription_sharing_unsupported_capability",
		"message": message,
	}})
}

// openAISIWCFoldableEfforts 是 configuration_update 可折叠到顶层的推理档位。
// sub2api 没有 cpa 那份按模型的推理档位目录，按 cpa「目录里没有该模型时只放行标准档位」的兜底语义处理。
var openAISIWCFoldableEfforts = map[string]bool{
	"none": true, "minimal": true, "low": true, "medium": true, "high": true, "xhigh": true, "max": true,
}

// openAISIWCStrippedItemFields 是 Codex MultiAgentV2 的内部元数据，SIWC 上游不认。
var openAISIWCStrippedItemFields = []string{"author", "recipient", "internal_chat_message_metadata_passthrough"}

// sanitizeOpenAISIWCInputItems 改写 SIWC 上游会以 400 subscription_sharing_unsupported_capability
// 拒绝的 input 项（移植自 cpa e2f3f0c 与 CLIProxyAPI 的 sanitizeInputForTokenSharing）：
//   - configuration_update：Codex 0.162+ 中途切换推理强度时插入。取最后一条非空强度写到顶层
//     reasoning.effort（保留 summary 等其它字段），并删除全部该项；auto / 未知档位只删项。
//   - agent_message：MultiAgentV2 子代理消息，改写成 role=user 的普通 message，
//     encrypted_content 片段转成 input_text。
//   - author / recipient / internal_chat_message_metadata_passthrough：多代理元数据，删除。
//   - 非 assistant 消息里的 output_text 片段降级为 input_text（上游对此返回通用 400）。
func sanitizeOpenAISIWCInputItems(decoded map[string]any) {
	items, ok := decoded["input"].([]any)
	if !ok {
		return
	}
	kept := make([]any, 0, len(items))
	foldEffort := ""
	removedConfigurationUpdate := false
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			kept = append(kept, raw)
			continue
		}
		itemType := strings.TrimSpace(fmt.Sprint(item["type"]))
		if itemType == "configuration_update" {
			removedConfigurationUpdate = true
			if reasoning, ok := item["reasoning"].(map[string]any); ok {
				if effort, ok := reasoning["effort"].(string); ok {
					if normalized := strings.ToLower(strings.TrimSpace(effort)); normalized != "" {
						foldEffort = normalized
					}
				}
			}
			continue
		}
		for _, field := range openAISIWCStrippedItemFields {
			delete(item, field)
		}
		if itemType == "agent_message" {
			item["type"] = "message"
			item["role"] = "user"
			if parts, ok := item["content"].([]any); ok {
				for _, rawPart := range parts {
					part, ok := rawPart.(map[string]any)
					if !ok || strings.TrimSpace(fmt.Sprint(part["type"])) != "encrypted_content" {
						continue
					}
					if text, ok := part["encrypted_content"].(string); ok {
						part["type"] = "input_text"
						part["text"] = text
						delete(part, "encrypted_content")
					}
				}
			}
		}
		if strings.TrimSpace(fmt.Sprint(item["type"])) == "message" && fmt.Sprint(item["role"]) != "assistant" {
			if parts, ok := item["content"].([]any); ok {
				for _, rawPart := range parts {
					if part, ok := rawPart.(map[string]any); ok && fmt.Sprint(part["type"]) == "output_text" {
						part["type"] = "input_text"
					}
				}
			}
		}
		kept = append(kept, item)
	}
	decoded["input"] = kept
	if !removedConfigurationUpdate || !openAISIWCFoldableEfforts[foldEffort] {
		return
	}
	reasoning, ok := decoded["reasoning"].(map[string]any)
	if !ok {
		reasoning = map[string]any{}
	}
	reasoning["effort"] = foldEffort
	decoded["reasoning"] = reasoning
}
