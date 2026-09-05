package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/dgraph-io/ristretto"
	"github.com/gin-gonic/gin"
)

// CodexDailySessionRepository owns immutable bindings and atomic first allocation.
type CodexDailySessionRepository interface {
	FindBinding(context.Context, string, string) (string, error)
	Allocate(context.Context, string, string, string, int64, int, int) (string, error)
	CleanupDays(context.Context, string, int) (int64, error)
}

type CodexDailySessionPool struct {
	repo        CodexDailySessionRepository
	cache       *ristretto.Cache
	location    *time.Location
	now         func() time.Time
	nextCleanup atomic.Int64
}

const codexDailySessionResolverContextKey = "codex_daily_session_resolver"

type codexDailySessionResolver struct {
	gateway *OpenAIGatewayService
	ctx     context.Context
}

func resolveCodexDailySessionProjection(c *gin.Context, account *Account, p *codexRequestIdentity) error {
	if c == nil || p == nil {
		return nil
	}
	if account.GetCodexFingerprintMode() != codexFingerprintSession {
		return nil
	}
	value, _ := c.Get(codexDailySessionResolverContextKey)
	resolver, _ := value.(codexDailySessionResolver)
	gateway := resolver.gateway
	source := codexAccountIdentitySource(c, account)
	enabled, _ := source.Extra[CodexDailySessionEnabledKey].(bool)
	// A shadow still needs its explicit session convergence mode; its pool
	// interval and enable switch belong to the resolved credential parent.
	enabled = enabled && account.GetCodexFingerprintMode() == codexFingerprintSession
	if p.originalSession == "" {
		if enabled {
			return errors.New("Codex daily session pooling requires an original root session_id")
		}
		return nil
	}
	if gateway == nil || gateway.codexDailySessionPool == nil {
		if enabled {
			return errors.New("Codex daily session storage is unavailable")
		}
		return nil
	}
	namespace := codexAccountIdentityNamespace(source)
	if namespace == "" {
		if enabled {
			return errors.New("Codex daily session pooling requires a stable credential identity")
		}
		return nil
	}
	if resolver.ctx == nil {
		return errors.New("Codex daily session resolution requires request context")
	}
	session, err := gateway.codexDailySessionPool.resolve(resolver.ctx, namespace, getAPIKeyIDFromContext(c), p.originalSession, source)
	if err != nil {
		return fmt.Errorf("resolve Codex daily session: %w", err)
	}
	if session != "" {
		p.values["session"] = session
	}
	return nil
}

func projectCodexRequestBodyWithDailySession(c *gin.Context, account *Account, body map[string]any) (bool, error) {
	p := stageCodexRequestIdentity(c, account, body)
	if err := resolveCodexDailySessionProjection(c, account, p); err != nil {
		return false, err
	}
	return p.applyBody(body), nil
}

func NewCodexDailySessionPool(repo CodexDailySessionRepository, cfg *config.Config) *CodexDailySessionPool {
	zone := "Asia/Shanghai"
	if cfg != nil && cfg.Timezone != "" {
		zone = cfg.Timezone
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		location, _ = time.LoadLocation("Asia/Shanghai")
	}
	cache, _ := ristretto.NewCache(&ristretto.Config{NumCounters: 100000, MaxCost: 10000, BufferItems: 64})
	return &CodexDailySessionPool{repo: repo, cache: cache, location: location, now: time.Now}
}

func (s *CodexDailySessionPool) Close() {
	if s != nil && s.cache != nil {
		s.cache.Close()
	}
}

func codexPoolDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum[:])
}

func (s *CodexDailySessionPool) resolve(ctx context.Context, namespace string, apiKeyID int64, root string, account *Account) (string, error) {
	scope := codexPoolDigest("codex-daily-account:v1:" + namespace)
	binding := codexPoolDigest(fmt.Sprintf("codex-daily-root:v1:%d:%s", apiKeyID, root))
	key := scope + ":" + binding
	if s.cache != nil {
		if value, ok := s.cache.Get(key); ok {
			return value.(string), nil
		}
	}
	session, err := s.repo.FindBinding(ctx, scope, binding)
	if err != nil {
		return "", err
	}
	if session == "" {
		enabled, min, max, policyErr := CodexDailySessionPolicy(account)
		if policyErr != nil {
			return "", policyErr
		}
		if !enabled {
			return "", nil
		}
		day := s.now().In(s.location).Format("2006-01-02")
		session, err = s.repo.Allocate(ctx, scope, binding, day, account.ID, min, max)
		if err != nil {
			return "", err
		}
		s.cleanupOldDays()
	}
	if session != "" && s.cache != nil {
		s.cache.SetWithTTL(key, session, 1, 15*time.Minute)
	}
	return session, nil
}

func (s *CodexDailySessionPool) cleanupOldDays() {
	now := s.now()
	next := s.nextCleanup.Load()
	if now.Unix() < next || !s.nextCleanup.CompareAndSwap(next, now.Add(time.Hour).Unix()) {
		return
	}
	before := now.In(s.location).AddDate(0, 0, -90).Format("2006-01-02")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := s.repo.CleanupDays(ctx, before, 1000); err != nil {
			slog.Warn("codex_daily_session_day_cleanup_failed", "error", err)
		}
	}()
}
