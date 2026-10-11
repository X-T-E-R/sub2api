package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 本文件全部离线：不读配置、不开数据库/Redis、不发任何网络请求。
// dry-run / -h / 坏参数的“零副作用”由桩 generationFunc 守住：它一旦被调用就 t.Fatal。

// TestMain 关掉 gin 的调试横幅（本包单测构造真实 gin.Context；生产路径里这一行出口已在
// silenceLibraryOutput 中被接到 os.DevNull）。
func TestMain(m *testing.M) {
	gin.SetMode(gin.ReleaseMode)
	gin.DefaultWriter = io.Discard
	gin.DefaultErrorWriter = io.Discard
	os.Exit(m.Run())
}

type stub struct {
	calls int
	run   func(context.Context, options, *eventStats) error
}

func (s *stub) gen(t *testing.T) generationFunc {
	return func(ctx context.Context, opts options, stats *eventStats) error {
		s.calls++
		if s.run == nil {
			t.Fatalf("generation must not run for this invocation")
		}
		return s.run(ctx, opts, stats)
	}
}

// -h / dry-run 必须在任何副作用之前返回，且不得触碰 CONFIG_FILE。
func TestRunWithoutExecuteHasNoSideEffects(t *testing.T) {
	t.Setenv(configEnvVar, "sentinel.yaml")
	for _, args := range [][]string{nil, {"-h"}, {"-config", "other.yaml"}} {
		stub := &stub{}
		var stdout, stderr bytes.Buffer

		code := run(args, &stdout, &stderr, stub.gen(t))

		require.Equal(t, exitOK, code)
		require.Zero(t, stub.calls)
		require.Equal(t, "sentinel.yaml", os.Getenv(configEnvVar), "dry-run 不得设置 CONFIG_FILE")
		if len(args) > 0 && args[0] == "-h" {
			require.Empty(t, stdout.String(), "用法只允许出现在 stderr")
			require.Contains(t, stderr.String(), "写副作用")
			continue
		}
		var out summary
		require.NoError(t, json.Unmarshal(stdout.Bytes(), &out))
		require.True(t, out.DryRun)
		require.Equal(t, stageDryRun, out.Stage)
		require.Equal(t, "not_attempted", out.Network)
		require.Equal(t, fixedModel, out.Model)
		require.Equal(t, fixedEffort, out.ReasoningEffort)
		require.False(t, out.Success)
	}
}

// 参数失败关闭：超时越界、未知参数都不产生 stdout，也不触发重活。
func TestRunFailsClosedOnArguments(t *testing.T) {
	for _, args := range [][]string{{"-execute", "-timeout", "0s"}, {"-execute", "-timeout", "121s"}, {"-nope"}} {
		stub := &stub{}
		var stdout, stderr bytes.Buffer

		code := run(args, &stdout, &stderr, stub.gen(t))

		require.Equal(t, exitFatal, code)
		require.Empty(t, stdout.String())
		require.NotEmpty(t, stderr.String())
		require.Zero(t, stub.calls)
	}
}

// -execute 恰好调用一次；上限 120s 被接受。
func TestRunExecutesOnceWithFixedTarget(t *testing.T) {
	stub := &stub{run: func(_ context.Context, _ options, stats *eventStats) error {
		stats.observe(`{"type":"test_complete","success":true}`)
		return nil
	}}
	var stdout, stderr bytes.Buffer

	code := run([]string{"-execute", "-timeout", "120s"}, &stdout, &stderr, stub.gen(t))

	require.Equal(t, exitOK, code)
	require.Equal(t, 1, stub.calls)
	require.Empty(t, stderr.String())
	var out summary
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &out))
	require.Equal(t, int64(120), out.TimeoutSeconds)
	require.Equal(t, stageOK, out.Stage)
	require.True(t, out.Success)
}

// 环境阶段失败=退出码 2 且无 stdout；生成阶段失败=退出码 1 且 stage 有限、库文本不外泄。
func TestRunFiniteStagesAndPanic(t *testing.T) {
	for _, tt := range []struct {
		name  string
		err   error
		code  int
		stage string
	}{
		{"config", newStageError(stageConfigLoad), exitFatal, ""},
		{"database", newStageError(stageDatabase), exitFatal, ""},
		{"redis", newStageError(stageRedis), exitFatal, ""},
		{"generation", errors.New("Request failed: ya29.leaked refresh_token=1//x"), exitFailed, stageGenerationFailed},
		{"incomplete", nil, exitFailed, stageStreamIncomplete},
		{"timeout", newStageError(stageTimeout), exitFailed, stageTimeout},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stub := &stub{run: func(context.Context, options, *eventStats) error { return tt.err }}
			var stdout, stderr bytes.Buffer

			code := run([]string{"-execute"}, &stdout, &stderr, stub.gen(t))

			require.Equal(t, tt.code, code)
			require.NotContains(t, stdout.String(), "ya29.leaked")
			require.NotContains(t, stderr.String(), "ya29.leaked")
			if tt.stage == "" {
				require.Empty(t, stdout.String())
				return
			}
			var out summary
			require.NoError(t, json.Unmarshal(stdout.Bytes(), &out))
			require.Equal(t, tt.stage, out.Stage)
			require.False(t, out.Success)
		})
	}

	// panic 也折成有限 stage：不打印堆栈，也不回显 panic 值。
	stub := &stub{run: func(context.Context, options, *eventStats) error { panic("token=ya29.panic") }}
	var stdout, stderr bytes.Buffer
	require.Equal(t, exitFailed, run([]string{"-execute"}, &stdout, &stderr, stub.gen(t)))
	require.NotContains(t, stdout.String(), "ya29.panic")
	require.NotContains(t, stderr.String(), "goroutine")
	require.Contains(t, stdout.String(), stageInternalPanic)
}

