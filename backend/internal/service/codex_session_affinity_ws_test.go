package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type sessionAffinityWSConn struct{ *stagedPassthroughConn }

type sessionAffinityWSDialer struct {
	conn   openAIWSClientConn
	dials  atomic.Int64
	status int
	err    error
}

func (d *sessionAffinityWSDialer) Dial(context.Context, string, http.Header, string) (openAIWSClientConn, int, http.Header, error) {
	d.dials.Add(1)
	if d.err != nil {
		return nil, d.status, http.Header{}, d.err
	}
	return d.conn, http.StatusSwitchingProtocols, http.Header{}, nil
}

func TestCodexSessionAffinityWebSocketHandshakeRejectAllowsMove(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough} {
		for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
			t.Run(mode+"/"+http.StatusText(status), func(t *testing.T) {
				a, b := sessionAffinityTestAccount(11, "A"), sessionAffinityTestAccount(12, "B")
				a.Extra["openai_oauth_responses_websockets_v2_mode"] = mode
				repo := &sessionOwnerFixture{owners: map[string]CodexSessionAccountOwner{
					codexSessionBindingKey(7, "root"): {AccountScope: CodexDailySessionScope(&a), AccountID: a.ID, Revision: 1},
				}}
				svc := sessionAffinityTestService(t, repo, a, b)
				ctx, _ := sessionAffinityTestContext(t, 7, "root")
				selected, _, err := sessionAffinitySelect(svc, ctx, 1, "", nil)
				require.NoError(t, err)
				defer releaseCodexSessionSelection(selected)
				svc.cfg = newOpenAIWSV2TestConfig()
				svc.settingService = nil
				svc.cfg.Gateway.OpenAIWS.OAuthEnabled = true
				svc.cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
				svc.httpUpstream = &httpUpstreamRecorder{}
				svc.toolCorrector = NewCodexToolCorrector()
				dialer := &sessionAffinityWSDialer{status: status, err: errors.New("fixture handshake rejection")}
				pool := newOpenAIWSConnPool(svc.cfg)
				pool.setClientDialerForTest(dialer)
				defer pool.Close()
				svc.openaiWSPool, svc.openaiWSPassthroughDialer = pool, dialer
				svc.openaiWSResolver = NewOpenAIWSProtocolResolver(svc.cfg)
				result := make(chan error, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, acceptErr := coderws.Accept(w, r, nil)
					if acceptErr != nil {
						result <- acceptErr
						return
					}
					defer func() { _ = conn.CloseNow() }()
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = r.Clone(ctx)
					c.Request.Header.Set("session_id", "root")
					c.Set("api_key", &APIKey{ID: 7})
					_, first, readErr := conn.Read(ctx)
					if readErr != nil {
						result <- readErr
						return
					}
					result <- svc.ProxyResponsesWebSocketFromClient(ctx, c, conn, selected.Account, "synthetic-token", first, nil)
				}))
				defer server.Close()
				clientCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				client, _, err := coderws.Dial(clientCtx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
				require.NoError(t, err)
				defer func() { _ = client.CloseNow() }()
				require.NoError(t, client.Write(clientCtx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.6-luna","input":"hello"}`)))
				select {
				case err = <-result:
				case <-clientCtx.Done():
					t.Fatal("handshake result missing")
				}
				var failover *UpstreamFailoverError
				require.ErrorAs(t, err, &failover)
				require.Equal(t, status, failover.StatusCode)
				require.True(t, CodexSessionMayFailover(ctx, failover))
				require.Equal(t, 1, dialer.DialCount())
				next, _, err := sessionAffinitySelect(svc, ctx, 1, "", map[int64]struct{}{a.ID: {}})
				require.NoError(t, err)
				require.Equal(t, b.ID, next.Account.ID)
				releaseCodexSessionSelection(next)
			})
		}
	}
}

func (d *sessionAffinityWSDialer) DialCount() int { return int(d.dials.Load()) }

func (c *sessionAffinityWSConn) WriteJSON(ctx context.Context, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return c.WriteFrame(ctx, coderws.MessageText, payload)
}

