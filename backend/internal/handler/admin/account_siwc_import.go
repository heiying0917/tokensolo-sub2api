package admin

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// SIWC 凭据导入：识别 cpa 工具包（chatgpt-account-toolkit）产出的 JSON 并落成 SIWC 账号行。
//
// 支持三种形态：
//  1. 工具包原生：{"type":"chatgpt_siwc","email","client_id":"oaiapp_…","access_token","refresh_token",
//     "id_token","plan_type","issuer","subject","scopes":[…],"expires_at":"RFC3339|unix","saved_at",…}
//  2. cpa 包裹：{"type":"codex","token_sharing":true,"siwc":{…同上…}}
//  3. 仅凭 client_id 前缀 oaiapp_ 识别的裸对象。
//
// 导入后立即走带锁的强制刷新完成 refresh_token 所有权转移（上游一次性轮转，JSON 副本随即作废）。

const (
	siwcImportTypeToolkit = "chatgpt_siwc"
	siwcIdentityKeyPrefix = "siwc:client_id:"
)

// siwcImportPayload 判断导入条目是否为 SIWC 凭据，并返回实际承载字段的对象。
func siwcImportPayload(raw map[string]any) (map[string]any, bool) {
	if raw == nil {
		return nil, false
	}
	if nested, ok := firstCodexMap(raw, []string{"siwc"}); ok && nested != nil {
		if strings.HasPrefix(strings.TrimSpace(firstCodexString(nested, []string{"client_id"}, []string{"clientId"})), openai.SIWCIssuedClientIDPrefix) {
			return nested, true
		}
	}
	if strings.EqualFold(strings.TrimSpace(firstCodexString(raw, []string{"type"})), siwcImportTypeToolkit) {
		return raw, true
	}
	if strings.EqualFold(strings.TrimSpace(firstCodexString(raw, []string{"auth_mode"}, []string{"authMode"})), service.OpenAIAuthModeSIWC) {
		return raw, true
	}
	if strings.HasPrefix(strings.TrimSpace(firstCodexString(raw, []string{"client_id"}, []string{"clientId"})), openai.SIWCIssuedClientIDPrefix) {
		return raw, true
	}
	return nil, false
}

func siwcImportIdentityKeys(clientID string) []string {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		return nil
	}
	return []string{siwcIdentityKeyPrefix + clientID}
}

func firstCodexStringSlice(obj map[string]any, keys ...string) []string {
	for _, key := range keys {
		raw, ok := obj[key]
		if !ok {
			continue
		}
		switch v := raw.(type) {
		case []string:
			return v
		case []any:
			out := make([]string, 0, len(v))
			for _, item := range v {
				if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
					out = append(out, strings.TrimSpace(s))
				}
			}
			return out
		case string:
			return openai.SplitScopes(v)
		}
	}
	return nil
}

