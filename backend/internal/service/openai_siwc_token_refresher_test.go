//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestOpenAISIWCTokenRefresher_NeedsRefreshWindowCap(t *testing.T) {
	client := &siwcOAuthClientStub{}
	svc := NewOpenAISIWCOAuthService(nil, client, nil, nil)
	r := NewOpenAISIWCTokenRefresher(svc)

	acc := siwcRefreshTestAccount(1)
	require.True(t, r.CanRefresh(acc))

	// 缺 expires_at：未知即刷
	require.True(t, r.NeedsRefresh(acc, time.Hour))

	// 30 分钟后过期，后台窗口 1 小时：被封顶到 10 分钟，不刷
	acc.Credentials["expires_at"] = time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339)
	require.False(t, r.NeedsRefresh(acc, time.Hour))

	// 5 分钟后过期：刷
	acc.Credentials["expires_at"] = time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)
	require.True(t, r.NeedsRefresh(acc, time.Hour))

	// 请求路径的 3 分钟窗口：8 分钟后过期不刷
	acc.Credentials["expires_at"] = time.Now().Add(8 * time.Minute).UTC().Format(time.RFC3339)
	require.False(t, r.NeedsRefresh(acc, 3*time.Minute))

	// 缺 client_id 不可刷
	delete(acc.Credentials, "client_id")
	require.False(t, r.CanRefresh(acc))
	require.False(t, r.NeedsRefresh(acc, time.Hour))

	require.True(t, r.RequiresDistributedRefreshLock(siwcRefreshTestAccount(2)))
	require.True(t, r.PersistsDetached(siwcRefreshTestAccount(2)))
	require.False(t, r.RequiresDistributedRefreshLock(&Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}))
}

func TestOpenAISIWCTokenRefresher_RefreshMergesCredentials(t *testing.T) {
	client := &siwcOAuthClientStub{}
	svc := NewOpenAISIWCOAuthService(nil, client, nil, nil)
	r := NewOpenAISIWCTokenRefresher(svc)
	acc := siwcRefreshTestAccount(3)
	acc.Credentials["email"] = "a@x.com"

	creds, err := r.Refresh(context.Background(), acc)
	require.NoError(t, err)
	require.Equal(t, "siwc-at-2", creds["access_token"])
	require.Equal(t, "siwc-rt-2", creds["refresh_token"])
	require.Equal(t, "siwc", creds["auth_mode"], "原有字段必须保留")
	require.Equal(t, "oaiapp_x", creds["client_id"])
	require.Equal(t, "a@x.com", creds["email"])
}

func TestOpenAITokenRefresherComposite_Dispatch(t *testing.T) {
	siwcSvc := NewOpenAISIWCOAuthService(nil, &siwcOAuthClientStub{}, nil, nil)
	standard := NewOpenAITokenRefresher(NewOpenAIOAuthService(nil, &openaiOAuthClientStateStub{}), nil)
	composite := NewOpenAITokenRefresherComposite(standard, NewOpenAISIWCTokenRefresher(siwcSvc))

	siwc := siwcRefreshTestAccount(10)
	codex := &Account{ID: 11, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
		Credentials: map[string]any{"refresh_token": "rt", "access_token": "at"}}

	// 标准刷新器拒绝 SIWC 账号
	require.False(t, standard.CanRefresh(siwc))
	require.True(t, standard.CanRefresh(codex))

	// 组合刷新器按 auth_mode 分派
	require.True(t, composite.CanRefresh(siwc))
	require.True(t, composite.CanRefresh(codex))
	require.True(t, composite.NeedsRefresh(siwc, time.Hour), "SIWC 缺 expires_at 即刷")
	require.False(t, composite.NeedsRefresh(codex, time.Hour), "Codex 缺 expires_at 且未限流不刷")
	require.Equal(t, OpenAITokenCacheKey(siwc), composite.CacheKey(siwc))

	lockReq, ok := composite.(distributedLockRequiredRefreshExecutor)
	require.True(t, ok)
	require.True(t, lockReq.RequiresDistributedRefreshLock(siwc))
	require.False(t, lockReq.RequiresDistributedRefreshLock(codex))

	creds, err := composite.Refresh(context.Background(), siwc)
	require.NoError(t, err)
	require.Equal(t, "siwc-at-2", creds["access_token"])

	// siwc 为 nil 时退化为标准刷新器
	require.Same(t, standard, NewOpenAITokenRefresherComposite(standard, nil))
}

func TestOpenAIOAuthService_RefreshAccountToken_RejectsSIWC(t *testing.T) {
	svc := NewOpenAIOAuthService(nil, &openaiOAuthClientStateStub{})
	_, err := svc.RefreshAccountToken(context.Background(), siwcRefreshTestAccount(1))
	require.Error(t, err)
	require.Contains(t, err.Error(), "locked token provider path")
}

