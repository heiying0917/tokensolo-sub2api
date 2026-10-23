package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newTestOpenAIOAuthSessionStore(t *testing.T) (service.OpenAIOAuthSessionStore, *miniredis.Miniredis, *redis.Client) {
	t.Helper()
	srv := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewOpenAIOAuthSessionStore(rdb), srv, rdb
}

func TestOpenAIOAuthSessionStore_SaveGetDelete(t *testing.T) {
	ctx := context.Background()
	store, srv, rdb := newTestOpenAIOAuthSessionStore(t)

	session := &openai.OAuthSession{
		State:        "st",
		CodeVerifier: "verifier",
		RedirectURI:  openai.SIWCDefaultRedirectURI,
		CreatedAt:    time.Now(),
		AuthMode:     openai.SIWCAuthModeSession,
		Nonce:        "nonce",
		DeviceID:     "dev",
		AgentName:    "agent",
	}
	require.NoError(t, store.Save(ctx, "sid", session, 10*time.Minute))

	// 另一个"副本"用独立客户端读同一份 Redis
	other := NewOpenAIOAuthSessionStore(redis.NewClient(&redis.Options{Addr: srv.Addr()}))
	got, err := other.Get(ctx, "sid")
	require.NoError(t, err)
	require.Equal(t, "st", got.State)
	require.Equal(t, "verifier", got.CodeVerifier)
	require.Equal(t, openai.SIWCAuthModeSession, got.AuthMode)
	require.Equal(t, "nonce", got.Nonce)
	require.Equal(t, "dev", got.DeviceID)
	require.Equal(t, "agent", got.AgentName)

	ttl := rdb.TTL(ctx, openAIOAuthSessionKeyPrefix+"sid").Val()
	require.Greater(t, ttl, 9*time.Minute)

	require.NoError(t, store.Delete(ctx, "sid"))
	_, err = store.Get(ctx, "sid")
	require.ErrorIs(t, err, service.ErrOpenAIOAuthSessionNotFound)
}

func TestOpenAIOAuthSessionStore_ExpiryAndInvalid(t *testing.T) {
	ctx := context.Background()
	store, srv, _ := newTestOpenAIOAuthSessionStore(t)

	require.NoError(t, store.Save(ctx, "sid", &openai.OAuthSession{State: "s", CreatedAt: time.Now()}, time.Second))
	srv.FastForward(2 * time.Second)
	_, err := store.Get(ctx, "sid")
	require.ErrorIs(t, err, service.ErrOpenAIOAuthSessionNotFound)

	// CreatedAt 超过 SessionTTL 也视为过期
	require.NoError(t, store.Save(ctx, "old", &openai.OAuthSession{State: "s", CreatedAt: time.Now().Add(-openai.SessionTTL - time.Minute)}, time.Hour))
	_, err = store.Get(ctx, "old")
	require.ErrorIs(t, err, service.ErrOpenAIOAuthSessionNotFound)

	_, err = store.Get(ctx, "")
	require.ErrorIs(t, err, service.ErrOpenAIOAuthSessionNotFound)
	require.Error(t, store.Save(ctx, "x", nil, time.Minute))
	require.Error(t, store.Save(ctx, "", &openai.OAuthSession{}, time.Minute))
	require.NoError(t, store.Delete(ctx, ""))
}
