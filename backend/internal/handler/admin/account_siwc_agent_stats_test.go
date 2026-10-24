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
