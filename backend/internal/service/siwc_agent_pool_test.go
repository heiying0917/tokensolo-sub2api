//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func siwcAgent(id int64, email, role string, schedulable bool) Account {
	acc := siwcTestAccount(id, email)
	acc.Schedulable = schedulable
	if role != "" {
		acc.Extra = map[string]any{siwcAgentRoleExtraKey: role}
	}
	return acc
}

func newAgentPoolForTest(accounts ...Account) (*SIWCAgentPoolService, *siwcGateRepoStub, *runtimeBlockerStub, *fakeLeaderLockCache) {
	repo := newSIWCGateRepoStub(accounts...)
	blocker := &runtimeBlockerStub{}
	lock := &fakeLeaderLockCache{}
	return NewSIWCAgentPoolService(repo, nil, lock, blocker), repo, blocker, lock
}

func schedulableIDs(repo *siwcGateRepoStub) []int64 {
	out := []int64{}
	for i := range repo.accounts {
		if repo.accounts[i].Schedulable {
			out = append(out, repo.accounts[i].ID)
		}
	}
	return out
}

func TestAccount_SIWCAgentRole(t *testing.T) {
	legacy := siwcTestAccount(1, "a@x.com")
	require.Equal(t, SIWCAgentRoleActive, legacy.GetOpenAISIWCAgentRole(), "缺失角色视为活跃，兼容旧行")
	standby := siwcAgent(2, "a@x.com", SIWCAgentRoleStandby, false)
	require.True(t, standby.IsOpenAISIWCStandby())
	codex := codexTestAccount(3, "a@x.com")
	require.Equal(t, "", codex.GetOpenAISIWCAgentRole())
}

func TestSIWCAgentPool_ActivateKeepsSingleActive(t *testing.T) {
	pool, repo, blocker, _ := newAgentPoolForTest(
		siwcAgent(1, "a@x.com", "", true),
		siwcAgent(2, "a@x.com", SIWCAgentRoleStandby, false),
		siwcAgent(3, "a@x.com", SIWCAgentRoleStandby, false),
		siwcAgent(9, "other@x.com", "", true),
	)
	group, err := pool.Activate(context.Background(), 2, "admin")
	require.NoError(t, err)
	require.Equal(t, int64(2), group.ActiveAccountID)
	require.ElementsMatch(t, []int64{2, 9}, schedulableIDs(repo), "同邮箱只剩 #2 可调度，其他邮箱不受影响")
	require.True(t, repo.accountsByID[1].IsOpenAISIWCStandby())
	require.Contains(t, blocker.cleared, int64(2))
}

func TestSIWCAgentPool_ActivateRejectsUnusableAndBusy(t *testing.T) {
	broken := siwcAgent(2, "a@x.com", SIWCAgentRoleStandby, false)
	broken.Status = StatusError
	pool, repo, _, lock := newAgentPoolForTest(siwcAgent(1, "a@x.com", "", true), broken)

	_, err := pool.Activate(context.Background(), 2, "admin")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not usable")
	require.ElementsMatch(t, []int64{1}, schedulableIDs(repo))

	// 另一副本正在切换同一账号
	_, _ = lock.TryAcquireLeaderLock(context.Background(), "siwc_agent:switch:a@x.com", "peer", time.Minute)
	_, err = pool.Activate(context.Background(), 1, "admin")
	require.Error(t, err)
	require.Contains(t, err.Error(), "in progress")
}

func TestSIWCAgentPool_FailoverPromotesStandby(t *testing.T) {
	pool, repo, _, _ := newAgentPoolForTest(
		siwcAgent(1, "a@x.com", SIWCAgentRoleActive, true),
		siwcAgent(2, "a@x.com", SIWCAgentRoleStandby, false),
		siwcAgent(3, "a@x.com", SIWCAgentRoleStandby, false),
	)
	require.NoError(t, repo.SetError(context.Background(), 1, "invalid_grant"))
	pool.Failover(context.Background(), repo.accountsByID[1])
	require.ElementsMatch(t, []int64{2}, schedulableIDs(repo), "按 ID 顺序切到第一个可用待命")
	require.True(t, repo.accountsByID[1].IsOpenAISIWCStandby(), "死掉的旧活跃降为待命")
}

