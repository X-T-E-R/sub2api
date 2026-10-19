package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/stretchr/testify/require"
)

type antigravityUASettingsRepo struct {
	SettingRepository
	mu          sync.Mutex
	values      map[string]string
	writeErr    error
	getAllErr   error
	writes      int
	afterUARead func()
	afterWrite  func()
}

func (r *antigravityUASettingsRepo) GetValue(_ context.Context, key string) (string, error) {
	r.mu.Lock()
	value := r.values[key]
	r.mu.Unlock()
	if key == SettingKeyAntigravityUserAgent && r.afterUARead != nil {
		r.afterUARead()
	}
	return value, nil
}

func (r *antigravityUASettingsRepo) GetAll(context.Context) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.values))
	for key, value := range r.values {
		out[key] = value
	}
	return out, r.getAllErr
}

func (r *antigravityUASettingsRepo) SetMultiple(_ context.Context, values map[string]string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.writeErr != nil {
		return r.writeErr
	}
	r.writes++
	for key, value := range values {
		r.values[key] = value
	}
	if r.afterWrite != nil {
		r.afterWrite()
	}
	return nil
}

func bindAntigravityUASettings(t *testing.T, svc *SettingService) {
	t.Helper()
	antigravity.SetUserAgentResolver(svc.GetAntigravityUserAgentOverride)
	antigravity.SetUserAgentVersionResolver(svc.GetAntigravityUserAgentVersion)
	t.Cleanup(func() {
		antigravity.SetUserAgentResolver(nil)
		antigravity.SetUserAgentVersionResolver(nil)
	})
}

func TestAntigravityUserAgentSettingsSaveHotReloadAndFallback(t *testing.T) {
	ctx := context.Background()
	repo := &antigravityUASettingsRepo{values: map[string]string{
		SettingKeyAntigravityUserAgent:        "stored-client/1",
		SettingKeyAntigravityUserAgentVersion: "1.23.2",
	}}
	svc := NewSettingService(repo, nil)
	bindAntigravityUASettings(t, svc)
	requestUA := func() string {
		req, err := antigravity.NewAPIRequestWithURL(ctx, "https://example.invalid", "streamGenerateContent", "fixture-token", nil)
		require.NoError(t, err)
		return req.Header.Get("User-Agent")
	}
	require.Equal(t, "stored-client/1", requestUA(), "cold runtime loads the saved full header")
	require.NoError(t, svc.UpdateSettings(ctx, &SystemSettings{AntigravityUserAgent: "  independent-client/2 (any profile)  ", AntigravityUserAgentVersion: "2.9.1"}))
	require.Equal(t, "independent-client/2 (any profile)", requestUA(), "successful save replaces a warm runtime immediately")
	stored, err := svc.GetAllSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, "independent-client/2 (any profile)", stored.AntigravityUserAgent)
	restarted := NewSettingService(repo, nil)
	require.Equal(t, stored.AntigravityUserAgent, restarted.GetAntigravityUserAgentOverride(ctx))

	// A partial service update must not clear the full override.
	require.NoError(t, svc.UpdateSettingsOmitting(ctx, &SystemSettings{AntigravityUserAgentVersion: "2.8.0"}, OmittedSettingKeys{SettingKeyAntigravityUserAgent: {}}))
	require.Equal(t, stored.AntigravityUserAgent, requestUA())
	require.Equal(t, stored.AntigravityUserAgent, repo.values[SettingKeyAntigravityUserAgent])

	writes := repo.writes
	require.Error(t, svc.UpdateSettings(ctx, &SystemSettings{AntigravityUserAgent: "bad\r\nheader"}))
	require.Equal(t, writes, repo.writes)
	require.Equal(t, stored.AntigravityUserAgent, requestUA())
	repo.writeErr = errors.New("database unavailable")
	require.Error(t, svc.UpdateSettings(ctx, &SystemSettings{AntigravityUserAgent: "not-persisted/3"}))
	require.Equal(t, stored.AntigravityUserAgent, requestUA())
	require.Equal(t, stored.AntigravityUserAgent, repo.values[SettingKeyAntigravityUserAgent])
	repo.writeErr = nil

	// A successful explicit write must publish even when the broad refresh fails.
	repo.getAllErr = errors.New("post-write read unavailable")
	require.NoError(t, svc.UpdateSettingsOmitting(ctx, &SystemSettings{AntigravityUserAgent: "fresh-client/4"}, OmittedSettingKeys{SettingKeySiteName: {}}))
	require.Equal(t, "fresh-client/4", requestUA())
	repo.getAllErr = nil
	require.NoError(t, svc.UpdateSettings(ctx, &SystemSettings{AntigravityUserAgent: "", AntigravityUserAgentVersion: "1.23.2"}))
	require.Equal(t, "antigravity/hub/1.23.2 darwin/arm64", requestUA())
	require.NoError(t, svc.UpdateSettings(ctx, &SystemSettings{}))
	require.Equal(t, antigravity.BuildUserAgent(antigravity.GetDefaultUserAgentVersion()), requestUA())
}

