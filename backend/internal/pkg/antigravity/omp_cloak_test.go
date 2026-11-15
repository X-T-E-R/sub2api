package antigravity

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// These are the upstream release templates, not reconstructed identities. The
// test exercises text nodes before/after rendering, including injected notices.
func TestOMPReleaseTemplatesRewriteSystemAndUser(t *testing.T) {
	r := NewCompiledSystemInstructionRewriter(DefaultSystemInstructionRewritePhrases())
	for _, version := range []string{"18.20.0", "18.20.2"} {
		t.Run(version, func(t *testing.T) {
			source, err := os.ReadFile("testdata/omp-" + version + "-system.md")
			require.NoError(t, err)
			input := strings.TrimSpace(string(source) + "\nomp://: harness docs, AVOID unless asked.\nrule://review agent://worker skill://test omp://guide\n")
			out := r.Rewrite(input)
			require.Equal(t, out, r.RewriteUser(input))
			require.NotEqual(t, input, out)
			require.NotContains(t, out, "You are omp's trusted coding assistant.")
			require.NotContains(t, out, "RFC 2119 keywords:")
			require.NotContains(t, out, "XML tags inject system content;")
			require.Contains(t, out, "trusted software development assistant")
			for _, address := range []string{"omp://guide", "rule://review", "agent://worker", "skill://test", "agent://<id>"} {
				require.Contains(t, out, address, "callable addresses must remain usable")
			}
			require.Contains(t, out, "Correctness, then six-month maintainability.")
			require.Equal(t, out, r.Rewrite(out))
			require.Equal(t, out, stripRewriteInvisible(out), "do not introduce invisible characters")

			// Exercise the real Claude Messages adapter into the outgoing wire.
			body, err := json.Marshal(input)
			require.NoError(t, err)
			req := &ClaudeRequest{Model: "gemini-3-flash", MaxTokens: 64, System: body,
				Messages: []ClaudeMessage{{Role: "user", Content: body}, {Role: "assistant", Content: body}}}
			wire, err := TransformClaudeToGeminiWithOptions(req, "project", req.Model, TransformOptions{
				SystemInstructionRewriteEnabled: true, SystemInstructionRewriter: r,
			})
			require.NoError(t, err)
			require.Equal(t, out, gjson.GetBytes(wire, "request.systemInstruction.parts.0.text").String())
			require.Equal(t, out, gjson.GetBytes(wire, "request.contents.0.parts.0.text").String())
			require.Equal(t, input, gjson.GetBytes(wire, "request.contents.1.parts.0.text").String())
		})
	}

	notice, err := os.ReadFile("testdata/omp-18.20.2-skill-notice.md")
	require.NoError(t, err)
	user := "<system-reminder>\nRFC 2119 keywords: MUST, REQUIRED, SHOULD, RECOMMENDED, MAY, OPTIONAL.\n" + string(notice) + "</system-reminder>"
	out := r.RewriteUser(user)
	require.Contains(t, out, "<system-notice id=\"skills\">")
	require.Contains(t, out, "read its instructions at `skill://<name>`")
	require.NotContains(t, out, "RFC 2119 keywords:")
	require.NotContains(t, out, "Matching skill → MUST read")
	require.Equal(t, out, r.RewriteUser(out))
}

func TestOMPUserPreambleWrappedAndInvisibleCopies(t *testing.T) {
	r := NewCompiledSystemInstructionRewriter(DefaultSystemInstructionRewritePhrases())
	preamble := "RFC 2119 keywords: MUST, REQUIRED, SHOULD, RECOMMENDED, MAY, OPTIONAL."
	want := "RFC 2119 key words: MUST, REQUIRED, SHOULD, RECOMMENDED, MAY, OPTIONAL."
	for _, input := range []string{
		strings.ReplaceAll(preamble, " ", "\r\n"),
		strings.ToUpper(preamble),
		strings.Join(strings.Split(preamble, ""), "\u2060"),
	} {
		require.Equal(t, want, r.RewriteUser(input))
	}
	input := "You are an expert coding assistant operating inside pi, a coding agent harness."
	require.Equal(t, "You are an expert coding assistant.", r.RewriteUser(input))
	input = "You are a coding assistant running inside pi."
	require.Equal(t, "You are a coding assistant running within pi.", r.RewriteUser(input))
}
