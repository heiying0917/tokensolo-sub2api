package service

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// SIWC 上游错误映射（官方 devkit 定义的 subscription_sharing_* 错误码）。
//
//	403 subscription_sharing_user_not_eligible   → 资格闸门按邮箱封禁，不进 403 三次计数
//	429 subscription_sharing_usage_limit_exceeded → 套餐窗口耗尽，按窗口冷却全部兄弟行
//	400 subscription_sharing_unsupported_capability → 请求级错误，账号不动
type openAISIWCErrorKind int

const (
	openAISIWCErrorNone openAISIWCErrorKind = iota
	openAISIWCErrorNotEligible
	openAISIWCErrorUsageLimit
	openAISIWCErrorUnsupportedCapability
)

const openAISIWCErrorCodeUnsupportedCapability = "subscription_sharing_unsupported_capability"

// classifyOpenAISIWCUpstreamError 只认结构化 error.code，不扫 message。
func classifyOpenAISIWCUpstreamError(statusCode int, responseBody []byte) openAISIWCErrorKind {
	code := strings.TrimSpace(extractUpstreamErrorCode(responseBody))
	switch {
	case statusCode == http.StatusForbidden && code == SIWCGateCodeNotEligible:
		return openAISIWCErrorNotEligible
	case statusCode == http.StatusTooManyRequests && code == SIWCGateCodeUsageLimit:
		return openAISIWCErrorUsageLimit
	case statusCode == http.StatusBadRequest && code == openAISIWCErrorCodeUnsupportedCapability:
		return openAISIWCErrorUnsupportedCapability
	default:
		return openAISIWCErrorNone
	}
}

// resolveOpenAISIWCUsageLimitReset 取 429 的重置时间：
// 响应头 x-codex-primary-reset-after-seconds → body resets_in_seconds / resets_at → 缺省 5h（body 标 weekly 取 7d）。
func resolveOpenAISIWCUsageLimitReset(now time.Time, headers http.Header, responseBody []byte) time.Time {
	if headers != nil {
		if raw := strings.TrimSpace(headers.Get("x-codex-primary-reset-after-seconds")); raw != "" {
			if secs, err := strconv.ParseInt(raw, 10, 64); err == nil && secs > 0 {
				return now.Add(time.Duration(secs) * time.Second)
			}
		}
	}
	for _, path := range []string{"error.resets_in_seconds", "resets_in_seconds", "error.reset_after_seconds"} {
		if v := gjson.GetBytes(responseBody, path); v.Exists() && v.Int() > 0 {
			return now.Add(time.Duration(v.Int()) * time.Second)
		}
	}
	for _, path := range []string{"error.resets_at", "resets_at"} {
		if v := gjson.GetBytes(responseBody, path); v.Exists() && v.Int() > now.Unix() {
			return time.Unix(v.Int(), 0)
		}
	}
	lower := strings.ToLower(string(responseBody))
	if strings.Contains(lower, "weekly") || strings.Contains(lower, "7d") || strings.Contains(lower, "7-day") {
		return now.Add(siwcGateUsageLimitWeekly)
	}
	return now.Add(siwcGateUsageLimitDefault)
}

// handleOpenAISIWCUpstreamError 在账号错误汇聚点处理 SIWC 专属错误。
// 返回 handled=false 时落回通用路径；disable 表示本请求应换号。
func (s *OpenAIGatewayService) handleOpenAISIWCUpstreamError(ctx context.Context, account *Account, statusCode int, headers http.Header, responseBody []byte) (handled bool, disable bool) {
	if s == nil || account == nil || !account.IsOpenAISIWC() {
		return false, false
	}
	requestID := ""
	if headers != nil {
		requestID = strings.TrimSpace(headers.Get("x-request-id"))
	}
	switch classifyOpenAISIWCUpstreamError(statusCode, responseBody) {
	case openAISIWCErrorNotEligible:
		if s.siwcGate == nil {
			// 未注入闸门：至少阻断本账号调度，避免被反复轮到
			s.BlockAccountScheduling(account, time.Time{}, siwcGateErrorPrefix)
			slog.Warn("siwc_not_eligible_without_gate", "account_id", account.ID, "request_id", requestID)
			return true, true
		}
		if _, err := s.siwcGate.Block(ctx, account, SIWCGateCodeNotEligible, requestID); err != nil {
			slog.Warn("siwc_gate_block_failed", "account_id", account.ID, "error", err)
			s.BlockAccountScheduling(account, time.Time{}, siwcGateErrorPrefix)
		}
		return true, true
	case openAISIWCErrorUsageLimit:
		resetAt := resolveOpenAISIWCUsageLimitReset(time.Now(), headers, responseBody)
		if s.siwcGate != nil {
			s.siwcGate.HandleUsageLimit(ctx, account, resetAt)
		} else if s.rateLimitService != nil && s.rateLimitService.accountRepo != nil {
			if err := s.rateLimitService.accountRepo.SetRateLimited(ctx, account.ID, resetAt); err != nil {
				slog.Warn("siwc_usage_limit_set_failed", "account_id", account.ID, "error", err)
			}
		}
		s.BlockAccountScheduling(account, resetAt, "siwc_usage_limit")
		slog.Info("siwc_usage_limit_exceeded", "account_id", account.ID, "reset_at", resetAt, "request_id", requestID)
		return true, true
	case openAISIWCErrorUnsupportedCapability:
		// 请求级错误：账号状态不动，由通用路径把 400 原样回客户端
		return true, false
	default:
		return false, false
	}
}