func TestAntigravityUserAgentSettingsCatalogUsesFullHeader(t *testing.T) {
	const ua = "independent-client/9 (not a Google-only profile)"
	repo := &antigravityUASettingsRepo{values: map[string]string{}}
	svc := NewSettingService(repo, nil)
	bindAntigravityUASettings(t, svc)
	require.Empty(t, svc.GetAntigravityUserAgentOverride(context.Background()))
	require.NoError(t, svc.UpdateSettings(context.Background(), &SystemSettings{AntigravityUserAgent: ua}))
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		require.Equal(t, "/v1internal:fetchAvailableModels", r.URL.Path)
		require.Equal(t, ua, r.Header.Get("User-Agent"))
		_, _ = w.Write([]byte(`{"models":{"gemini-4-argon-low":{},"gemini-4-argon-medium":{},"gemini-4-argon-high":{}}}`))
	}))
	defer server.Close()
	oldURL, oldURLs := antigravity.BaseURL, antigravity.BaseURLs
	antigravity.BaseURL, antigravity.BaseURLs = server.URL, []string{server.URL}
	t.Cleanup(func() { antigravity.BaseURL, antigravity.BaseURLs = oldURL, oldURLs })
	accountTest := &AccountTestService{cfg: upstreamModelSyncTestConfig(), accountRepo: &upstreamModelMetadataRepoStub{}, antigravityGatewayService: newAntigravityCompatService(config.GatewayConfig{}, nil)}
	catalog, err := accountTest.SyncUpstreamModelCatalog(context.Background(), newAntigravityCompatAccount(AccountTypeOAuth))
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	require.Equal(t, "gemini-4-argon-{effort}", catalog.ModelMapping["gemini-4-argon"])
}

func TestAntigravityUserAgentSettingsSaveWinsOverInflightRead(t *testing.T) {
	started, release, written := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	repo := &antigravityUASettingsRepo{values: map[string]string{SettingKeyAntigravityUserAgent: "old-client/1"}}
	repo.afterUARead = func() { once.Do(func() { close(started); <-release }) }
	repo.afterWrite = func() { close(written) }
	svc := NewSettingService(repo, nil)
	readDone := make(chan string, 1)
	go func() { readDone <- svc.GetAntigravityUserAgentOverride(context.Background()) }()
	<-started
	saveDone := make(chan error, 1)
	go func() {
		saveDone <- svc.UpdateSettings(context.Background(), &SystemSettings{AntigravityUserAgent: "new-client/2"})
	}()
	<-written
	close(release)
	require.Equal(t, "old-client/1", <-readDone)
	require.NoError(t, <-saveDone)
	require.Equal(t, "new-client/2", svc.GetAntigravityUserAgentOverride(context.Background()))
}
