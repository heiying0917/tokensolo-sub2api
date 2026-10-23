package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/redis/go-redis/v9"
)

const (
	oauthTokenKeyPrefix       = "oauth:token:"
	oauthRefreshLockKeyPrefix = "oauth:refresh_lock:"
)

type geminiTokenCache struct {
	rdb *redis.Client
}

func NewGeminiTokenCache(rdb *redis.Client) service.GeminiTokenCache {
	return &geminiTokenCache{rdb: rdb}
}

func (c *geminiTokenCache) GetAccessToken(ctx context.Context, cacheKey string) (string, error) {
	key := fmt.Sprintf("%s%s", oauthTokenKeyPrefix, cacheKey)
	return c.rdb.Get(ctx, key).Result()
}

func (c *geminiTokenCache) SetAccessToken(ctx context.Context, cacheKey string, token string, ttl time.Duration) error {
	key := fmt.Sprintf("%s%s", oauthTokenKeyPrefix, cacheKey)
	return c.rdb.Set(ctx, key, token, ttl).Err()
}

func (c *geminiTokenCache) DeleteAccessToken(ctx context.Context, cacheKey string) error {
	key := fmt.Sprintf("%s%s", oauthTokenKeyPrefix, cacheKey)
	return c.rdb.Del(ctx, key).Err()
}

func (c *geminiTokenCache) AcquireRefreshLock(ctx context.Context, cacheKey string, ttl time.Duration) (bool, error) {
	key := fmt.Sprintf("%s%s", oauthRefreshLockKeyPrefix, cacheKey)
	return c.rdb.SetNX(ctx, key, 1, ttl).Result()
}

func (c *geminiTokenCache) ReleaseRefreshLock(ctx context.Context, cacheKey string) error {
	key := fmt.Sprintf("%s%s", oauthRefreshLockKeyPrefix, cacheKey)
	return c.rdb.Del(ctx, key).Err()
}

// AcquireRefreshLockOwned 以 owner 为值 SETNX，供 compare-and-delete 释放。
func (c *geminiTokenCache) AcquireRefreshLockOwned(ctx context.Context, cacheKey, owner string, ttl time.Duration) (bool, error) {
	key := fmt.Sprintf("%s%s", oauthRefreshLockKeyPrefix, cacheKey)
	return c.rdb.SetNX(ctx, key, owner, ttl).Result()
}

// ReleaseRefreshLockOwned 只在锁仍属于 owner 时删除（复用 leader lock 的 Lua 脚本），
// 避免刷新耗时超过 TTL 后误删别的实例新拿到的锁。
func (c *geminiTokenCache) ReleaseRefreshLockOwned(ctx context.Context, cacheKey, owner string) error {
	key := fmt.Sprintf("%s%s", oauthRefreshLockKeyPrefix, cacheKey)
	return leaderLockReleaseScript.Run(ctx, c.rdb, []string{key}, owner).Err()
}
