package service

import (
	"context"
	"errors"
	"strings"
	"time"
)

// openAISIWCRefreshWindowCap 是 SIWC 的后台刷新提前量上限。
// 后台 RefreshBeforeExpiryHours 是小时级配置，而 SIWC access_token 只有 1 小时，
// 不封顶会每轮都刷、每次都轮转一次性 refresh_token，徒增竞争与上游压力。
const openAISIWCRefreshWindowCap = 10 * time.Minute

// OpenAISIWCTokenRefresher 处理 SIWC 子模式的 token 刷新。
//
// 与标准 OpenAITokenRefresher 共用 cache key（同一把 Redis 锁、同一份 token 缓存），
// 但刷新走 SIWC 的 token 端点，且声明：
//   - RequiresDistributedRefreshLock：拿不到分布式锁就不刷（fail-closed）；
//   - PersistsDetached：刷新成功后的落库脱离请求 ctx。
//
// 两者都是因为上游一次性轮转 refresh_token：并发刷新或丢弃已轮转的凭据都等于账号死亡。
type OpenAISIWCTokenRefresher struct {
	siwcService *OpenAISIWCOAuthService
}

// NewOpenAISIWCTokenRefresher 构造 SIWC 刷新器。
func NewOpenAISIWCTokenRefresher(siwcService *OpenAISIWCOAuthService) *OpenAISIWCTokenRefresher {
	return &OpenAISIWCTokenRefresher{siwcService: siwcService}
}

// CacheKey 与标准 OpenAI OAuth 同 key。
func (r *OpenAISIWCTokenRefresher) CacheKey(account *Account) string {
	return OpenAITokenCacheKey(account)
}

// CanRefresh 只处理有 client_id 与 refresh_token 的 SIWC 账号。
func (r *OpenAISIWCTokenRefresher) CanRefresh(account *Account) bool {
	if account == nil || !account.IsOpenAISIWC() || account.IsCredentialShadow() {
		return false
	}
	if account.GetOpenAISIWCClientID() == "" {
		return false
	}
	return strings.TrimSpace(account.GetCredential("refresh_token")) != ""
}

// NeedsRefresh：expires_at 缺失即刷（未知即刷，与标准版"限流才刷"不同），
// 否则剩余有效期小于 min(refreshWindow, 10 分钟) 才刷。
func (r *OpenAISIWCTokenRefresher) NeedsRefresh(account *Account, refreshWindow time.Duration) bool {
	if !r.CanRefresh(account) {
		return false
	}
	expiresAt := account.GetCredentialAsTime("expires_at")
	if expiresAt == nil {
		return true
	}
	window := refreshWindow
	if window <= 0 || window > openAISIWCRefreshWindowCap {
		window = openAISIWCRefreshWindowCap
	}
	return time.Until(*expiresAt) < window
}

// Refresh 调用 SIWC token 端点并把增量键合并进原 credentials。
func (r *OpenAISIWCTokenRefresher) Refresh(ctx context.Context, account *Account) (map[string]any, error) {
	if r == nil || r.siwcService == nil {
		return nil, errors.New("siwc oauth service is not configured")
	}
	delta, err := r.siwcService.RefreshCredential(ctx, account)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(siwcStringValue(delta["refresh_token"])) == "" {
		// 上游没发新 RT（不应发生），保留旧值继续用。
		delete(delta, "refresh_token")
	}
	return MergeCredentials(account.Credentials, delta), nil
}

// RequiresDistributedRefreshLock 声明 fail-closed：无分布式锁不刷新。
func (r *OpenAISIWCTokenRefresher) RequiresDistributedRefreshLock(account *Account) bool {
	return account != nil && account.IsOpenAISIWC()
}

// PersistsDetached 声明刷新成功后的持久化脱离请求 ctx。
func (r *OpenAISIWCTokenRefresher) PersistsDetached(account *Account) bool {
	return account != nil && account.IsOpenAISIWC()
}

// openAITokenRefresherComposite 按 auth_mode 把 OpenAI 平台的刷新分派到标准刷新器或 SIWC 刷新器。
// TokenRefreshService 的 registrations 按 platform 唯一，所以分派必须发生在 executor 内部。
type openAITokenRefresherComposite struct {
	standard *OpenAITokenRefresher
	siwc     *OpenAISIWCTokenRefresher
}

// NewOpenAITokenRefresherComposite 构造 OpenAI 平台的组合刷新器；siwc 为 nil 时退化为标准刷新器。
func NewOpenAITokenRefresherComposite(standard *OpenAITokenRefresher, siwc *OpenAISIWCTokenRefresher) OAuthRefreshExecutor {
	if siwc == nil {
		return standard
	}
	return &openAITokenRefresherComposite{standard: standard, siwc: siwc}
}

func (c *openAITokenRefresherComposite) isSIWC(account *Account) bool {
	return c != nil && c.siwc != nil && account != nil && account.IsOpenAISIWC()
}

func (c *openAITokenRefresherComposite) CacheKey(account *Account) string {
	if c.isSIWC(account) {
		return c.siwc.CacheKey(account)
	}
	return c.standard.CacheKey(account)
}

func (c *openAITokenRefresherComposite) CanRefresh(account *Account) bool {
	if c.isSIWC(account) {
		return c.siwc.CanRefresh(account)
	}
	return c.standard.CanRefresh(account)
}

func (c *openAITokenRefresherComposite) NeedsRefresh(account *Account, refreshWindow time.Duration) bool {
	if c.isSIWC(account) {
		return c.siwc.NeedsRefresh(account, refreshWindow)
	}
	return c.standard.NeedsRefresh(account, refreshWindow)
}

func (c *openAITokenRefresherComposite) Refresh(ctx context.Context, account *Account) (map[string]any, error) {
	if c.isSIWC(account) {
		return c.siwc.Refresh(ctx, account)
	}
	return c.standard.Refresh(ctx, account)
}

func (c *openAITokenRefresherComposite) RequiresDistributedRefreshLock(account *Account) bool {
	return c.isSIWC(account) && c.siwc.RequiresDistributedRefreshLock(account)
}

func (c *openAITokenRefresherComposite) PersistsDetached(account *Account) bool {
	return c.isSIWC(account) && c.siwc.PersistsDetached(account)
}

func siwcStringValue(v any) string {
	s, _ := v.(string)
	return s
}
