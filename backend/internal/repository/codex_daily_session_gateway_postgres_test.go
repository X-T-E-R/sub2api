package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type dailyPoolPostgresUpstream struct {
	headers http.Header
	body    []byte
}

func (u *dailyPoolPostgresUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.headers = req.Header.Clone()
	u.body, _ = io.ReadAll(req.Body)
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: [DONE]\n\n"))}, nil
}
func (u *dailyPoolPostgresUpstream) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, id, concurrency)
}

func TestCodexDailyPoolPostgresActualGatewayAndRestart(t *testing.T) {
	db, repo := dailyPoolPostgres(t)
	account := &service.Account{ID: 1, Platform: "openai", Type: "oauth", Concurrency: 1,
		Credentials: map[string]any{"chatgpt_account_id": "gateway-account", "chatgpt_user_id": "gateway-user", "access_token": "fixture-token"},
		Extra:       map[string]any{"codex_fingerprint_mode": "session", "codex_fingerprint_seed": "11111111-1111-4111-8111-111111111111", service.CodexDailySessionEnabledKey: true, service.CodexDailySessionMinKey: 5, service.CodexDailySessionMaxKey: 5}}
	credentials, err := json.Marshal(account.Credentials)
	require.NoError(t, err)
	extra, err := json.Marshal(account.Extra)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO accounts(id,platform,type,credentials,extra) VALUES(1,'openai','oauth',$1,$2)`, string(credentials), string(extra))
	require.NoError(t, err)
	upstream := &dailyPoolPostgresUpstream{}
	newGateway := func() *service.OpenAIGatewayService {
		cfg := &config.Config{Timezone: "Asia/Shanghai"}
		pool := service.NewCodexDailySessionPool(repo, cfg)
		t.Cleanup(pool.Close)
		return service.ProvideOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil, pool)
	}
	gateway := newGateway()
	forward := func(root, thread string, raw bool) (string, string) {
		t.Helper()
		account.Extra["openai_oauth_passthrough"] = raw
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
		c.Request.Header.Set("User-Agent", "codex_cli_rs/0.144.1")
		c.Set("api_key", &service.APIKey{ID: 77})
		body := []byte(fmt.Sprintf(`{"model":"gpt-5.4","instructions":"fixture","input":[],"prompt_cache_key":%q,"client_metadata":{"session_id":%q,"thread_id":%q}}`, root, root, thread))
		_, err := gateway.Forward(context.Background(), c, account, body)
		require.NoError(t, err)
		session := upstream.headers.Get("session-id")
		require.Equal(t, session, gjson.GetBytes(upstream.body, "client_metadata.session_id").String())
		return session, upstream.headers.Get("thread-id")
	}
	root, rootThread := forward("root-1", "root-1", false)
	child, childThread := forward("root-1", "child-1", true)
	require.Equal(t, root, child)
	require.NotEqual(t, rootThread, childThread)
	// New gateway and cache have no prior process state. The actual repository
	// and outbound request, rather than a map fixture, must restore this binding.
	gateway = newGateway()
	restored, _ := forward("root-1", "child-1", false)
	require.Equal(t, root, restored)
	distinct := map[string]bool{root: true}
	for i := 2; i <= 12; i++ {
		session, _ := forward(fmt.Sprintf("root-%d", i), fmt.Sprintf("child-%d", i), i%2 == 0)
		distinct[session] = true
	}
	require.Len(t, distinct, 5)
	var bindings, slots int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM codex_daily_session_bindings`).Scan(&bindings))
	require.Equal(t, 12, bindings)
	require.NoError(t, db.QueryRow(`SELECT cardinality(sessions) FROM codex_daily_session_days`).Scan(&slots))
	require.Equal(t, 5, slots)
}