func TestSIWCAgentPool_FailoverIgnoresStandbyAndNoCandidate(t *testing.T) {
	pool, repo, _, _ := newAgentPoolForTest(
		siwcAgent(1, "a@x.com", SIWCAgentRoleActive, true),
		siwcAgent(2, "a@x.com", SIWCAgentRoleStandby, false),
	)
	// 待命 Agent 失效：不动活跃
	require.NoError(t, repo.SetError(context.Background(), 2, "invalid_grant"))
	pool.Failover(context.Background(), repo.accountsByID[2])
	require.ElementsMatch(t, []int64{1}, schedulableIDs(repo))

	// 活跃也失效且无可用待命：只告警，不报错
	require.NoError(t, repo.SetError(context.Background(), 1, "invalid_grant"))
	pool.Failover(context.Background(), repo.accountsByID[1])
	require.Empty(t, schedulableIDs(repo))
}

func TestSIWCAgentPool_AssignNewAgentRole(t *testing.T) {
	pool, repo, _, _ := newAgentPoolForTest(siwcAgent(1, "a@x.com", "", true))

	// 已有可用活跃 → 新 Agent 待命
	repo.accounts = append(repo.accounts, siwcAgent(2, "a@x.com", "", true))
	repo.accountsByID = map[int64]*Account{}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}
	require.Equal(t, SIWCAgentRoleStandby, pool.AssignNewAgentRole(context.Background(), repo.accountsByID[2]))
	require.ElementsMatch(t, []int64{1}, schedulableIDs(repo))

	// 新邮箱首个 Agent → 活跃
	pool2, repo2, _, _ := newAgentPoolForTest(siwcAgent(5, "b@x.com", "", true))
	require.Equal(t, SIWCAgentRoleActive, pool2.AssignNewAgentRole(context.Background(), repo2.accountsByID[5]))
	require.ElementsMatch(t, []int64{5}, schedulableIDs(repo2))
}

func TestAdminSetAccountSchedulable_RejectsStandbySIWCAgent(t *testing.T) {
	repo := newSIWCGateRepoStub(siwcAgent(1, "a@x.com", SIWCAgentRoleStandby, false), siwcAgent(2, "a@x.com", "", false))
	svc := &adminServiceImpl{accountRepo: repo}

	_, err := svc.SetAccountSchedulable(context.Background(), 1, true)
	require.ErrorIs(t, err, ErrSIWCAgentStandbySchedulable)
	require.False(t, repo.accountsByID[1].Schedulable)

	// 关闭调度不受限；活跃 Agent 可正常打开
	_, err = svc.SetAccountSchedulable(context.Background(), 1, false)
	require.NoError(t, err)
	_, err = svc.SetAccountSchedulable(context.Background(), 2, true)
	require.NoError(t, err)
	require.True(t, repo.accountsByID[2].Schedulable)
}

// siwcAgentRepoStub 在闸门桩之上补删除 / 批量更新 / 分组绑定的记录。
type siwcAgentRepoStub struct {
	*siwcGateRepoStub
	deleted    map[int64]bool
	bulkIDs    []int64
	bulkUpdate AccountBulkUpdate
	bound      map[int64][]int64
}

func newSIWCAgentRepoStub(accounts ...Account) *siwcAgentRepoStub {
	return &siwcAgentRepoStub{siwcGateRepoStub: newSIWCGateRepoStub(accounts...), deleted: map[int64]bool{}, bound: map[int64][]int64{}}
}

func (r *siwcAgentRepoStub) ListByPlatform(ctx context.Context, platform string) ([]Account, error) {
	all, err := r.siwcGateRepoStub.ListByPlatform(ctx, platform)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, acc := range all {
		if !r.deleted[acc.ID] {
			out = append(out, acc)
		}
	}
	return out, nil
}

func (r *siwcAgentRepoStub) Delete(_ context.Context, id int64) error {
	r.deleted[id] = true
	return nil
}

func (r *siwcAgentRepoStub) BulkUpdate(_ context.Context, ids []int64, updates AccountBulkUpdate) (int64, error) {
	r.bulkIDs = append([]int64{}, ids...)
	r.bulkUpdate = updates
	return int64(len(ids)), nil
}

func (r *siwcAgentRepoStub) BindGroups(_ context.Context, accountID int64, groupIDs []int64) error {
	r.bound[accountID] = append([]int64{}, groupIDs...)
	return nil
}

func parentOf(repo *siwcAgentRepoStub, id int64) int64 {
	return repo.accountsByID[id].GetOpenAISIWCAgentParentID()
}

