package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// SIWC（Sign in with ChatGPT）子模式：挂在 platform=openai, type=oauth 账号下，
// 由 credentials.auth_mode="siwc" 标识。与 Codex PAT / Agent Identity 同一套
// 子模式机制，只在取 token、刷新判定、鉴权头、能力裁剪、导入五个收口点分支。
//
// 关键差异：
//   - 上游是 api.openai.com/v1/responses，不是 chatgpt.com/backend-api/codex；
//   - SIWC 令牌打 chatgpt.com 会 401 rejected_by_access_enforcement，
//     所以额度 / 模型清单 / plan_type 全部借用同邮箱的 Codex OAuth"管理凭据"；
//   - refresh_token 一次性轮转，刷新必须串行化并脱离请求 ctx 持久化。
const (
	// OpenAIAuthModeSIWC 是 credentials.auth_mode 的取值。
	OpenAIAuthModeSIWC = "siwc"

	// openAISIWCManagementAccountExtraKey 显式指定管理凭据账号 ID（extra）。
	openAISIWCManagementAccountExtraKey = "siwc_management_account_id"
	// openAISIWCDeviceIDCredentialKey 授权时生成的设备 UUID（ext_agent_host_id）。
	openAISIWCDeviceIDCredentialKey = "siwc_device_id"
	// openAISIWCAgentNameCredentialKey 授权时的 agent_name_hint。
	openAISIWCAgentNameCredentialKey = "siwc_agent_name"
	// openAISIWCScopesCredentialKey token 响应里的 scope 列表。
	openAISIWCScopesCredentialKey = "siwc_scopes"
	// openAISIWCIssuerCredentialKey / openAISIWCSubjectCredentialKey 来自 id_token，诊断用。
	openAISIWCIssuerCredentialKey  = "siwc_issuer"
	openAISIWCSubjectCredentialKey = "siwc_subject"

	// OpenAIPrivacyModeSIWCNotApplicable 写入 extra.privacy_mode，
	// 让 shouldSkipOpenAIPrivacyEnsure 跳过"关闭训练"调用（SIWC 令牌不能打 chatgpt.com）。
	OpenAIPrivacyModeSIWCNotApplicable = "siwc_not_applicable"

	// OpenAISIWCImportSource 写入 extra.import_source / auth_provider。
	OpenAISIWCImportSource = "chatgpt_siwc"
)

// openAISIWCDefaultModels 是空 model_mapping 时 SIWC 账号可服务的模型白名单。
// 来源：cpa 实测 pro 账号经 Codex 令牌查 chatgpt.com/backend-api/codex/models
// 得到的清单，与 SIWC 实测一致；gpt-5.5 家族被上游明确拒绝
// ("not supported when using Codex with a ChatGPT account")。
var openAISIWCDefaultModels = []string{
	"gpt-6-sol",
	"gpt-6-astra",
	"gpt-6-luna",
	"gpt-6.1-sol",
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"gpt-5.6-luna",
}

// errOpenAISIWCManagementAccountMissing 表示找不到可用的管理凭据。
var errOpenAISIWCManagementAccountMissing = errors.New("openai siwc management account not found")

// OpenAIAuthModeCredentialKey 返回 credentials 里 auth_mode 的键名（供 handler 层构造凭据）。
func OpenAIAuthModeCredentialKey() string {
	return openAIAuthModeCredentialKey
}

// IsOpenAISIWC 报告账号是否为 SIWC 子模式。
func (a *Account) IsOpenAISIWC() bool {
	if a == nil || !a.IsOpenAIOAuth() {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(a.GetCredential(openAIAuthModeCredentialKey)), OpenAIAuthModeSIWC)
}

// GetOpenAISIWCClientID 返回授权回调签发的 oaiapp_* client_id。
func (a *Account) GetOpenAISIWCClientID() string {
	if a == nil {
		return ""
	}
	return strings.TrimSpace(a.GetCredential("client_id"))
}

// GetOpenAISIWCDeviceID 返回授权时生成的设备 UUID，导入的凭据可能为空。
func (a *Account) GetOpenAISIWCDeviceID() string {
	if a == nil {
		return ""
	}
	return strings.TrimSpace(a.GetCredential(openAISIWCDeviceIDCredentialKey))
}

// GetOpenAISIWCManagementAccountID 返回 extra 里显式指定的管理凭据账号 ID，未指定返回 0。
func (a *Account) GetOpenAISIWCManagementAccountID() int64 {
	if a == nil || a.Extra == nil {
		return 0
	}
	return extraInt64Value(a.Extra[openAISIWCManagementAccountExtraKey])
}

// extraInt64Value 把 JSONB 解码后的各种数值形态归一化为 int64。
func extraInt64Value(raw any) int64 {
	switch v := raw.(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case int32:
		return int64(v)
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return n
	default:
		return 0
	}
}

// openAIAccountCredentialEmail 返回小写、去空白的凭据邮箱，用于同账号关联。
func openAIAccountCredentialEmail(a *Account) string {
	if a == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(a.GetCredential("email")))
}

