//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// These declarations are exact parameter fixtures extracted from the task-0007
// saved-request/source ledger. Expected transformations below are hand-authored.
var task0007GrokSchemaFixtures = map[string]json.RawMessage{
	"codex_create_thread_actual":           json.RawMessage(`{"type":"object","required":["prompt","target"],"properties":{"model":{"type":"string","description":"Codex threads only. Do not specify a model unless the user explicitly requests a specific model. Otherwise omit this field so the new thread uses the user's configured default model. Omit for ChatGPT Work cloud threads. Models and supported reasoning efforts on the calling host: gpt-5.6-sol (Latest frontier agentic coding model.; supported reasoning efforts: low, medium, high, xhigh, max, ultra), gpt-5.6-terra (Balanced agentic coding model for everyday work.; supported reasoning efforts: low, medium, high, xhigh, max, ultra), gpt-5.6-luna (Fast and affordable agentic coding model.; supported reasoning efforts: low, medium, high, xhigh, max), grok-4.6 (xAI frontier model for coding and agentic work. Native API context is 500k; this catalog caps Codex at 400k. Text and image in, text out. Function calling and structured outputs. Reasoning cannot be disabled; default effort is high.; supported reasoning efforts: low, medium, high, xhigh), deepseek-v4-flash (DeepSeek V4 Flash (API id deepseek-v4-flash, current version DeepSeek-V4-Flash-0731). Native API context is 1M with 384k max output; this catalog caps Codex at 400k. Text-only input, tool calls, JSON output. Thinking is on by default; native efforts are low/high/max.; supported reasoning efforts: low, high, max), gpt-5.5 (Frontier model for complex coding, research, and real-world work.; supported reasoning efforts: low, medium, high, xhigh), gpt-5.2 (Optimized for professional work and long-running agents.; supported reasoning efforts: low, medium, high, xhigh). A different destination host's model availability and reasoning combinations are validated when the tool runs."},"title":{"type":"string","description":"Optional title applied when the thread is created, including while a worktree is pending. It is normalized like an automatically generated title."},"prompt":{"type":"string","description":"Initial prompt for the new thread."},"target":{"anyOf":[{"type":"object","required":["type","projectId","environment"],"properties":{"type":{"enum":["project"],"type":"string"},"projectId":{"type":"string","description":"Project id returned by list_projects."},"environment":{"anyOf":[{"type":"object","required":["type"],"properties":{"type":{"enum":["local"],"type":"string"}},"additionalProperties":false},{"type":"object","required":["type"],"properties":{"type":{"enum":["worktree"],"type":"string"},"startingState":{"anyOf":[{"type":"object","required":["type"],"properties":{"type":{"enum":["working-tree"],"type":"string"}},"additionalProperties":false},{"type":"object","required":["type","branchName"],"properties":{"type":{"enum":["branch"],"type":"string"},"onMissing":{"enum":["error","create-branch"],"type":"string","description":"What to do when branchName does not exist. Omission is equivalent to \"error\". Use \"create-branch\" only when the user explicitly requested a new branch with this exact name; the branch is created from the project default branch."},"branchName":{"type":"string","description":"The branch or ref to start from. Never invent this value. It may name a new branch only when the user requested that exact name and onMissing is \"create-branch\"."}},"additionalProperties":false}],"description":"Only specify this when the user explicitly asks to start from a particular git state. Use working-tree to include the current checkout and uncommitted changes. Use branch for an existing branch or ref. To create a user-requested branch when it does not exist, set onMissing to \"create-branch\"; otherwise omission defaults to an error. Omit startingState to start from the project's default branch."}},"additionalProperties":false}],"description":"Where the project thread should run. Check the selected project's isGitRepository value from list_projects: default to worktree when it is true and use local otherwise; local runs directly in the saved project on its configured host. Follow an explicit user request to use the saved project directly."}},"additionalProperties":false},{"type":"object","required":["type"],"properties":{"type":{"enum":["projectless"],"type":"string"},"directoryName":{"type":"string","description":"Optional projectless output directory name."}},"additionalProperties":false},{"type":"object","required":["type"],"properties":{"type":{"enum":["chatgptWorkCloud"],"type":"string","description":"Create a cloud ChatGPT Work task."},"projectId":{"type":"string","description":"Optional ChatGPT project id returned by list_projects. Omit for a projectless cloud task."}},"additionalProperties":false}],"description":"Where to create the thread."},"thinking":{"enum":["none","minimal","low","medium","high","xhigh","max","ultra"],"type":"string","description":"Optional Codex reasoning effort override. Must be supported by the selected model. Omit for ChatGPT Work cloud threads."}},"additionalProperties":false}`),
	"codex_automation_update_actual":       json.RawMessage(`{"$defs":{"__schema0":{"type":"object","required":["mode","id"],"properties":{"id":{"$ref":"#/$defs/__schema1"},"mode":{"enum":["view"],"type":"string"}},"additionalProperties":false},"__schema1":{"$ref":"#/$defs/__schema2"},"__schema2":{"type":"string"},"__schema3":{"oneOf":[{"$ref":"#/$defs/__schema4"},{"$ref":"#/$defs/__schema17"}]},"__schema4":{"type":"object","required":["name","prompt","rrule","status","kind","projectId","model","reasoningEffort","mode","executionEnvironment"],"properties":{"kind":{"$ref":"#/$defs/__schema11"},"mode":{"$ref":"#/$defs/__schema16"},"name":{"$ref":"#/$defs/__schema5"},"model":{"$ref":"#/$defs/__schema14"},"rrule":{"$ref":"#/$defs/__schema7"},"prompt":{"$ref":"#/$defs/__schema6"},"status":{"$ref":"#/$defs/__schema8"},"projectId":{"$ref":"#/$defs/__schema12"},"destination":{"enum":["local"],"type":"string"},"reasoningEffort":{"$ref":"#/$defs/__schema15"},"notificationPolicy":{"$ref":"#/$defs/__schema9"},"executionEnvironment":{"enum":["local"],"type":"string"}},"additionalProperties":false},"__schema5":{"$ref":"#/$defs/__schema2"},"__schema6":{"$ref":"#/$defs/__schema2"},"__schema7":{"$ref":"#/$defs/__schema2"},"__schema8":{"enum":["ACTIVE","PAUSED"],"type":"string"},"__schema9":{"$ref":"#/$defs/__schema10"},"__schema10":{"anyOf":[{"enum":["failed_runs_only"],"type":"string"},{"type":"null"}]},"__schema11":{"enum":["cron"],"type":"string"},"__schema12":{"anyOf":[{"$ref":"#/$defs/__schema13"},{"type":"null"}]},"__schema13":{"type":"string"},"__schema14":{"$ref":"#/$defs/__schema2"},"__schema15":{"enum":["none","minimal","low","medium","high","xhigh","max","ultra"],"type":"string"},"__schema16":{"enum":["create","suggested_create"],"type":"string"},"__schema17":{"type":"object","required":["name","prompt","rrule","status","kind","mode"],"properties":{"kind":{"$ref":"#/$defs/__schema18"},"mode":{"$ref":"#/$defs/__schema16"},"name":{"$ref":"#/$defs/__schema5"},"rrule":{"$ref":"#/$defs/__schema7"},"prompt":{"$ref":"#/$defs/__schema6"},"status":{"$ref":"#/$defs/__schema8"},"destination":{"$ref":"#/$defs/__schema19"},"targetThreadId":{"$ref":"#/$defs/__schema20"},"notificationPolicy":{"$ref":"#/$defs/__schema9"}},"additionalProperties":false},"__schema18":{"enum":["heartbeat"],"type":"string"},"__schema19":{"enum":["local","thread"],"type":"string"},"__schema20":{"$ref":"#/$defs/__schema2","type":"string"},"__schema21":{"oneOf":[{"type":"object","required":["name","prompt","rrule","status","kind","projectId","model","reasoningEffort","mode","id","executionEnvironment"],"properties":{"id":{"$ref":"#/$defs/__schema1"},"kind":{"$ref":"#/$defs/__schema11"},"mode":{"$ref":"#/$defs/__schema23"},"name":{"$ref":"#/$defs/__schema5"},"model":{"$ref":"#/$defs/__schema14"},"rrule":{"$ref":"#/$defs/__schema22"},"prompt":{"$ref":"#/$defs/__schema6"},"status":{"$ref":"#/$defs/__schema8"},"projectId":{"$ref":"#/$defs/__schema12"},"destination":{"enum":["local","worktree"],"type":"string"},"reasoningEffort":{"$ref":"#/$defs/__schema15"},"notificationPolicy":{"$ref":"#/$defs/__schema9"},"executionEnvironment":{"enum":["worktree","local"],"type":"string"},"localEnvironmentConfigPath":{"anyOf":[{"type":"string"},{"type":"null"}]}},"additionalProperties":false},{"type":"object","required":["name","prompt","rrule","status","kind","mode","id"],"properties":{"id":{"$ref":"#/$defs/__schema1"},"kind":{"$ref":"#/$defs/__schema18"},"mode":{"$ref":"#/$defs/__schema23"},"name":{"$ref":"#/$defs/__schema5"},"rrule":{"$ref":"#/$defs/__schema22"},"prompt":{"$ref":"#/$defs/__schema6"},"status":{"$ref":"#/$defs/__schema8"},"destination":{"$ref":"#/$defs/__schema19"},"targetThreadId":{"$ref":"#/$defs/__schema20"},"notificationPolicy":{"$ref":"#/$defs/__schema9"}},"additionalProperties":false}]},"__schema22":{"$ref":"#/$defs/__schema2"},"__schema23":{"enum":["update","suggested_update"],"type":"string"},"__schema24":{"type":"object","required":["mode","id"],"properties":{"id":{"$ref":"#/$defs/__schema1"},"mode":{"enum":["delete"],"type":"string"}},"additionalProperties":false}},"oneOf":[{"$ref":"#/$defs/__schema0"},{"$ref":"#/$defs/__schema3"},{"$ref":"#/$defs/__schema21"},{"$ref":"#/$defs/__schema24"}]}`),
	"codex_app_wait_threads_old_actual":    json.RawMessage(`{"type":"object","required":["targets"],"properties":{"targets":{"type":"array","items":{"type":"object","required":["threadId"],"properties":{"hostId":{"type":"string","description":"Optional host id returned by create_thread or list_threads."},"threadId":{"type":"string","description":"Thread id to wait for."},"afterCursor":{"type":"string","description":"Optional cursor returned by an earlier wait."}},"additionalProperties":false},"description":"Threads to wait for. The first target that completes or needs attention wins."},"timeoutMs":{"type":"integer","description":"Maximum event-wait time in milliseconds. A bounded snapshot fetch for fresh progress may add latency. Defaults to 120000."}},"additionalProperties":false}`),
	"codex_wait_agent_v1_actual":           json.RawMessage(`{"type":"object","required":["targets"],"properties":{"targets":{"type":"array","items":{"type":"string"},"description":"Agent ids to wait on. Pass multiple ids to wait for whichever finishes first."},"timeout_ms":{"type":"number","description":"Timeout in milliseconds. Defaults to 30000, min 10000, max 3600000. Prefer longer waits (minutes) to avoid busy polling."}},"additionalProperties":false}`),
	"codex_wait_agent_v2_actual":           json.RawMessage(`{"type":"object","properties":{"timeout_ms":{"type":"number","description":"Timeout in milliseconds. Defaults to 30000, min 10000, max 3600000."}},"additionalProperties":false}`),
	"kiki_wait_threads_actual_control":     json.RawMessage(`{"type":"object","$schema":"http://json-schema.org/draft-07/schema#","required":["threads"],"properties":{"threads":{"type":"array","items":{"type":"object","required":["thread"],"properties":{"cursor":{"type":"string","maxLength":4096,"minLength":1},"thread":{"type":"object","required":["host_id","workspace_id","session_id"],"properties":{"host_id":{"type":"string","maxLength":256,"minLength":1},"session_id":{"type":"string","maxLength":256,"minLength":1},"workspace_id":{"type":"string","maxLength":512,"minLength":1}},"additionalProperties":false}},"additionalProperties":false},"maxItems":8,"minItems":1},"timeout_ms":{"type":"integer","maximum":60000,"minimum":0}},"additionalProperties":false}`),
	"kiki_wait_agent_v2_synthetic_control": json.RawMessage(`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"timeout_ms":{"type":"integer","minimum":10000,"maximum":3600000}},"additionalProperties":false}`),
}

