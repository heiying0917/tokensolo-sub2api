//go:build unit

package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// lockedRefreshExecutorStub 在基础桩上声明 fail-closed 与脱离 ctx 落库。
type lockedRefreshExecutorStub struct {
	refreshAPIExecutorStub
	requiresLock    bool
	persistDetached bool
}

func (e *lockedRefreshExecutorStub) RequiresDistributedRefreshLock(*Account) bool {
	return e.requiresLock
}
func (e *lockedRefreshExecutorStub) PersistsDetached(*Account) bool { return e.persistDetached }

// ownedLockCacheStub 实现 OwnedRefreshLockCache，记录 owner。
type ownedLockCacheStub struct {
	refreshAPICacheStub
	acquireOwner string
	releaseOwner string
	ownedAcquire int
	ownedRelease int
}

func (c *ownedLockCacheStub) AcquireRefreshLockOwned(_ context.Context, _ string, owner string, _ time.Duration) (bool, error) {
	c.ownedAcquire++
	c.acquireOwner = owner
	return c.lockResult, c.lockErr
}

func (c *ownedLockCacheStub) ReleaseRefreshLockOwned(_ context.Context, _ string, owner string) error {
	c.ownedRelease++
	c.releaseOwner = owner
	return nil
}

// setErrorRecordingRepo 记录 SetError 调用。
type setErrorRecordingRepo struct {
	refreshAPIAccountRepo
	setErrorCalls int
	lastErrorMsg  string
}

func (r *setErrorRecordingRepo) SetError(_ context.Context, _ int64, msg string) error {
	r.setErrorCalls++
	r.lastErrorMsg = msg
	return nil
}

func siwcRefreshTestAccount(id int64) *Account {
	return &Account{
		ID: id, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
		Credentials: map[string]any{"auth_mode": "siwc", "client_id": "oaiapp_x", "refresh_token": "rt-old", "access_token": "at-old"},
	}
}

func TestRefreshIfNeeded_RequiresLock_LockErrorFailsClosed(t *testing.T) {
	account := siwcRefreshTestAccount(1)
	repo := &refreshAPIAccountRepo{account: account}
	cache := &refreshAPICacheStub{lockErr: errors.New("redis down")}
	executor := &lockedRefreshExecutorStub{
		refreshAPIExecutorStub: refreshAPIExecutorStub{needsRefresh: true, credentials: map[string]any{"access_token": "new"}},
		requiresLock:           true,
	}

	api := NewOAuthRefreshAPI(repo, cache)
	result, err := api.RefreshIfNeeded(context.Background(), account, executor, time.Minute)

	require.Nil(t, result)
	require.ErrorIs(t, err, errOAuthRefreshLockUnavailable)
	var containment *providerCycleContainmentRefreshError
	require.ErrorAs(t, err, &containment)
	require.Equal(t, 0, executor.refreshCalls, "拿不到分布式锁时绝不能刷新一次性轮转的 RT")
	require.Equal(t, 0, repo.updateCalls)
}

func TestRefreshIfNeeded_RequiresLock_NoCacheFailsClosed(t *testing.T) {
	account := siwcRefreshTestAccount(2)
	repo := &refreshAPIAccountRepo{account: account}
	executor := &lockedRefreshExecutorStub{
		refreshAPIExecutorStub: refreshAPIExecutorStub{needsRefresh: true, credentials: map[string]any{"access_token": "new"}},
		requiresLock:           true,
	}

	api := NewOAuthRefreshAPI(repo, nil)
	_, err := api.RefreshIfNeeded(context.Background(), account, executor, time.Minute)

	require.ErrorIs(t, err, errOAuthRefreshLockUnavailable)
	require.Equal(t, 0, executor.refreshCalls)
}

