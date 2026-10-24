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
