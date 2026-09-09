package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type modelReasoningFloorTurnResult struct {
	result *OpenAIForwardResult
	err    error
}

func TestModelReasoningFloorWebSocketPassthrough(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const finalModel = "gpt-5.6-floor"
	controlCtx, cancelControl := context.WithCancelCause(context.Background())
	defer cancelControl(context.Canceled)

	upstream := newStagedPassthroughConn()
	for _, responseID := range []string{"resp_floor_pass_1", "resp_floor_pass_2"} {
		upstream.Send(`{"type":"response.completed","response":{"id":"` + responseID + `","model":"` + finalModel + `","usage":{"input_tokens":1,"output_tokens":1}}}`)
	}

	turnResults := make(chan modelReasoningFloorTurnResult, 2)
	svc := newPassthroughLifecycleService(passthroughLifecycleConfig(), upstream)
	svc.settingService = floorTestSettings(t, finalModel)
	hooks := &OpenAIWSIngressHooks{
		MapRequestModel: func(_ int, model string) (string, error) {
			if strings.TrimSpace(model) == "floor-alias" {
				return finalModel, nil
			}
			return model, nil
		},
		AfterTurn: func(_ int, result *OpenAIForwardResult, turnErr error) {
			turnResults <- modelReasoningFloorTurnResult{result: result, err: turnErr}
		},
	}
	server, serverErr := startPassthroughLifecycleServerWithHooks(t, controlCtx, svc, passthroughLifecycleAccount(), func(*gin.Context) *OpenAIWSIngressHooks {
		return hooks
	})
	defer server.Close()

	clientConn := dialPassthroughLifecycleClientWithPayload(t, server, `{"type":"response.create","model":"floor-alias","reasoning":{"effort":"low"},"stream":false}`)
	defer func() { _ = clientConn.CloseNow() }()

	firstUpstream := requirePassthroughUpstreamWrite(t, upstream, time.Second)
	require.Equal(t, finalModel, gjson.GetBytes(firstUpstream, "model").String())
	require.Equal(t, "xhigh", gjson.GetBytes(firstUpstream, "reasoning.effort").String())
	firstEvent, err := readPassthroughLifecycleFrame(t, clientConn, time.Second)
	require.NoError(t, err)
	require.Equal(t, "resp_floor_pass_1", gjson.GetBytes(firstEvent, "response.id").String())

	writePassthroughFloorFrame(t, clientConn, `{"type":"session.update","session":{"model":"`+finalModel+`"}}`)
	sessionUpdate := requirePassthroughUpstreamWrite(t, upstream, time.Second)
	require.Equal(t, "session.update", gjson.GetBytes(sessionUpdate, "type").String())
	require.False(t, gjson.GetBytes(sessionUpdate, "reasoning").Exists(), string(sessionUpdate))
	require.False(t, gjson.GetBytes(sessionUpdate, "reasoning.effort").Exists(), string(sessionUpdate))

	writePassthroughFloorFrame(t, clientConn, `{"type":"response.cancel","response_id":"resp_floor_pass_1"}`)
	cancelFrame := requirePassthroughUpstreamWrite(t, upstream, time.Second)
	require.Equal(t, "response.cancel", gjson.GetBytes(cancelFrame, "type").String())
	require.False(t, gjson.GetBytes(cancelFrame, "reasoning").Exists(), string(cancelFrame))
	require.False(t, gjson.GetBytes(cancelFrame, "reasoning.effort").Exists(), string(cancelFrame))

	writePassthroughFloorFrame(t, clientConn, `{"type":"response.create","reasoning":{"effort":"low"},"stream":false,"previous_response_id":"resp_floor_pass_1"}`)
	secondUpstream := requirePassthroughUpstreamWrite(t, upstream, time.Second)
	require.Equal(t, finalModel, gjson.GetBytes(secondUpstream, "model").String())
	require.Equal(t, "xhigh", gjson.GetBytes(secondUpstream, "reasoning.effort").String())
	secondEvent, err := readPassthroughLifecycleFrame(t, clientConn, time.Second)
	require.NoError(t, err)
	require.Equal(t, "resp_floor_pass_2", gjson.GetBytes(secondEvent, "response.id").String())

	require.NoError(t, clientConn.Close(coderws.StatusNormalClosure, "done"))
	select {
	case err := <-serverErr:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("passthrough floor test did not exit")
	}

	for i, want := range []struct {
		requested string
		effective string
	}{
		{requested: "low", effective: "xhigh"},
		{requested: "low", effective: "xhigh"},
	} {
		turn := <-turnResults
		require.NoError(t, turn.err, "turn %d", i+1)
		require.NotNil(t, turn.result, "turn %d", i+1)
		require.NotNil(t, turn.result.RequestedReasoningEffort, "turn %d requested", i+1)
		require.Equal(t, want.requested, *turn.result.RequestedReasoningEffort, "turn %d requested", i+1)
		require.NotNil(t, turn.result.ReasoningEffort, "turn %d effective", i+1)
		require.Equal(t, want.effective, *turn.result.ReasoningEffort, "turn %d effective", i+1)
	}
}