// fixture 是“上游什么都可能发”的合成字节流：白名单事件 + 思维块/签名原始帧 + tool_use 原始
// 载荷 + error 事件 + 内联 base64 媒体 + 非 SSE 原始响应体。
func fixture() string {
	frames := []string{
		`{"type":"test_start","requested_model":"gemini-3.8-flash","upstream_sent_model":"gemini-3.8-flash-medium","effective_prompt":"Say hello in one short sentence.","effective_effort":"medium","stream_mode":"stream","data":{"raw":"should-not-leak"}}`,
		`{"type":"content","text":"Hello"}`,
		`{"type":"content","text":"{\"type\":\"tool_use\",\"input\":{\"secret\":\"should-not-leak\"},\"signature\":\"ErUBCkYIBRgCIkAxYz\"}"}`,
		`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"thought should-not-leak","signature":"ErUBCkYIBRgCIkAxYz"}}`,
		`{"type":"content","text":" there"}`,
		`{"type":"error","error":"API returned 401: {\"access_token\":\"ya29.should-not-leak\"}","code":"invalid_grant"}`,
		`{"type":"image","image_url":"data:image/png;base64,c2hvdWxkLW5vdC1sZWFr"}`,
		`{"type":"test_metadata","upstream_response_model":"gemini-3.8-flash","upstream_response_model_conflict":true}`,
		`{"type":"test_complete","success":true,"upstream_response_model_conflict":false}`,
	}
	var builder strings.Builder
	for _, frame := range frames {
		builder.WriteString("data: " + frame + "\n\n")
	}
	// 非 SSE 的原始响应体（正式代码在错误分支会直接写 writer）；半帧再补全，验证拼帧。
	builder.WriteString(`{"error":{"message":"upstream body should-not-leak ya29.should-not-leak"}}` + "\n")
	return builder.String()
}

// TestSmokeWriterCountsOnlyMetadataAndNonJSONContent 是出口契约：汇总里只有元数据与计数，
// 正文、工具载荷、签名、error 文本、base64 媒体在任何输出流上都找不到。
func TestSmokeWriterCountsOnlyMetadataAndNonJSONContent(t *testing.T) {
	stub := &stub{run: func(_ context.Context, _ options, stats *eventStats) error {
		ginCtx, _ := gin.CreateTestContext(newSmokeWriter(stats))
		split := len(fixture()) / 2
		if _, err := ginCtx.Writer.WriteString(fixture()[:split]); err != nil {
			return err
		}
		_, err := ginCtx.Writer.WriteString(fixture()[split:])
		return err
	}}
	var stdout, stderr bytes.Buffer

	code := run([]string{"-execute"}, &stdout, &stderr, stub.gen(t))

	require.Equal(t, exitOK, code)
	for _, forbidden := range []string{
		"should-not-leak", "Hello", "there", "ErUBCkYIBRgCIkAxYz", "signature", "thinking",
		"ya29.", "access_token", "invalid_grant", "tool_use", "base64", "image_url", "upstream body",
	} {
		require.NotContains(t, stdout.String(), forbidden, "禁止出现在 stdout 上的片段: %s", forbidden)
		require.NotContains(t, stderr.String(), forbidden)
	}

	var out summary
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &out))
	require.Equal(t, "gemini-3.8-flash", out.RequestedModel)
	require.Equal(t, "gemini-3.8-flash-medium", out.UpstreamSentModel)
	require.Equal(t, "gemini-3.8-flash", out.UpstreamResponseModel)
	require.True(t, out.UpstreamResponseModelConflict, "冲突标志必须活过收尾 test_complete 的 false")
	require.Equal(t, "Say hello in one short sentence.", out.EffectivePrompt)
	require.Equal(t, "medium", out.EffectiveEffort)
	require.Equal(t, "stream", out.StreamMode)
	require.Equal(t, 2, out.ChunkCount, "只有两段模型文本；tool_use 的 JSON 载荷不计数")
	require.Equal(t, 11, out.TextChars)
	require.NotNil(t, out.FirstContentMs)
	require.True(t, out.Success)
	require.Equal(t, stageOK, out.Stage)
	require.GreaterOrEqual(t, out.ElapsedMs, int64(0))
}