// isOpenAISIWCServableModel 判断空 model_mapping 的 SIWC 账号能否服务请求模型。
// 与 isOpenAIOAuthServableModel 的"黑名单默认放行"不同，这里是白名单精确匹配：
// SIWC 上游对不支持的模型返回不可重试的 400，且会触发 per-model 冷却，
// 应在调度阶段就跳过。带 [1m] 后缀或带前缀的写法先取最后一段再比对。
func isOpenAISIWCServableModel(requestedModel string) bool {
	model := strings.ToLower(normalizeClaudeCodeLongContextModel(strings.TrimSpace(lastOpenAIModelSegment(requestedModel))))
	if model == "" {
		return true // 空模型交由上层必填校验处理
	}
	if strings.HasPrefix(model, "gpt-5.5") {
		return false
	}
	for _, candidate := range openAISIWCDefaultModels {
		if model == candidate {
			return true
		}
	}
	return false
}

// filterOpenAISIWCUpstreamModels 过滤管理凭据 codex/models 清单里 SIWC 上游明确拒绝的模型。
// 清单与 SIWC 实测基本一致，只有 gpt-5.5 家族在 Codex 侧可见、SIWC 侧被拒（cpa 用全局规则排除同一批）。
func filterOpenAISIWCUpstreamModels(models []string) []string {
	out := make([]string, 0, len(models))
	for _, model := range models {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "gpt-5.5") {
			continue
		}
		out = append(out, model)
	}
	return out
}

// isOpenAISIWCManagementCandidate 判断账号能否充当 SIWC 的管理凭据：
// 同平台 OAuth、非 SIWC、非影子、未删除且状态 active（可以不参与调度）。
func isOpenAISIWCManagementCandidate(candidate *Account) bool {
	if candidate == nil || !candidate.IsOpenAIOAuth() {
		return false
	}
	if candidate.IsOpenAISIWC() || candidate.IsCredentialShadow() {
		return false
	}
	if candidate.IsOpenAIPersonalAccessToken() || candidate.IsOpenAIAgentIdentity() {
		return false
	}
	if candidate.Status != StatusActive {
		return false
	}
	return strings.TrimSpace(candidate.GetCredential("access_token")) != ""
}

// resolveOpenAISIWCManagementAccount 解析 SIWC 账号的管理凭据：
//  1. extra.siwc_management_account_id 显式指定，校验通过即用；
//  2. 否则按 credentials.email 在同平台 OAuth 账号里自动匹配（取 ID 最小者）；
//  3. 都没有返回 errOpenAISIWCManagementAccountMissing，调用方按场景降级。
//
// 账号总量小于百级，直接 ListByPlatform 后内存过滤，不额外加 SQL / 索引，
// 也避免扩 AccountRepository 接口牵动全部测试桩。
func resolveOpenAISIWCManagementAccount(ctx context.Context, repo AccountRepository, account *Account) (*Account, error) {
	if repo == nil {
		return nil, errors.New("account repository is nil")
	}
	if account == nil || !account.IsOpenAISIWC() {
		return nil, errors.New("not an openai siwc account")
	}
	if explicitID := account.GetOpenAISIWCManagementAccountID(); explicitID > 0 {
		candidate, err := repo.GetByID(ctx, explicitID)
		if err != nil {
			return nil, fmt.Errorf("load siwc management account %d: %w", explicitID, err)
		}
		if !isOpenAISIWCManagementCandidate(candidate) {
			return nil, fmt.Errorf("%w: explicit account %d is not a usable codex oauth credential", errOpenAISIWCManagementAccountMissing, explicitID)
		}
		return candidate, nil
	}

	email := openAIAccountCredentialEmail(account)
	if email == "" {
		return nil, fmt.Errorf("%w: siwc account %d has no email", errOpenAISIWCManagementAccountMissing, account.ID)
	}
	accounts, err := repo.ListByPlatform(ctx, PlatformOpenAI)
	if err != nil {
		return nil, fmt.Errorf("list openai accounts for siwc management lookup: %w", err)
	}
	var best *Account
	for i := range accounts {
		candidate := &accounts[i]
		if openAIAccountCredentialEmail(candidate) != email {
			continue
		}
		if !isOpenAISIWCManagementCandidate(candidate) {
			continue
		}
		if best == nil || candidate.ID < best.ID {
			best = candidate
		}
	}
	if best == nil {
		return nil, fmt.Errorf("%w: no codex oauth account with email %s", errOpenAISIWCManagementAccountMissing, email)
	}
	return best, nil
}

// listOpenAISIWCSiblings 返回同邮箱的全部 SIWC 账号行（含传入账号本身）。
// 403 user_not_eligible / 429 usage_limit 按 ChatGPT 账号生效，兄弟行需要联动。
func listOpenAISIWCSiblings(ctx context.Context, repo AccountRepository, email string) ([]Account, error) {
	if repo == nil {
		return nil, errors.New("account repository is nil")
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return nil, nil
	}
	accounts, err := repo.ListByPlatform(ctx, PlatformOpenAI)
	if err != nil {
		return nil, fmt.Errorf("list openai accounts for siwc siblings: %w", err)
	}
	siblings := make([]Account, 0, 4)
	for i := range accounts {
		candidate := &accounts[i]
		if !candidate.IsOpenAISIWC() || candidate.IsCredentialShadow() {
			continue
		}
		if openAIAccountCredentialEmail(candidate) != email {
			continue
		}
		siblings = append(siblings, *candidate)
	}
	return siblings, nil
}