func TestModelReasoningFloorWebSocketPassthroughKeepsMax(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const finalModel = "gpt-5.6-sol"
	controlCtx, cancelControl := context.WithCancelCause(context.Background())
	defer cancelControl(context.Canceled)

	upstream := newStagedPassthroughConn()
	upstream.Send(`{"type":"response.completed","response":{"id":"resp_floor_pass_max","model":"` + finalModel + `","usage":{"input_tokens":1,"output_tokens":1}}}`)
	turnResults := make(chan modelReasoningFloorTurnResult, 1)
	svc := newPassthroughLifecycleService(passthroughLifecycleConfig(), upstream)
	svc.settingService = floorTestSettings(t, finalModel)
	hooks := &OpenAIWSIngressHooks{
		MapRequestModel: func(_ int, model string) (string, error) {
			if strings.TrimSpace(model) == "floor-max-alias" {
				return finalModel, nil
			}
			return model, nil
		},
		AfterTurn: func(_ int, result *OpenAIForwardResult, turnErr error) {
			turnResults <- modelReasoningFloorTurnResult{result: result, err: turnErr}
		},
	}
	server, serverErr := startPassthroughLifecycleServerWithHooks(t, controlCtx, svc, passthroughLifecycleAccount(), func(*gin.Context) *OpenAIWSIngressHooks {
		return hooks
	})
	defer server.Close()

	clientConn := dialPassthroughLifecycleClientWithPayload(t, server, `{"type":"response.create","model":"floor-max-alias","reasoning":{"effort":"max"},"stream":false}`)
	defer func() { _ = clientConn.CloseNow() }()

	upstreamBody := requirePassthroughUpstreamWrite(t, upstream, time.Second)
	require.Equal(t, finalModel, gjson.GetBytes(upstreamBody, "model").String())
	require.Equal(t, "max", gjson.GetBytes(upstreamBody, "reasoning.effort").String())
	_, err := readPassthroughLifecycleFrame(t, clientConn, time.Second)
	require.NoError(t, err)
	require.NoError(t, clientConn.Close(coderws.StatusNormalClosure, "done"))

	select {
	case err := <-serverErr:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("passthrough max floor test did not exit")
	}

	turn := <-turnResults
	require.NoError(t, turn.err)
	require.NotNil(t, turn.result)
	require.NotNil(t, turn.result.RequestedReasoningEffort)
	require.Equal(t, "max", *turn.result.RequestedReasoningEffort)
	require.NotNil(t, turn.result.ReasoningEffort)
	require.Equal(t, "max", *turn.result.ReasoningEffort)
}

func writePassthroughFloorFrame(t *testing.T, clientConn *coderws.Conn, payload string) {
	t.Helper()
	writeCtx, cancelWrite := context.WithTimeout(context.Background(), time.Second)
	defer cancelWrite()
	require.NoError(t, clientConn.Write(writeCtx, coderws.MessageText, []byte(payload)))
}

