package service

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync/atomic"
	"time"
)

const (
	openAITokenRefreshSkew    = 3 * time.Minute
	openAITokenCacheSkew      = 5 * time.Minute
	openAILockInitialWait     = 20 * time.Millisecond
	openAILockMaxWait         = 120 * time.Millisecond
	openAILockMaxAttempts     = 5
	openAILockJitterRatio     = 0.2
	openAILockWarnThresholdMs = 250
)

// OpenAITokenRuntimeMetrics is a snapshot of refresh and lock contention metrics.
type OpenAITokenRuntimeMetrics struct {
	RefreshRequests    int64
	RefreshSuccess     int64
	RefreshFailure     int64
	LockAcquireFailure int64
	LockContention     int64
	LockWaitSamples    int64
	LockWaitTotalMs    int64
	LockWaitHit        int64
	LockWaitMiss       int64
	LastObservedUnixMs int64
}

type openAITokenRuntimeMetricsStore struct {
	refreshRequests    atomic.Int64
	refreshSuccess     atomic.Int64
	refreshFailure     atomic.Int64
	lockAcquireFailure atomic.Int64
	lockContention     atomic.Int64
	lockWaitSamples    atomic.Int64
	lockWaitTotalMs    atomic.Int64
	lockWaitHit        atomic.Int64
	lockWaitMiss       atomic.Int64
	lastObservedUnixMs atomic.Int64
}

func (m *openAITokenRuntimeMetricsStore) snapshot() OpenAITokenRuntimeMetrics {
	if m == nil {
		return OpenAITokenRuntimeMetrics{}
	}
	return OpenAITokenRuntimeMetrics{
		RefreshRequests:    m.refreshRequests.Load(),
		RefreshSuccess:     m.refreshSuccess.Load(),
		RefreshFailure:     m.refreshFailure.Load(),
		LockAcquireFailure: m.lockAcquireFailure.Load(),
		LockContention:     m.lockContention.Load(),
		LockWaitSamples:    m.lockWaitSamples.Load(),
		LockWaitTotalMs:    m.lockWaitTotalMs.Load(),
		LockWaitHit:        m.lockWaitHit.Load(),
		LockWaitMiss:       m.lockWaitMiss.Load(),
		LastObservedUnixMs: m.lastObservedUnixMs.Load(),
	}
}

func (m *openAITokenRuntimeMetricsStore) touchNow() {
	if m == nil {
		return
	}
	m.lastObservedUnixMs.Store(time.Now().UnixMilli())
}

// OpenAITokenCache token cache interface.
type OpenAITokenCache = GeminiTokenCache

// OpenAITokenProvider manages access_token for OpenAI OAuth accounts.
type OpenAITokenProvider struct {
	accountRepo        AccountRepository
	tokenCache         OpenAITokenCache
	openAIOAuthService *OpenAIOAuthService
	runtimeBlocker     AccountRuntimeBlocker
	metrics            *openAITokenRuntimeMetricsStore
	refreshAPI         *OAuthRefreshAPI
	executor           OAuthRefreshExecutor
	refreshPolicy      ProviderRefreshPolicy
}

func NewOpenAITokenProvider(
	accountRepo AccountRepository,
	tokenCache OpenAITokenCache,
	openAIOAuthService *OpenAIOAuthService,
) *OpenAITokenProvider {
	return &OpenAITokenProvider{
		accountRepo:        accountRepo,
		tokenCache:         tokenCache,
		openAIOAuthService: openAIOAuthService,
		metrics:            &openAITokenRuntimeMetricsStore{},
		refreshPolicy:      OpenAIProviderRefreshPolicy(),
	}
}

// SetRefreshAPI injects unified OAuth refresh API and executor.
func (p *OpenAITokenProvider) SetRefreshAPI(api *OAuthRefreshAPI, executor OAuthRefreshExecutor) {
	p.refreshAPI = api
	p.executor = executor
}

// SetRefreshPolicy injects caller-side refresh policy.
func (p *OpenAITokenProvider) SetRefreshPolicy(policy ProviderRefreshPolicy) {
	p.refreshPolicy = policy
}

func (p *OpenAITokenProvider) SetAccountRuntimeBlocker(blocker AccountRuntimeBlocker) {
	p.runtimeBlocker = blocker
}

func (p *OpenAITokenProvider) SnapshotRuntimeMetrics() OpenAITokenRuntimeMetrics {
	if p == nil {
		return OpenAITokenRuntimeMetrics{}
	}
	p.ensureMetrics()
	return p.metrics.snapshot()
}

func (p *OpenAITokenProvider) ensureMetrics() {
	if p != nil && p.metrics == nil {
		p.metrics = &openAITokenRuntimeMetricsStore{}
	}
}