func TestCodexSessionAffinityWebSocketFrames(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough} {
		for _, scenario := range []string{"same root", "changed root", "upstream EOF", "missing continuation"} {
			t.Run(mode+"/"+scenario, func(t *testing.T) {
				a := sessionAffinityTestAccount(11, "A")
				a.Extra["openai_oauth_responses_websockets_v2_mode"] = mode
				require.Equal(t, mode, a.ResolveOpenAIResponsesWebSocketV2Mode(OpenAIWSIngressModeCtxPool))
				repo := &sessionOwnerFixture{owners: map[string]CodexSessionAccountOwner{
					codexSessionBindingKey(7, "root"): {AccountScope: CodexDailySessionScope(&a), AccountID: a.ID},
				}}
				svc := sessionAffinityTestService(t, repo, a)
				ctx, _ := sessionAffinityTestContext(t, 7, "root")
				selected, _, err := sessionAffinitySelect(svc, ctx, 1, "", nil)
				require.NoError(t, err)
				defer releaseCodexSessionSelection(selected)
				svc.settingService = nil
				svc.httpUpstream = &httpUpstreamRecorder{}
				svc.toolCorrector = NewCodexToolCorrector()
				cfg := svc.cfg
				cfg.Gateway.OpenAIWS.Enabled = true
				cfg.Gateway.OpenAIWS.OAuthEnabled = true
				cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
				cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
				cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
				cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
				cfg.Gateway.OpenAIWS.QueueLimitPerConn = 8
				cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 2
				cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 2
				cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 2
				upstream := &sessionAffinityWSConn{newStagedPassthroughConn()}
				dialer := &sessionAffinityWSDialer{conn: upstream}
				pool := newOpenAIWSConnPool(cfg)
				pool.setClientDialerForTest(dialer)
				defer pool.Close()
				svc.openaiWSPool = pool
				svc.openaiWSPassthroughDialer = dialer
				svc.openaiWSResolver = NewOpenAIWSProtocolResolver(cfg)
				serverErr := make(chan error, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, acceptErr := coderws.Accept(w, r, nil)
					if acceptErr != nil {
						serverErr <- acceptErr
						return
					}
					defer func() { _ = conn.CloseNow() }()
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = r.Clone(ctx)
					c.Request.Header.Set("session_id", "root")
					c.Request.Header.Set("User-Agent", "codex_cli_rs/0.144.1")
					c.Set("api_key", &APIKey{ID: 7})
					_, first, readErr := conn.Read(ctx)
					if readErr != nil {
						serverErr <- readErr
						return
					}
					serverErr <- svc.ProxyResponsesWebSocketFromClient(ctx, c, conn, selected.Account, "synthetic-token", first, nil)
				}))
				defer server.Close()
				clientCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				client, _, err := coderws.Dial(clientCtx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
				require.NoError(t, err)
				defer func() { _ = client.CloseNow() }()
				send := func(root, thread string) {
					body := identityFixture(t, root, thread, "turn-"+thread, 1, nil)
					body["type"], body["model"], body["input"], body["stream"] = "response.create", "gpt-5.6-luna", []any{}, false
					if scenario == "missing continuation" {
						body["previous_response_id"] = "resp_mapping_expired"
					}
					payload, marshalErr := json.Marshal(body)
					require.NoError(t, marshalErr)
					require.NoError(t, client.Write(clientCtx, coderws.MessageText, payload))
				}
				readWrite := func() []byte {
					select {
					case data := <-upstream.writes:
						return data
					case <-clientCtx.Done():
						t.Fatal("upstream write missing")
						return nil
					}
				}
				complete := func(id string) {
					upstream.Send(`{"type":"response.completed","response":{"id":"` + id + `","model":"gpt-5.6-luna","usage":{"input_tokens":1,"output_tokens":1}}}`)
					_, payload, readErr := client.Read(clientCtx)
					require.NoError(t, readErr)
					require.Equal(t, id, gjson.GetBytes(payload, "response.id").String())
				}
				send("root", "first")
				missingPoolConnection := scenario == "missing continuation" && mode == OpenAIWSIngressModeCtxPool
				if !missingPoolConnection {
					first := readWrite()
					if scenario == "missing continuation" {
						require.Equal(t, "resp_mapping_expired", gjson.GetBytes(first, "previous_response_id").String())
					}
					if scenario == "upstream EOF" {
						upstream.Fail(io.EOF)
					} else {
						complete("resp_first")
						switch scenario {
						case "same root":
							send("root", "second")
							second := readWrite()
							fm, sm := codexMetadataObject(gjson.GetBytes(first, "client_metadata.x-codex-turn-metadata").String()), codexMetadataObject(gjson.GetBytes(second, "client_metadata.x-codex-turn-metadata").String())
							require.NotEmpty(t, fm["session_id"])
							require.Equal(t, fm["session_id"], sm["session_id"])
							require.NotEqual(t, fm["thread_id"], sm["thread_id"])
							complete("resp_second")
							require.NoError(t, client.Close(coderws.StatusNormalClosure, "done"))
						case "changed root":
							send("another-root", "second")
						case "missing continuation":
							require.NoError(t, client.Close(coderws.StatusNormalClosure, "done"))
						}
					}
				}
				if mode == OpenAIWSIngressModePassthrough && (scenario == "changed root" || scenario == "upstream EOF") {
					_, _, readErr := client.Read(clientCtx)
					require.Error(t, readErr, "client must observe the close frame and complete its handshake")
				}
				select {
				case runErr := <-serverErr:
					if scenario == "same root" || (scenario == "missing continuation" && !missingPoolConnection) {
						require.NoError(t, runErr)
					} else if mode != OpenAIWSIngressModePassthrough || scenario != "upstream EOF" {
						require.Error(t, runErr)
					}
				case <-clientCtx.Done():
					t.Fatal("websocket did not terminate")
				}
				select {
				case data := <-upstream.writes:
					t.Fatalf("unexpected replay/frame: %s", data)
				default:
				}
				if missingPoolConnection {
					require.Zero(t, dialer.DialCount())
				} else {
					require.Equal(t, 1, dialer.DialCount())
				}
			})
		}
	}
}