func TestRefreshIfNeeded_NonLockRequiringExecutorStillDegrades(t *testing.T) {
	// 回归保护：不声明 fail-closed 的 executor 保持原有降级行为
	account := &Account{ID: 3, Platform: PlatformGemini, Type: AccountTypeOAuth, Status: StatusActive}
	repo := &refreshAPIAccountRepo{account: account}
	cache := &refreshAPICacheStub{lockErr: errors.New("redis down")}
	executor := &lockedRefreshExecutorStub{
		refreshAPIExecutorStub: refreshAPIExecutorStub{needsRefresh: true, credentials: map[string]any{"access_token": "degraded"}},
		requiresLock:           false,
	}
	api := NewOAuthRefreshAPI(repo, cache)
	result, err := api.RefreshIfNeeded(context.Background(), account, executor, time.Minute)
	require.NoError(t, err)
	require.True(t, result.Refreshed)
	require.Equal(t, 1, executor.refreshCalls)
}

func TestRefreshIfNeeded_OwnedLockReleasesWithSameOwner(t *testing.T) {
	account := siwcRefreshTestAccount(4)
	repo := &refreshAPIAccountRepo{account: account}
	cache := &ownedLockCacheStub{refreshAPICacheStub: refreshAPICacheStub{lockResult: true}}
	executor := &lockedRefreshExecutorStub{
		refreshAPIExecutorStub: refreshAPIExecutorStub{needsRefresh: true, credentials: map[string]any{"access_token": "new"}},
		requiresLock:           true,
	}

	api := NewOAuthRefreshAPI(repo, cache)
	result, err := api.RefreshIfNeeded(context.Background(), account, executor, time.Minute)

	require.NoError(t, err)
	require.True(t, result.Refreshed)
	require.Equal(t, 1, cache.ownedAcquire)
	require.Equal(t, 1, cache.ownedRelease)
	require.NotEmpty(t, cache.acquireOwner)
	require.Equal(t, cache.acquireOwner, cache.releaseOwner, "释放必须带上获取时的 owner")
	require.Equal(t, 0, cache.releaseCalls, "不应再走无主 DEL")
}

func TestRefreshIfNeeded_OwnedLockHeldByPeer(t *testing.T) {
	account := siwcRefreshTestAccount(5)
	repo := &refreshAPIAccountRepo{account: account}
	cache := &ownedLockCacheStub{refreshAPICacheStub: refreshAPICacheStub{lockResult: false}}
	executor := &lockedRefreshExecutorStub{
		refreshAPIExecutorStub: refreshAPIExecutorStub{needsRefresh: true, credentials: map[string]any{"access_token": "new"}},
		requiresLock:           true,
	}
	api := NewOAuthRefreshAPI(repo, cache)
	result, err := api.RefreshIfNeeded(context.Background(), account, executor, time.Minute)
	require.NoError(t, err)
	require.True(t, result.LockHeld)
	require.Equal(t, 0, executor.refreshCalls)
	require.Equal(t, 0, cache.ownedRelease)
}

func TestRefreshIfNeeded_PersistsDetached_AfterCtxCancel(t *testing.T) {
	account := siwcRefreshTestAccount(6)
	repo := &refreshAPIAccountRepo{account: account}
	cache := &refreshAPICacheStub{lockResult: true}
	executor := &lockedRefreshExecutorStub{
		refreshAPIExecutorStub: refreshAPIExecutorStub{
			needsRefresh: true,
			credentials:  map[string]any{"access_token": "at-new", "refresh_token": "rt-new"},
			delay:        30 * time.Millisecond, // 刷新期间调用方 ctx 到期
		},
		requiresLock:    true,
		persistDetached: true,
	}

	api := NewOAuthRefreshAPI(repo, cache)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	result, err := api.RefreshIfNeeded(ctx, account, executor, time.Hour)

	require.NoError(t, err, "一次性轮转的 RT 已在上游作废，ctx 取消也必须落库")
	require.True(t, result.Refreshed)
	require.Equal(t, 1, repo.updateCredentialsCalls)
	require.Equal(t, "rt-new", repo.account.GetCredential("refresh_token"))
	require.NotEmpty(t, repo.account.Credentials["_token_version"])
}