// GetAccessToken returns a valid access_token.
func (p *OpenAITokenProvider) GetAccessToken(ctx context.Context, account *Account) (string, error) {
	p.ensureMetrics()
	if account == nil {
		return "", errors.New("account is nil")
	}
	if account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth {
		return "", errors.New("not an openai oauth account")
	}

	cacheKey := OpenAITokenCacheKey(account)

	// 1) Try cache first.
	if p.tokenCache != nil {
		if token, err := p.tokenCache.GetAccessToken(ctx, cacheKey); err == nil && strings.TrimSpace(token) != "" {
			slog.Debug("openai_token_cache_hit", "account_id", account.ID)
			return token, nil
		} else if err != nil {
			slog.Warn("openai_token_cache_get_failed", "account_id", account.ID, "error", err)
		}
	}

	slog.Debug("openai_token_cache_miss", "account_id", account.ID)

	// 2) Refresh if needed (pre-expiry skew).
	expiresAt := account.GetCredentialAsTime("expires_at")
	needsRefresh := !account.IsOpenAIPersonalAccessToken() && (expiresAt == nil || time.Until(*expiresAt) <= openAITokenRefreshSkew)
	if needsRefresh && strings.TrimSpace(account.GetOpenAIRefreshToken()) == "" {
		if expiresAt != nil && !time.Now().Before(*expiresAt) {
			const reason = "openai access_token expired and refresh_token is missing"
			// 永久故障：缺失 refresh_token 时账号无法自愈，必须立即从调度池剔除，
			// 否则会被反复选中、每次都在 token 阶段直接返回错误，对用户呈现持续 502。
			p.disableAccountMissingRefreshToken(account, reason)
			return "", errors.New(reason)
		}
		needsRefresh = false
	}
	refreshFailed := false

	if needsRefresh && p.refreshAPI != nil && p.executor != nil {
		p.metrics.refreshRequests.Add(1)
		p.metrics.touchNow()

		result, err := p.refreshAPI.RefreshIfNeeded(ctx, account, p.executor, openAITokenRefreshSkew)
		if err != nil {
			if p.refreshPolicy.OnRefreshError == ProviderRefreshErrorReturn {
				return "", err
			}
			slog.Warn("openai_token_refresh_failed", "account_id", account.ID, "error", err)
			p.metrics.refreshFailure.Add(1)
			refreshFailed = true
		} else if result.LockHeld {
			if p.refreshPolicy.OnLockHeld == ProviderLockHeldWaitForCache {
				p.metrics.lockContention.Add(1)
				p.metrics.touchNow()
				token, waitErr := p.waitForTokenAfterLockRace(ctx, cacheKey)
				if waitErr != nil {
					return "", waitErr
				}
				if strings.TrimSpace(token) != "" {
					slog.Debug("openai_token_cache_hit_after_wait", "account_id", account.ID)
					return token, nil
				}
			}
		} else if result.Refreshed {
			p.metrics.refreshSuccess.Add(1)
			account = result.Account
			expiresAt = account.GetCredentialAsTime("expires_at")
		} else {
			account = result.Account
			expiresAt = account.GetCredentialAsTime("expires_at")
		}
	} else if needsRefresh && p.tokenCache != nil {
		// Backward-compatible test path when refreshAPI is not injected.
		p.metrics.refreshRequests.Add(1)
		p.metrics.touchNow()
		locked, lockErr := p.tokenCache.AcquireRefreshLock(ctx, cacheKey, 30*time.Second)
		if lockErr == nil && locked {
			defer func() { _ = p.tokenCache.ReleaseRefreshLock(ctx, cacheKey) }()
		} else if lockErr != nil {
			p.metrics.lockAcquireFailure.Add(1)
			p.metrics.touchNow()
			slog.Warn("openai_token_lock_failed", "account_id", account.ID, "error", lockErr)
		} else {
			p.metrics.lockContention.Add(1)
			p.metrics.touchNow()
			token, waitErr := p.waitForTokenAfterLockRace(ctx, cacheKey)
			if waitErr != nil {
				return "", waitErr
			}
			if strings.TrimSpace(token) != "" {
				slog.Debug("openai_token_cache_hit_after_wait", "account_id", account.ID)
				return token, nil
			}
		}
	}

	accessToken := account.GetCredential("access_token")
	if strings.TrimSpace(accessToken) == "" {
		return "", errors.New("access_token not found in credentials")
	}

	// 3) Populate cache with TTL.
	if p.tokenCache != nil {
		latestAccount, isStale := CheckTokenVersion(ctx, account, p.accountRepo)
		if isStale && latestAccount != nil {
			slog.Debug("openai_token_version_stale_use_latest", "account_id", account.ID)
			accessToken = latestAccount.GetOpenAIAccessToken()
			if strings.TrimSpace(accessToken) == "" {
				return "", errors.New("access_token not found after version check")
			}
		} else {
			ttl := 30 * time.Minute
			if refreshFailed {
				if p.refreshPolicy.FailureTTL > 0 {
					ttl = p.refreshPolicy.FailureTTL
				} else {
					ttl = time.Minute
				}
				slog.Debug("openai_token_cache_short_ttl", "account_id", account.ID, "reason", "refresh_failed")
			} else if expiresAt != nil {
				until := time.Until(*expiresAt)
				switch {
				case until > openAITokenCacheSkew:
					ttl = until - openAITokenCacheSkew
				case until > 0:
					ttl = until
				default:
					ttl = time.Minute
				}
			}
			if err := p.tokenCache.SetAccessToken(ctx, cacheKey, accessToken, ttl); err != nil {
				slog.Warn("openai_token_cache_set_failed", "account_id", account.ID, "error", err)
			}
		}
	}

	return accessToken, nil
}