func TestSIWCAgentPool_NormalizeAllBuildsPrimaryChildTree(t *testing.T) {
	repo := newSIWCAgentRepoStub(
		siwcAgent(4, "a@x.com", SIWCAgentRoleStandby, false),
		siwcAgent(2, "a@x.com", "", true),
		siwcAgent(9, "b@x.com", "", true),
		codexTestAccount(1, "a@x.com"),
	)
	pool := NewSIWCAgentPoolService(repo, nil, &fakeLeaderLockCache{}, &runtimeBlockerStub{})
	pool.NormalizeAll(context.Background())

	require.Equal(t, int64(0), parentOf(repo, 2), "最小 ID 为主行")
	require.Equal(t, int64(2), parentOf(repo, 4))
	require.Equal(t, int64(0), parentOf(repo, 9), "单 Agent 账号自己是主行")
	require.Equal(t, int64(0), repo.accountsByID[1].GetOpenAISIWCAgentParentID(), "Codex 账号不参与")

	// 幂等：再跑一次不再写库
	before := len(repo.extras[4])
	pool.NormalizeAll(context.Background())
	require.Equal(t, before, len(repo.extras[4]))
}

func TestSIWCAgentPool_RemovePrimaryPromotesNext(t *testing.T) {
	primary := siwcAgent(2, "a@x.com", SIWCAgentRoleActive, true)
	repo := newSIWCAgentRepoStub(primary, siwcAgent(4, "a@x.com", SIWCAgentRoleStandby, false), siwcAgent(5, "a@x.com", SIWCAgentRoleStandby, false))
	pool := NewSIWCAgentPoolService(repo, nil, &fakeLeaderLockCache{}, &runtimeBlockerStub{})
	pool.NormalizeAll(context.Background())

	group, err := pool.RemoveAgent(context.Background(), 2, repo.Delete)
	require.NoError(t, err)
	require.True(t, repo.deleted[2])
	require.False(t, repo.deleted[4], "删主行不级联")
	require.Equal(t, int64(4), group.PrimaryAccountID)
	require.Equal(t, int64(0), parentOf(repo, 4))
	require.Equal(t, int64(4), parentOf(repo, 5))
	require.Equal(t, int64(4), group.ActiveAccountID, "删的是活跃 Agent：自动切到下一个")

	_, err = pool.RemoveAgent(context.Background(), 5, repo.Delete)
	require.NoError(t, err)
	_, err = pool.RemoveAgent(context.Background(), 4, repo.Delete)
	require.Error(t, err, "最后一个 Agent 只能删整个账号")
}

func TestPropagateSIWCAgentSettings_SyncsAccountLevelFields(t *testing.T) {
	proxyID := int64(7)
	rate := 1.5
	primary := siwcAgent(2, "a@x.com", "", true)
	primary.ProxyID = &proxyID
	primary.Priority = 3
	primary.Concurrency = 8
	primary.RateMultiplier = &rate
	primary.GroupIDs = []int64{11, 12}
	primary.Credentials["model_mapping"] = map[string]any{"gpt-6-luna": "gpt-6-luna"}
	primary.Extra = map[string]any{siwcAgentParentExtraKey: int64(0), openAISIWCManagementAccountExtraKey: int64(1)}
	child := siwcAgent(4, "a@x.com", SIWCAgentRoleStandby, false)
	child.Extra[siwcAgentParentExtraKey] = int64(2)
	other := siwcAgent(9, "b@x.com", "", true)
	repo := newSIWCAgentRepoStub(primary, child, other)

	require.NoError(t, propagateSIWCAgentSettings(context.Background(), repo, repo.accountsByID[2]))
	require.Equal(t, []int64{4}, repo.bulkIDs)
	require.Equal(t, int64(7), *repo.bulkUpdate.ProxyID)
	require.Equal(t, 3, *repo.bulkUpdate.Priority)
	require.Equal(t, 8, *repo.bulkUpdate.Concurrency)
	require.Equal(t, map[string]any{"gpt-6-luna": "gpt-6-luna"}, repo.bulkUpdate.Credentials["model_mapping"])
	require.Equal(t, map[string]any{}, repo.bulkUpdate.Credentials["compact_model_mapping"])
	require.Equal(t, int64(1), repo.bulkUpdate.Extra[openAISIWCManagementAccountExtraKey])
	require.NotContains(t, repo.bulkUpdate.Credentials, "refresh_token", "不能触碰子 Agent 的令牌")
	require.Equal(t, []int64{11, 12}, repo.bound[4])

	// 子 Agent 自身更新不反向同步
	repo.bulkIDs = nil
	require.NoError(t, propagateSIWCAgentSettings(context.Background(), repo, repo.accountsByID[4]))
	require.Nil(t, repo.bulkIDs)
}

func TestAdminDeleteAccount_CascadesSIWCAgents(t *testing.T) {
	primary := siwcAgent(2, "a@x.com", "", true)
	primary.Extra = map[string]any{siwcAgentParentExtraKey: int64(0)}
	child := siwcAgent(4, "a@x.com", SIWCAgentRoleStandby, false)
	child.Extra[siwcAgentParentExtraKey] = int64(2)
	repo := newSIWCAgentRepoStub(primary, child, siwcAgent(9, "b@x.com", "", true))
	svc := &adminServiceImpl{accountRepo: repo}

	require.NoError(t, svc.DeleteAccount(context.Background(), 2))
	require.True(t, repo.deleted[2])
	require.True(t, repo.deleted[4])
	require.False(t, repo.deleted[9])
}

