//go:build unit

package admin

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestMergeSIWCAgentTodayStats(t *testing.T) {
	stats := map[int64]*service.WindowStats{
		2: {Requests: 3, Tokens: 100, Cost: 0.5},
		4: {Requests: 2, Tokens: 50, Cost: 0.25},
		7: {Requests: 1, Tokens: 10, Cost: 0.1},
	}
	merged := mergeSIWCAgentTodayStats(stats, []int64{2, 7}, map[int64][]int64{2: {4}})
	require.Equal(t, int64(5), merged[2].Requests)
	require.Equal(t, int64(150), merged[2].Tokens)
	require.InDelta(t, 0.75, merged[2].Cost, 1e-9)
	require.Equal(t, int64(1), merged[7].Requests)
	require.NotContains(t, merged, int64(4), "子 Agent 不单独返回")

	// 主行自己今天没用量、子 Agent 有：仍返回主行汇总
	merged = mergeSIWCAgentTodayStats(map[int64]*service.WindowStats{4: {Requests: 2}}, []int64{2}, map[int64][]int64{2: {4}})
	require.Equal(t, int64(2), merged[2].Requests)

	// 无子 Agent：原样返回
	require.Equal(t, stats, mergeSIWCAgentTodayStats(stats, []int64{2}, nil))
}

func TestApplySIWCAccountView(t *testing.T) {
	primary := &service.Account{ID: 2, Status: service.StatusActive, Schedulable: false}
	active := &service.Account{ID: 6, Status: service.StatusActive, Schedulable: true, ErrorMessage: ""}

	// 主行待命、#6 活跃：列表显示账号可调度
	applySIWCAccountView(primary, &service.SIWCAgentSummary{Count: 2, ActiveAccountID: 6, Active: active})
	require.True(t, primary.Schedulable)

	// 账号暂停：不可调度
	primary.Schedulable = true
	applySIWCAccountView(primary, &service.SIWCAgentSummary{Count: 2, ActiveAccountID: 6, Active: active, Paused: true})
	require.False(t, primary.Schedulable)

	// 没有可用活跃 Agent：不可调度，状态保留主行自身
	primary = &service.Account{ID: 2, Status: service.StatusError, Schedulable: true}
	applySIWCAccountView(primary, &service.SIWCAgentSummary{Count: 1})
	require.False(t, primary.Schedulable)
	require.Equal(t, service.StatusError, primary.Status)

	// 主行出错但 #6 正常活跃：账号状态按活跃 Agent 展示
	primary = &service.Account{ID: 2, Status: service.StatusError, ErrorMessage: "invalid_grant"}
	applySIWCAccountView(primary, &service.SIWCAgentSummary{Count: 2, ActiveAccountID: 6, Active: active})
	require.Equal(t, service.StatusActive, primary.Status)
	require.Empty(t, primary.ErrorMessage)
}