func TestRefreshIfNeeded_NonDetachedExecutorStillDropsLateCredentials(t *testing.T) {
	account := siwcRefreshTestAccount(7)
	repo := &refreshAPIAccountRepo{account: account}
	executor := &lockedRefreshExecutorStub{
		refreshAPIExecutorStub: refreshAPIExecutorStub{needsRefresh: true, credentials: map[string]any{"access_token": "late"}, delay: 30 * time.Millisecond},
	}
	api := NewOAuthRefreshAPI(repo, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := api.RefreshIfNeeded(ctx, account, executor, time.Hour)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Zero(t, repo.updateCredentialsCalls)
}

func TestRefreshIfNeeded_PersistsDetached_PersistFailureSetsError(t *testing.T) {
	account := siwcRefreshTestAccount(8)
	repo := &setErrorRecordingRepo{refreshAPIAccountRepo: refreshAPIAccountRepo{account: account, updateErr: errors.New("pg down")}}
	cache := &refreshAPICacheStub{lockResult: true}
	executor := &lockedRefreshExecutorStub{
		refreshAPIExecutorStub: refreshAPIExecutorStub{needsRefresh: true, credentials: map[string]any{"access_token": "at-new", "refresh_token": "rt-new"}},
		requiresLock:           true,
		persistDetached:        true,
	}

	api := NewOAuthRefreshAPI(repo, cache)
	_, err := api.RefreshIfNeeded(context.Background(), account, executor, time.Minute)

	require.ErrorIs(t, err, errOAuthRefreshCredentialPersist)
	var containment *providerCycleContainmentRefreshError
	require.ErrorAs(t, err, &containment)
	require.Equal(t, 1+len(refreshDetachedPersistBackoffs), repo.updateCalls, "应重试落库")
	require.Equal(t, 1, repo.setErrorCalls, "落库彻底失败时账号实际已死，必须标 error")
	require.Contains(t, repo.lastErrorMsg, "re-authorize")
}

// ---------- 后台刷新 leader 锁 ----------

type leaderLockCacheStub struct {
	mu      sync.Mutex
	holder  string
	acquire int
	release int
}

func (c *leaderLockCacheStub) TryAcquireLeaderLock(_ context.Context, _ string, owner string, _ time.Duration) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.acquire++
	if c.holder == "" || c.holder == owner {
		c.holder = owner
		return true, nil
	}
	return false, nil
}

func (c *leaderLockCacheStub) ReleaseLeaderLock(_ context.Context, _ string, owner string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.release++
	if c.holder == owner {
		c.holder = ""
	}
	return nil
}

type leaderPagerRepo struct {
	mockAccountRepoForGemini
	pageCalls int
}

func (r *leaderPagerRepo) ListOAuthRefreshCandidatePage(_ context.Context, _ OAuthRefreshPageOptions) (*OAuthRefreshCandidatePage, error) {
	r.pageCalls++
	return &OAuthRefreshCandidatePage{}, nil
}

func TestTokenRefreshService_CycleRunsOnlyOnLeader(t *testing.T) {
	cfg := &config.Config{}
	repo := &leaderPagerRepo{}
	lock := &leaderLockCacheStub{}

	a := NewTokenRefreshService(repo, nil, nil, nil, nil, nil, nil, cfg, nil)
	a.SetLeaderLock(lock, nil)
	b := NewTokenRefreshService(repo, nil, nil, nil, nil, nil, nil, cfg, nil)
	b.SetLeaderLock(lock, nil)

	// 模拟 b 先持锁：a 的周期必须跳过
	lock.holder = b.instanceID
	a.processRefreshContext(context.Background())
	require.Equal(t, 0, repo.pageCalls, "非 leader 不能扫描账号")

	// b 自己是 holder，可执行；执行完释放
	b.processRefreshContext(context.Background())
	require.Equal(t, 1, repo.pageCalls)
	require.Equal(t, "", lock.holder, "周期结束应释放 leader 锁")

	// 锁释放后 a 可执行
	a.processRefreshContext(context.Background())
	require.Equal(t, 2, repo.pageCalls)
}

func TestTokenRefreshService_NoLeaderLockRunsUngated(t *testing.T) {
	repo := &leaderPagerRepo{}
	svc := NewTokenRefreshService(repo, nil, nil, nil, nil, nil, nil, &config.Config{}, nil)
	svc.processRefreshContext(context.Background())
	require.Equal(t, 1, repo.pageCalls)
}