func TestNormalizeGrokFunctionParametersPreservesRealCodexKikiSchemas(t *testing.T) {
	for name, raw := range task0007GrokSchemaFixtures {
		t.Run(name, func(t *testing.T) {
			normalized, disposition := normalizeGrokFunctionParameters(raw)
			require.Equal(t, grokSchemaProvenObject, disposition)

			var want map[string]any
			require.NoError(t, decodeOpenAIJSONUseNumber(raw, &want))
			delete(want, "$schema")
			want["type"] = "object"
			if name == "codex_automation_update_actual" {
				branches := want["oneOf"].([]any)
				for _, rawBranch := range branches {
					rawBranch.(map[string]any)["type"] = "object"
				}
			}
			wantJSON, err := json.Marshal(want)
			require.NoError(t, err)
			require.JSONEq(t, string(wantJSON), string(normalized))
		})
	}
}

func TestNormalizeGrokFunctionParametersRealSchemaActionableConstraints(t *testing.T) {
	tests := []struct {
		name   string
		checks map[string]string
	}{
		{name: "codex_create_thread_actual", checks: map[string]string{"required.1": "target", "properties.target.anyOf.#": "3", "properties.target.anyOf.0.properties.environment.anyOf.#": "2"}},
		{name: "codex_automation_update_actual", checks: map[string]string{"oneOf.#": "4", "oneOf.0.type": "object", "oneOf.3.type": "object", "$defs.__schema4.required.0": "name", "$defs.__schema21.oneOf.#": "2", "$defs.__schema0.properties.id.$ref": "#/$defs/__schema1"}},
		{name: "codex_app_wait_threads_old_actual", checks: map[string]string{"required.0": "targets", "properties.targets.type": "array", "properties.targets.items.required.0": "threadId"}},
		{name: "codex_wait_agent_v1_actual", checks: map[string]string{"required.0": "targets", "properties.targets.items.type": "string"}},
		{name: "kiki_wait_threads_actual_control", checks: map[string]string{"required.0": "threads", "properties.threads.minItems": "1", "properties.threads.items.properties.thread.required.#": "3", "properties.timeout_ms.maximum": "60000"}},
		{name: "kiki_wait_agent_v2_synthetic_control", checks: map[string]string{"properties.timeout_ms.type": "integer", "properties.timeout_ms.minimum": "10000", "properties.timeout_ms.maximum": "3600000"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			normalized, disposition := normalizeGrokFunctionParameters(task0007GrokSchemaFixtures[tt.name])
			require.Equal(t, grokSchemaProvenObject, disposition)
			for path, want := range tt.checks {
				require.Equal(t, want, gjson.GetBytes(normalized, path).String(), path)
			}
		})
	}
}