// normalizeSIWCImportEntry 把 SIWC JSON 归一化为导入条目。
// 与 Codex 导入的关键差异：access_token 可以缺失或已过期（AT 只有 1 小时，导入几乎必然过期），
// 身份键只用 client_id（同邮箱多份凭据各自成行，且与普通 Codex 行互不匹配）。
func normalizeSIWCImportEntry(entry codexImportEntry, raw map[string]any) (*codexImportAccount, error) {
	now := time.Now().UTC()
	item := &codexImportAccount{
		IsSIWC:      true,
		Credentials: map[string]any{service.OpenAIAuthModeCredentialKey(): service.OpenAIAuthModeSIWC},
		Extra: map[string]any{
			"import_source": service.OpenAISIWCImportSource,
			"auth_provider": service.OpenAISIWCImportSource,
			"imported_at":   now.Format(time.RFC3339),
		},
	}
	for k, v := range service.BuildOpenAISIWCAccountExtra(0) {
		item.Extra[k] = v
	}

	clientID := strings.TrimSpace(firstCodexString(raw, []string{"client_id"}, []string{"clientId"}))
	if !strings.HasPrefix(clientID, openai.SIWCIssuedClientIDPrefix) {
		return nil, fmt.Errorf("第 %d 条缺少 SIWC 签发的 client_id（oaiapp_*）", entry.Index)
	}
	item.RefreshToken = strings.TrimSpace(firstCodexString(raw, []string{"refresh_token"}, []string{"refreshToken"}))
	if item.RefreshToken == "" {
		return nil, errors.New("SIWC 凭据缺少 refresh_token")
	}
	item.AccessToken = strings.TrimSpace(firstCodexString(raw, []string{"access_token"}, []string{"accessToken"}))
	item.IDToken = strings.TrimSpace(firstCodexString(raw, []string{"id_token"}, []string{"idToken"}))
	item.Email = strings.TrimSpace(firstCodexString(raw, []string{"email"}))
	item.PlanType = strings.TrimSpace(firstCodexString(raw, []string{"plan_type"}, []string{"planType"}))

	if item.IDToken != "" {
		// 只取身份字段，不校验过期（SIWC id_token 只在换码时签发一次）
		_ = enrichCodexImportAccountFromJWT(item, item.IDToken, false, now)
	}
	if item.Email == "" {
		return nil, errors.New("SIWC 凭据缺少 email（无法关联管理凭据与兄弟行）")
	}

	scopes := firstCodexStringSlice(raw, "scopes", "scope", "authorization_scope")
	if len(scopes) > 0 {
		hasRequired := false
		for _, s := range scopes {
			if s == openai.SIWCRequiredScope {
				hasRequired = true
				break
			}
		}
		if !hasRequired {
			return nil, fmt.Errorf("SIWC 凭据缺少 %s scope", openai.SIWCRequiredScope)
		}
		item.Credentials["siwc_scopes"] = scopes
	}

	item.Credentials["client_id"] = clientID
	item.Credentials["refresh_token"] = item.RefreshToken
	if item.AccessToken != "" {
		item.Credentials["access_token"] = item.AccessToken
	} else {
		// 没有 AT 时写占位，首次使用由刷新器补齐；access_token 非空是现有校验的前置条件。
		item.Credentials["access_token"] = ""
		item.WarningTexts = append(item.WarningTexts, "SIWC 凭据不含 access_token，导入后将立即刷新获取")
	}
	if item.IDToken != "" {
		item.Credentials["id_token"] = item.IDToken
	}
	setCodexCredentialIfNotEmpty(item.Credentials, "email", item.Email)
	setCodexCredentialIfNotEmpty(item.Credentials, "plan_type", item.PlanType)
	setCodexCredentialIfNotEmpty(item.Credentials, "siwc_issuer", strings.TrimSpace(firstCodexString(raw, []string{"issuer"})))
	setCodexCredentialIfNotEmpty(item.Credentials, "siwc_subject", strings.TrimSpace(firstCodexString(raw, []string{"subject"})))
	setCodexCredentialIfNotEmpty(item.Credentials, "siwc_device_id", strings.TrimSpace(firstCodexString(raw, []string{"device_id"}, []string{"deviceId"})))
	setCodexCredentialIfNotEmpty(item.Credentials, "siwc_agent_name", strings.TrimSpace(firstCodexString(raw, []string{"agent_name"}, []string{"name"})))

	// expires_at 缺失或已过期都不拒绝：写成过去时间让首次使用触发刷新。
	if expiresAt, ok := firstCodexTime(raw, []string{"expires_at"}, []string{"expiresAt"}, []string{"expired"}); ok {
		if expiresAt.Before(now) {
			item.WarningTexts = append(item.WarningTexts, fmt.Sprintf("SIWC access_token 已于 %s 过期，导入后将立即刷新", expiresAt.Format(time.RFC3339)))
		}
		item.TokenExpiresAt = &expiresAt
		item.Credentials["expires_at"] = expiresAt.UTC().Format(time.RFC3339)
	} else {
		past := now.Add(-time.Minute)
		item.TokenExpiresAt = &past
		item.Credentials["expires_at"] = past.Format(time.RFC3339)
	}
	for _, key := range []string{"authorization_profile", "saved_at"} {
		if v := strings.TrimSpace(firstCodexString(raw, []string{key})); v != "" {
			item.Extra["siwc_"+key] = v
		}
	}
	if item.PlanType == "" {
		item.WarningTexts = append(item.WarningTexts, "SIWC 凭据不含 plan_type，将按同邮箱管理凭据回填；缺失时按 free 目录处理")
	}

	item.IdentityKeys = siwcImportIdentityKeys(clientID)
	item.Name = buildSIWCImportAccountName(item, raw)
	return item, nil
}

func buildSIWCImportAccountName(item *codexImportAccount, raw map[string]any) string {
	if name := strings.TrimSpace(firstCodexString(raw, []string{"name"})); name != "" {
		return name
	}
	label := strings.TrimSpace(firstCodexString(raw, []string{"agent_name"}, []string{"label"}))
	if label != "" {
		return fmt.Sprintf("%s (siwc:%s)", item.Email, label)
	}
	return item.Email + " (siwc)"
}

// siwcStoredIdentityKeys 存量 SIWC 账号行只按 client_id 建索引。
func siwcStoredIdentityKeys(account service.Account) ([]string, bool) {
	if !account.IsOpenAISIWC() {
		return nil, false
	}
	return siwcImportIdentityKeys(codexCredentialString(account.Credentials, "client_id")), true
}