// disableAccountMissingRefreshToken 在请求路径上发现 OpenAI OAuth 账号
// 凭证已过期且 refresh_token 缺失时，将账号标记为 error 状态。
// 这是一种永久性故障：仅靠后续请求或 TokenRefreshService 不会自愈
// （NeedsRefresh 也会因 refresh_token 为空直接跳过），
// 必须主动剔除以避免账号被持续选中导致用户端反复 502。
// 使用 background context 是因为请求 context 可能很快结束。
func (p *OpenAITokenProvider) disableAccountMissingRefreshToken(account *Account, reason string) {
	if p == nil || p.accountRepo == nil || account == nil {
		return
	}
	if p.runtimeBlocker != nil {
		p.runtimeBlocker.BlockAccountScheduling(account, time.Time{}, "missing_refresh_token")
	}
	bgCtx := context.Background()
	if err := p.accountRepo.SetError(bgCtx, account.ID, reason); err != nil {
		slog.Warn("openai_token_provider.set_error_failed",
			"account_id", account.ID,
			"error", err,
		)
		return
	}
	if p.tokenCache != nil {
		cacheKey := OpenAITokenCacheKey(account)
		if err := p.tokenCache.DeleteAccessToken(bgCtx, cacheKey); err != nil {
			slog.Warn("openai_token_provider.cache_delete_failed",
				"account_id", account.ID,
				"error", err,
			)
		}
	}
	slog.Warn("openai_token_provider.account_disabled_missing_refresh_token",
		"account_id", account.ID,
		"reason", reason,
	)
}

func (p *OpenAITokenProvider) waitForTokenAfterLockRace(ctx context.Context, cacheKey string) (string, error) {
	wait := openAILockInitialWait
	totalWaitMs := int64(0)
	for i := 0; i < openAILockMaxAttempts; i++ {
		actualWait := jitterLockWait(wait)
		timer := time.NewTimer(actualWait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return "", ctx.Err()
		case <-timer.C:
		}

		waitMs := actualWait.Milliseconds()
		if waitMs < 0 {
			waitMs = 0
		}
		totalWaitMs += waitMs
		p.metrics.lockWaitSamples.Add(1)
		p.metrics.lockWaitTotalMs.Add(waitMs)
		p.metrics.touchNow()

		token, err := p.tokenCache.GetAccessToken(ctx, cacheKey)
		if err == nil && strings.TrimSpace(token) != "" {
			p.metrics.lockWaitHit.Add(1)
			if totalWaitMs >= openAILockWarnThresholdMs {
				slog.Warn("openai_token_lock_wait_high", "wait_ms", totalWaitMs, "attempts", i+1)
			}
			return token, nil
		}

		if wait < openAILockMaxWait {
			wait *= 2
			if wait > openAILockMaxWait {
				wait = openAILockMaxWait
			}
		}
	}

	p.metrics.lockWaitMiss.Add(1)
	if totalWaitMs >= openAILockWarnThresholdMs {
		slog.Warn("openai_token_lock_wait_high", "wait_ms", totalWaitMs, "attempts", openAILockMaxAttempts)
	}
	return "", nil
}

func jitterLockWait(base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	minFactor := 1 - openAILockJitterRatio
	maxFactor := 1 + openAILockJitterRatio
	factor := minFactor + rand.Float64()*(maxFactor-minFactor)
	return time.Duration(float64(base) * factor)
}

// forcedRefreshExecutor 包装 executor，让 NeedsRefresh 恒为 true（手动刷新）。
type forcedRefreshExecutor struct {
	OAuthRefreshExecutor
}

func (e forcedRefreshExecutor) NeedsRefresh(account *Account, _ time.Duration) bool {
	return e.OAuthRefreshExecutor.CanRefresh(account)
}

