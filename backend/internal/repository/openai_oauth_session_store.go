package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const openAIOAuthSessionKeyPrefix = "oauth:session:openai:"

// openaiOAuthSessionStore 是 service.OpenAIOAuthSessionStore 的 Redis 实现，
// 让授权会话在多副本之间共享（照 passkey_session_store 的写法）。
type openaiOAuthSessionStore struct {
	rdb *redis.Client
}

// NewOpenAIOAuthSessionStore 构造 Redis 授权会话存储。
func NewOpenAIOAuthSessionStore(rdb *redis.Client) service.OpenAIOAuthSessionStore {
	return &openaiOAuthSessionStore{rdb: rdb}
}

func openAIOAuthSessionKey(sessionID string) string {
	return openAIOAuthSessionKeyPrefix + strings.TrimSpace(sessionID)
}

func (s *openaiOAuthSessionStore) Save(ctx context.Context, sessionID string, session *openai.OAuthSession, ttl time.Duration) error {
	if session == nil {
		return errors.New("oauth session is nil")
	}
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("oauth session id is empty")
	}
	if ttl <= 0 {
		ttl = openai.SessionTTL
	}
	payload, err := json.Marshal(session)
	if err != nil {
		return fmt.Errorf("encode openai oauth session: %w", err)
	}
	if err := s.rdb.Set(ctx, openAIOAuthSessionKey(sessionID), payload, ttl).Err(); err != nil {
		return fmt.Errorf("store openai oauth session: %w", err)
	}
	return nil
}

func (s *openaiOAuthSessionStore) Get(ctx context.Context, sessionID string) (*openai.OAuthSession, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || len(sessionID) > 128 {
		return nil, service.ErrOpenAIOAuthSessionNotFound
	}
	payload, err := s.rdb.Get(ctx, openAIOAuthSessionKey(sessionID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, service.ErrOpenAIOAuthSessionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load openai oauth session: %w", err)
	}
	var session openai.OAuthSession
	if err := json.Unmarshal(payload, &session); err != nil {
		return nil, service.ErrOpenAIOAuthSessionNotFound
	}
	// Redis TTL 已兜底，这里再按 CreatedAt 校验一次，与进程内实现语义一致。
	if !session.CreatedAt.IsZero() && time.Since(session.CreatedAt) > openai.SessionTTL {
		_ = s.rdb.Del(ctx, openAIOAuthSessionKey(sessionID)).Err()
		return nil, service.ErrOpenAIOAuthSessionNotFound
	}
	return &session, nil
}

func (s *openaiOAuthSessionStore) Delete(ctx context.Context, sessionID string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}
	return s.rdb.Del(ctx, openAIOAuthSessionKey(sessionID)).Err()
}