func TestOpenAITokenProvider_ForceRefreshAndRecoverAfterUnauthorized(t *testing.T) {
	client := &siwcOAuthClientStub{}
	siwcSvc := NewOpenAISIWCOAuthService(nil, client, nil, nil)
	standard := NewOpenAITokenRefresher(NewOpenAIOAuthService(nil, &openaiOAuthClientStateStub{}), nil)
	composite := NewOpenAITokenRefresherComposite(standard, NewOpenAISIWCTokenRefresher(siwcSvc))

	account := siwcRefreshTestAccount(20)
	account.Credentials["expires_at"] = time.Now().Add(50 * time.Minute).UTC().Format(time.RFC3339)
	repo := &refreshAPIAccountRepo{account: account}
	cache := &refreshAPICacheStub{lockResult: true}
	api := NewOAuthRefreshAPI(repo, cache)

	provider := NewOpenAITokenProvider(repo, cache, nil)
	provider.SetRefreshAPI(api, composite)

	// ForceRefresh：即使未到期也刷新；请求 ctx 已取消也能完成
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	refreshed, err := provider.ForceRefresh(ctx, account)
	require.NoError(t, err)
	require.Equal(t, "siwc-at-2", refreshed.GetCredential("access_token"))
	require.Equal(t, "siwc-rt-2", refreshed.GetCredential("refresh_token"))
	require.Equal(t, int32(1), client.refreshCalls)
	require.Equal(t, 1, cache.deleteCalls, "刷新后应清掉旧 token 缓存")

	// RecoverAfterUnauthorized：DB 里的 AT 已不是被拒的那一个 → 不刷，直接恢复
	recovered, err := provider.RecoverAfterUnauthorized(context.Background(), account, "at-old")
	require.NoError(t, err)
	require.True(t, recovered)
	require.Equal(t, int32(1), client.refreshCalls, "别的副本已换过 token，不应再刷")

	// DB 里的 AT 仍是被拒的那一个 → 刷新
	recovered, err = provider.RecoverAfterUnauthorized(context.Background(), account, "siwc-at-2")
	require.NoError(t, err)
	require.True(t, recovered)
	require.Equal(t, int32(2), client.refreshCalls)

	// 锁被别的副本持有：ForceRefresh 报错，Recover 视为已恢复（交给持锁者）
	cache.lockResult = false
	_, err = provider.ForceRefresh(context.Background(), account)
	require.Error(t, err)
	recovered, err = provider.RecoverAfterUnauthorized(context.Background(), account, "siwc-at-2")
	require.NoError(t, err)
	require.True(t, recovered)
	require.Equal(t, int32(2), client.refreshCalls)
}

func TestRateLimitService_SIWC401UsesRecoverer(t *testing.T) {
	t.Run("recovered_switches_account_without_cooldown", func(t *testing.T) {
		repo := &rateLimitAccountRepoStub{}
		invalidator := &tokenCacheInvalidatorRecorder{}
		recoverer := &siwcRecovererStub{recovered: true}
		service := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
		service.SetTokenCacheInvalidator(invalidator)
		service.SetOpenAISIWCUnauthorizedRecoverer(recoverer)
		account := siwcRefreshTestAccount(30)

		shouldDisable := service.HandleUpstreamError(context.Background(), account, 401, nil, []byte(`{"error":{"message":"expired"}}`))

		require.True(t, shouldDisable, "本请求换号重试")
		require.Equal(t, 1, recoverer.calls)
		require.Equal(t, "at-old", recoverer.lastRejected)
		require.Equal(t, 0, repo.tempCalls, "恢复成功不进冷却")
		require.Equal(t, 0, repo.setErrorCalls)
		require.Len(t, invalidator.accounts, 1)
	})

	t.Run("not_recovered_falls_back_to_oauth_cooldown", func(t *testing.T) {
		repo := &rateLimitAccountRepoStub{}
		recoverer := &siwcRecovererStub{recovered: false}
		service := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
		service.SetOpenAISIWCUnauthorizedRecoverer(recoverer)
		account := siwcRefreshTestAccount(31)

		shouldDisable := service.HandleUpstreamError(context.Background(), account, 401, nil, []byte(`unauthorized`))

		require.True(t, shouldDisable)
		require.Equal(t, 1, recoverer.calls)
		require.Equal(t, 1, repo.tempCalls, "恢复失败走通用 OAuth 401 冷却")
		require.Equal(t, 0, repo.setErrorCalls)
	})

	t.Run("codex_account_not_routed_to_recoverer", func(t *testing.T) {
		repo := &rateLimitAccountRepoStub{}
		recoverer := &siwcRecovererStub{recovered: true}
		service := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
		service.SetOpenAISIWCUnauthorizedRecoverer(recoverer)
		account := &Account{ID: 32, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
			Credentials: map[string]any{"refresh_token": "rt", "access_token": "at"}}

		service.HandleUpstreamError(context.Background(), account, 401, nil, []byte(`unauthorized`))
		require.Equal(t, 0, recoverer.calls)
		require.Equal(t, 1, repo.tempCalls)
	})
}

type siwcRecovererStub struct {
	recovered    bool
	calls        int
	lastRejected string
}

func (s *siwcRecovererStub) RecoverAfterUnauthorized(_ context.Context, _ *Account, rejected string) (bool, error) {
	s.calls++
	s.lastRejected = rejected
	return s.recovered, nil
}