func (e forcedRefreshExecutor) RequiresDistributedRefreshLock(account *Account) bool {
	return executorRequiresDistributedLock(e.OAuthRefreshExecutor, account)
}

func (e forcedRefreshExecutor) PersistsDetached(account *Account) bool {
	return executorPersistsDetached(e.OAuthRefreshExecutor, account)
}

// rejectedTokenRefreshExecutor 包装 executor：只有锁内回读到的 access_token
// 仍等于被上游拒绝的那一个才刷新；已被别的副本换掉就直接复用新凭据。
type rejectedTokenRefreshExecutor struct {
	OAuthRefreshExecutor
	rejectedAccessToken string
}

func (e rejectedTokenRefreshExecutor) NeedsRefresh(account *Account, _ time.Duration) bool {
	if !e.OAuthRefreshExecutor.CanRefresh(account) {
		return false
	}
	current := strings.TrimSpace(account.GetCredential("access_token"))
	return current == "" || current == strings.TrimSpace(e.rejectedAccessToken)
}

func (e rejectedTokenRefreshExecutor) RequiresDistributedRefreshLock(account *Account) bool {
	return executorRequiresDistributedLock(e.OAuthRefreshExecutor, account)
}

func (e rejectedTokenRefreshExecutor) PersistsDetached(account *Account) bool {
	return executorPersistsDetached(e.OAuthRefreshExecutor, account)
}

// openAIForcedRefreshTimeout 手动 / 401 恢复刷新的总超时，脱离请求 ctx。
const openAIForcedRefreshTimeout = 45 * time.Second

func (p *OpenAITokenProvider) runLockedRefresh(parent context.Context, account *Account, executor OAuthRefreshExecutor) (*OAuthRefreshResult, error) {
	if p == nil || p.refreshAPI == nil || p.executor == nil {
		return nil, errors.New("openai token provider refresh api is not configured")
	}
	if account == nil {
		return nil, errors.New("account is nil")
	}
	base := context.Background()
	if parent != nil {
		base = context.WithoutCancel(parent)
	}
	ctx, cancel := context.WithTimeout(withOAuthRefreshRequestPath(base), openAIForcedRefreshTimeout)
	defer cancel()

	cacheKey := OpenAITokenCacheKey(account)
	result, err := p.refreshAPI.RefreshIfNeeded(ctx, account, executor, openAITokenRefreshSkew)
	if err != nil {
		return nil, err
	}
	if result != nil && result.Refreshed && p.tokenCache != nil {
		// 旧缓存可能仍指向被轮转前的 access_token，刷新成功后立即清掉。
		if delErr := p.tokenCache.DeleteAccessToken(ctx, cacheKey); delErr != nil {
			slog.Warn("openai_token_provider.cache_delete_after_refresh_failed", "account_id", account.ID, "error", delErr)
		}
	}
	return result, nil
}

// ForceRefresh 在统一锁保护下无条件刷新一次（管理端手动刷新、导入后 RT 所有权转移）。
// 锁被其它副本持有时返回错误而不是绕锁刷新。
func (p *OpenAITokenProvider) ForceRefresh(ctx context.Context, account *Account) (*Account, error) {
	result, err := p.runLockedRefresh(ctx, account, forcedRefreshExecutor{p.executor})
	if err != nil {
		return nil, err
	}
	if result.LockHeld {
		return nil, errors.New("token refresh is already in progress on another instance; retry shortly")
	}
	if result.Account == nil {
		return nil, errors.New("token refresh returned no account")
	}
	return result.Account, nil
}

// RecoverAfterUnauthorized 上游 401 后的即时恢复：锁内回读 DB，
// 只有当前 access_token 仍是被拒的那一个才刷新，避免多副本各刷一次轮转两次 RT。
// 返回 recovered=true 表示账号已持有可用的新凭据（本副本刷新或别的副本已刷新）。
func (p *OpenAITokenProvider) RecoverAfterUnauthorized(ctx context.Context, account *Account, rejectedAccessToken string) (bool, error) {
	result, err := p.runLockedRefresh(ctx, account, rejectedTokenRefreshExecutor{
		OAuthRefreshExecutor: p.executor,
		rejectedAccessToken:  rejectedAccessToken,
	})
	if err != nil {
		return false, err
	}
	if result.LockHeld {
		// 别的副本正在刷新，交给它；本请求换号。
		return true, nil
	}
	if result.Refreshed {
		return true, nil
	}
	if result.Account != nil {
		current := strings.TrimSpace(result.Account.GetCredential("access_token"))
		if current != "" && current != strings.TrimSpace(rejectedAccessToken) {
			return true, nil
		}
	}
	return false, nil
}
