package service

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// openAISIWCProbeReadLimit 探测响应最多读取的字节数（SSE 全文很小，防御性上限）。
const openAISIWCProbeReadLimit = 4 << 20

// siwcGateBlocker 是网关在 403 汇聚点调用的闸门入口（由 SIWCGateService 实现）。
type siwcGateBlocker interface {
	Block(ctx context.Context, account *Account, code, requestID string) (bool, error)
	HandleUsageLimit(ctx context.Context, account *Account, resetAt time.Time)
}

// SetSIWCGate 注入资格闸门；未注入时 403 user_not_eligible 退化为普通错误处理。
func (s *OpenAIGatewayService) SetSIWCGate(gate siwcGateBlocker) {
	if s == nil {
		return
	}
	s.siwcGate = gate
}

// ProbeSIWC 用账号的 SIWC 凭据向 api.openai.com/v1/responses 发一个固定请求，
// 返回状态码、结构化错误码、request id 与推理 token 数（SSE 的 response.completed）。
// 复用转发路径同一套请求构造，保证探测和真实流量的出站身份一致。
func (s *OpenAIGatewayService) ProbeSIWC(ctx context.Context, account *Account, body []byte) (*SIWCProbeResult, error) {
	if s == nil || s.httpUpstream == nil || s.openAITokenProvider == nil {
		return nil, errors.New("siwc probe is not configured")
	}
	if account == nil || !account.IsOpenAISIWC() {
		return nil, errors.New("not an openai siwc account")
	}
	token, err := s.openAITokenProvider.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	req, err := s.buildOpenAISIWCUpstreamRequest(ctx, nil, account, body, token, "")
	if err != nil {
		return nil, err
	}
	proxyURL := ""
	if account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, openAISIWCProbeReadLimit))
	if err != nil {
		return nil, err
	}
	result := &SIWCProbeResult{
		Status:    resp.StatusCode,
		RequestID: strings.TrimSpace(resp.Header.Get("x-request-id")),
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		result.Code = extractUpstreamErrorCode(payload)
		return result, nil
	}
	tokens, ok := parseSIWCReasoningTokens(payload)
	result.ReasoningTokens = tokens
	result.HasReasoning = ok
	return result, nil
}

// parseSIWCReasoningTokens 从 SSE（或纯 JSON）响应里取 usage.output_tokens_details.reasoning_tokens。
func parseSIWCReasoningTokens(payload []byte) (int64, bool) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return 0, false
	}
	if trimmed[0] == '{' {
		if v := gjson.GetBytes(trimmed, "usage.output_tokens_details.reasoning_tokens"); v.Exists() {
			return v.Int(), true
		}
		if v := gjson.GetBytes(trimmed, "response.usage.output_tokens_details.reasoning_tokens"); v.Exists() {
			return v.Int(), true
		}
		return 0, false
	}
	scanner := bufio.NewScanner(bytes.NewReader(trimmed))
	scanner.Buffer(make([]byte, 0, 64*1024), openAISIWCProbeReadLimit)
	var last int64
	found := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		if gjson.Get(data, "type").String() != "response.completed" {
			continue
		}
		if v := gjson.Get(data, "response.usage.output_tokens_details.reasoning_tokens"); v.Exists() {
			last = v.Int()
			found = true
		}
	}
	return last, found
}
