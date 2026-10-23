package service

import (
	"context"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

// ErrOpenAIOAuthSessionNotFound 表示授权会话不存在或已过期。
var ErrOpenAIOAuthSessionNotFound = errors.New("openai oauth session not found")

// OpenAIOAuthSessionStore 保存 OpenAI OAuth 授权会话（state / PKCE verifier / nonce 等）。
//
// 多副本部署下"生成授权链接"和"粘贴回调换码"可能落到不同 pod，
// 所以生产实现必须是 Redis（repository 层），进程内 map 仅供单测与无 Redis 的回退。
type OpenAIOAuthSessionStore interface {
	Save(ctx context.Context, sessionID string, session *openai.OAuthSession, ttl time.Duration) error
	// Get 返回会话；不存在或过期返回 ErrOpenAIOAuthSessionNotFound。
	Get(ctx context.Context, sessionID string) (*openai.OAuthSession, error)
	Delete(ctx context.Context, sessionID string) error
}

// memoryOpenAIOAuthSessionStore 把进程内 openai.SessionStore 适配成接口，
// 作为未注入 Redis 实现时的回退（现有单测用 NewOpenAIOAuthService(nil, stub) 构造）。
type memoryOpenAIOAuthSessionStore struct {
	inner *openai.SessionStore
}

func newMemoryOpenAIOAuthSessionStore() *memoryOpenAIOAuthSessionStore {
	return &memoryOpenAIOAuthSessionStore{inner: openai.NewSessionStore()}
}

func (s *memoryOpenAIOAuthSessionStore) Save(_ context.Context, sessionID string, session *openai.OAuthSession, _ time.Duration) error {
	if session == nil {
		return errors.New("oauth session is nil")
	}
	s.inner.Set(sessionID, session)
	return nil
}

func (s *memoryOpenAIOAuthSessionStore) Get(_ context.Context, sessionID string) (*openai.OAuthSession, error) {
	session, ok := s.inner.Get(sessionID)
	if !ok {
		return nil, ErrOpenAIOAuthSessionNotFound
	}
	return session, nil
}

func (s *memoryOpenAIOAuthSessionStore) Delete(_ context.Context, sessionID string) error {
	s.inner.Delete(sessionID)
	return nil
}

// Stop 停止进程内清理协程。
func (s *memoryOpenAIOAuthSessionStore) Stop() {
	s.inner.Stop()
}