func TestGrokComplexSchemaSharedPreparationRoutesAndCompatOff(t *testing.T) {
	raw := task0007GrokSchemaFixtures["kiki_wait_threads_actual_control"]
	tool := `{"type":"function","name":"wait_for_work","strict":true,"parameters":` + string(raw) + `}`
	assertPrepared := func(t *testing.T, body []byte) {
		t.Helper()
		require.Equal(t, "object", gjson.GetBytes(body, "tools.0.parameters.type").String())
		require.False(t, gjson.GetBytes(body, "tools.0.parameters.$schema").Exists())
		require.Equal(t, int64(1), gjson.GetBytes(body, "tools.0.parameters.properties.threads.minItems").Int())
		require.Equal(t, int64(3), gjson.GetBytes(body, "tools.0.parameters.properties.threads.items.properties.thread.required.#").Int())
	}

	native, _, err := patchGrokResponsesBodyWithClientToolsCompat([]byte(`{"model":"grok-4.6","input":"hi","tools":[`+tool+`]}`), "grok-4.6", true)
	require.NoError(t, err)
	assertPrepared(t, native)

	messagesReq := &apicompat.AnthropicRequest{
		Model: "grok-4.6", MaxTokens: 32,
		Messages: []apicompat.AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Tools:    []apicompat.AnthropicTool{{Name: "wait_for_work", InputSchema: raw}},
	}
	convertedMessages, err := apicompat.AnthropicToResponses(messagesReq)
	require.NoError(t, err)
	messagesBody, err := json.Marshal(convertedMessages)
	require.NoError(t, err)
	messagesBody, err = patchGrokResponsesBodyBaseWithCompat(messagesBody, "grok-4.6", true)
	require.NoError(t, err)
	assertPrepared(t, messagesBody)

	chatBody := []byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"wait_for_work","strict":true,"parameters":` + string(raw) + `}}]}`)
	eligible, reason := grokChatResponsesBridgeEligibilityWithCompat(chatBody, true)
	require.True(t, eligible, reason)
	var chatReq apicompat.ChatCompletionsRequest
	require.NoError(t, json.Unmarshal(chatBody, &chatReq))
	convertedChat, err := apicompat.ChatCompletionsToResponses(&chatReq)
	require.NoError(t, err)
	chatResponsesBody, err := json.Marshal(convertedChat)
	require.NoError(t, err)
	chatResponsesBody, err = patchGrokResponsesBodyBaseWithCompat(chatResponsesBody, "grok-4.6", true)
	require.NoError(t, err)
	assertPrepared(t, chatResponsesBody)

	account := healthyGrokOAuthGatewayTestAccount(9904, "access-token")
	ws, err := prepareGrokWSResponsesBody([]byte(`{"type":"response.create","model":"grok-4.6","input":"hi","tools":[`+tool+`]}`), account, "grok-4.6", apicompat.ResponsesClientToolMapping{}, nil, true)
	require.NoError(t, err)
	assertPrepared(t, ws.Body)

	offBody := []byte(`{"tools":[` + tool + `]}`)
	off, err := sanitizeGrokResponsesToolsWithCompat(offBody, false)
	require.NoError(t, err)
	require.JSONEq(t, string(raw), gjson.GetBytes(off, "tools.0.parameters").Raw)
	require.True(t, gjson.GetBytes(off, "tools.0.strict").Bool())
}

func TestForwardGrokOAuthChatComplexSchemaUsesActualResponsesEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	raw := task0007GrokSchemaFixtures["kiki_wait_threads_actual_control"]
	body := []byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"wait"}],"stream":false,"tools":[{"type":"function","function":{"name":"wait_for_work","strict":true,"parameters":` + string(raw) + `}}]}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, grokChatRawEndpoint, bytes.NewReader(body))
	c.Set("api_key", &APIKey{ID: 9905})
	account := grokChatBridgeTestAccount(9905)
	repo := &grokQuotaAccountRepo{mockAccountRepoForPlatform: &mockAccountRepoForPlatform{accountsByID: map[int64]*Account{account.ID: account}}}
	upstream := &httpUpstreamRecorder{resp: grokChatBridgeCompletedResponse("resp_complex_schema", 0)}
	svc := &OpenAIGatewayService{httpUpstream: upstream, grokTokenProvider: NewGrokTokenProvider(repo, nil), accountRepo: repo}

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
	require.NoError(t, err)
	require.Equal(t, grokChatResponsesEndpoint, result.UpstreamEndpoint)
	require.Equal(t, xai.DefaultCLIBaseURL+"/responses", upstream.lastReq.URL.String())
	require.Equal(t, "wait_for_work", gjson.GetBytes(upstream.lastBody, "tools.0.name").String())
	require.Equal(t, "object", gjson.GetBytes(upstream.lastBody, "tools.0.parameters.type").String())
	require.False(t, gjson.GetBytes(upstream.lastBody, "tools.0.parameters.$schema").Exists())
	require.Equal(t, int64(1), gjson.GetBytes(upstream.lastBody, "tools.0.parameters.properties.threads.minItems").Int())
	require.Equal(t, int64(3), gjson.GetBytes(upstream.lastBody, "tools.0.parameters.properties.threads.items.properties.thread.required.#").Int())
	require.True(t, gjson.GetBytes(upstream.lastBody, "tools.0.strict").Bool())
}