func TestModelReasoningFloorWebSocketPooledIngress(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const finalModel = "gpt-5.6-pooled-floor"
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	cfg.Gateway.OpenAIWS.IngressModeDefault = OpenAIWSIngressModeCtxPool
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	cfg.Gateway.OpenAIWS.QueueLimitPerConn = 8
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3

	captureConn := &openAIWSCaptureConn{events: [][]byte{
		[]byte(`{"type":"response.completed","response":{"id":"resp_floor_pool_1","model":"` + finalModel + `","usage":{"input_tokens":1,"output_tokens":1}}}`),
		[]byte(`{"type":"response.completed","response":{"id":"resp_floor_pool_2","model":"` + finalModel + `","usage":{"input_tokens":1,"output_tokens":1}}}`),
	}}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(&openAIWSCaptureDialer{conn: captureConn})
	defer pool.Close()

	svc := &OpenAIGatewayService{
		cfg:              cfg,
		httpUpstream:     &httpUpstreamRecorder{},
		cache:            &stubGatewayCache{},
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg),
		toolCorrector:    NewCodexToolCorrector(),
		openaiWSPool:     pool,
		settingService:   floorTestSettings(t, finalModel),
	}
	account := &Account{
		ID:          1601,
		Name:        "model-reasoning-floor-pooled",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test", "model_mapping": map[string]any{"floor-alias": finalModel}},
		Extra:       map[string]any{"responses_websockets_v2_enabled": true},
	}
	turnResults := make(chan modelReasoningFloorTurnResult, 2)
	hooks := &OpenAIWSIngressHooks{
		MaxReasoningEffort: "medium",
		AfterTurn: func(_ int, result *OpenAIForwardResult, turnErr error) {
			turnResults <- modelReasoningFloorTurnResult{result: result, err: turnErr}
		},
	}
	server, serverErr := startModelReasoningFloorPooledServer(t, context.Background(), svc, account, hooks)
	defer server.Close()

	dialCtx, cancelDial := context.WithTimeout(context.Background(), time.Second)
	clientConn, _, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	cancelDial()
	require.NoError(t, err)
	defer func() { _ = clientConn.CloseNow() }()

	writePooled := func(payload string) {
		writeCtx, cancelWrite := context.WithTimeout(context.Background(), time.Second)
		defer cancelWrite()
		require.NoError(t, clientConn.Write(writeCtx, coderws.MessageText, []byte(payload)))
	}
	readPooled := func() []byte {
		readCtx, cancelRead := context.WithTimeout(context.Background(), time.Second)
		defer cancelRead()
		msgType, payload, readErr := clientConn.Read(readCtx)
		require.NoError(t, readErr)
		require.Equal(t, coderws.MessageText, msgType)
		return payload
	}

	writePooled(`{"type":"response.create","model":"floor-alias","reasoning":{"effort":"low"},"stream":false}`)
	require.Equal(t, "resp_floor_pool_1", gjson.GetBytes(readPooled(), "response.id").String())
	writePooled(`{"type":"response.create","reasoning":{"effort":"max"},"stream":false,"previous_response_id":"resp_floor_pool_1"}`)
	require.Equal(t, "resp_floor_pool_2", gjson.GetBytes(readPooled(), "response.id").String())
	require.NoError(t, clientConn.Close(coderws.StatusNormalClosure, "done"))

	select {
	case err := <-serverErr:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("pooled ingress floor test did not exit")
	}

	require.Len(t, captureConn.writes, 2)
	firstUpstream := requestToJSONString(captureConn.writes[0])
	secondUpstream := requestToJSONString(captureConn.writes[1])
	require.Equal(t, finalModel, gjson.Get(firstUpstream, "model").String())
	require.Equal(t, "xhigh", gjson.Get(firstUpstream, "reasoning.effort").String(), "group ceiling must not override the model floor")
	require.Equal(t, finalModel, gjson.Get(secondUpstream, "model").String())
	require.Equal(t, "xhigh", gjson.Get(secondUpstream, "reasoning.effort").String(), "omitted model follow-up must still use the floor")

	for i, want := range []struct {
		requested string
		effective string
	}{
		{requested: "low", effective: "xhigh"},
		{requested: "max", effective: "xhigh"},
	} {
		turn := <-turnResults
		require.NoError(t, turn.err, "turn %d", i+1)
		require.NotNil(t, turn.result, "turn %d", i+1)
		require.NotNil(t, turn.result.RequestedReasoningEffort, "turn %d requested", i+1)
		require.Equal(t, want.requested, *turn.result.RequestedReasoningEffort, "turn %d requested", i+1)
		require.NotNil(t, turn.result.ReasoningEffort, "turn %d effective", i+1)
		require.Equal(t, want.effective, *turn.result.ReasoningEffort, "turn %d effective", i+1)
	}
}

func startModelReasoningFloorPooledServer(
	t *testing.T,
	controlCtx context.Context,
	svc *OpenAIGatewayService,
	account *Account,
	hooks *OpenAIWSIngressHooks,
) (*httptest.Server, <-chan error) {
	t.Helper()
	serverErr := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, &coderws.AcceptOptions{CompressionMode: coderws.CompressionContextTakeover})
		if err != nil {
			serverErr <- err
			return
		}
		defer func() { _ = conn.CloseNow() }()

		recorder := httptest.NewRecorder()
		ginCtx, _ := gin.CreateTestContext(recorder)
		request := r.Clone(controlCtx)
		request.Header = request.Header.Clone()
		request.Header.Set("User-Agent", "unit-test-agent/1.0")
		ginCtx.Request = request

		readCtx, cancelRead := context.WithTimeout(controlCtx, 3*time.Second)
		msgType, firstMessage, readErr := conn.Read(readCtx)
		cancelRead()
		if readErr != nil {
			serverErr <- readErr
			return
		}
		if msgType != coderws.MessageText && msgType != coderws.MessageBinary {
			serverErr <- errors.New("unsupported websocket client message type")
			return
		}
		serverErr <- svc.ProxyResponsesWebSocketFromClient(controlCtx, ginCtx, conn, account, "sk-test", firstMessage, hooks)
	}))
	return server, serverErr
}