func TestAccountListHideSIWCAgentChildrenContext(t *testing.T) {
	require.False(t, AccountListHidesSIWCAgentChildren(context.Background()))
	require.True(t, AccountListHidesSIWCAgentChildren(WithAccountListHideSIWCAgentChildren(context.Background())))
}

func TestSIWCAgentPool_PauseAndResumeWholeAccount(t *testing.T) {
	primary := siwcAgent(2, "a@x.com", SIWCAgentRoleStandby, false)
	primary.Extra[siwcAgentParentExtraKey] = int64(0)
	active := siwcAgent(6, "a@x.com", SIWCAgentRoleActive, true)
	active.Extra[siwcAgentParentExtraKey] = int64(2)
	repo := newSIWCAgentRepoStub(primary, active)
	pool := NewSIWCAgentPoolService(repo, nil, &fakeLeaderLockCache{}, &runtimeBlockerStub{})
	ctx := context.Background()

	// 暂停：全部不可调度，角色保留，主行打标
	group, err := pool.SetAccountPaused(ctx, 2, true, "admin")
	require.NoError(t, err)
	require.True(t, group.Paused)
	require.Empty(t, schedulableIDs(repo.siwcGateRepoStub))
	require.Equal(t, SIWCAgentRoleActive, repo.accountsByID[6].GetOpenAISIWCAgentRole())

	// 暂停期间活跃 Agent 失效：故障转移只调整角色，不打开调度
	require.NoError(t, repo.SetError(ctx, 6, "invalid_grant"))
	pool.Failover(ctx, repo.accountsByID[6])
	require.Empty(t, schedulableIDs(repo.siwcGateRepoStub), "暂停中不能被系统动作重新打开")
	require.Equal(t, SIWCAgentRoleActive, repo.accountsByID[2].GetOpenAISIWCAgentRole())

	// 恢复：活跃 Agent #2 可用 → 只有它可调度，暂停标记清除
	group, err = pool.SetAccountPaused(ctx, 2, false, "admin")
	require.NoError(t, err)
	require.False(t, group.Paused)
	require.ElementsMatch(t, []int64{2}, schedulableIDs(repo.siwcGateRepoStub))

	// 全部不可用时恢复失败
	require.NoError(t, repo.SetError(ctx, 2, "invalid_grant"))
	_, err = pool.SetAccountPaused(ctx, 2, false, "admin")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no usable agent")
}

func TestSIWCAgentPool_ManualActivateResumesPausedAccount(t *testing.T) {
	primary := siwcAgent(2, "a@x.com", SIWCAgentRoleActive, false)
	primary.Extra[siwcAgentParentExtraKey] = int64(0)
	primary.Extra[siwcAccountPausedExtraKey] = true
	child := siwcAgent(6, "a@x.com", SIWCAgentRoleStandby, false)
	child.Extra[siwcAgentParentExtraKey] = int64(2)
	repo := newSIWCAgentRepoStub(primary, child)
	pool := NewSIWCAgentPoolService(repo, nil, &fakeLeaderLockCache{}, &runtimeBlockerStub{})

	group, err := pool.Activate(context.Background(), 6, "admin")
	require.NoError(t, err)
	require.False(t, group.Paused)
	require.ElementsMatch(t, []int64{6}, schedulableIDs(repo.siwcGateRepoStub))
}

func TestSIWCAgentPool_SummaryReportsPausedAndActive(t *testing.T) {
	primary := siwcAgent(2, "a@x.com", SIWCAgentRoleStandby, false)
	primary.Extra[siwcAgentParentExtraKey] = int64(0)
	active := siwcAgent(6, "a@x.com", SIWCAgentRoleActive, true)
	active.Extra[siwcAgentParentExtraKey] = int64(2)
	repo := newSIWCAgentRepoStub(primary, active)
	pool := NewSIWCAgentPoolService(repo, nil, &fakeLeaderLockCache{}, &runtimeBlockerStub{})

	summaries := pool.SummariesFor(context.Background(), []Account{*repo.accountsByID[2]})
	require.Equal(t, 2, summaries[2].Count)
	require.Equal(t, int64(6), summaries[2].ActiveAccountID)
	require.False(t, summaries[2].Paused)
	require.Equal(t, int64(6), summaries[2].Active.ID)
}
