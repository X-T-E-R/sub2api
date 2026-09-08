package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAIJSONNoOpBoundaries(t *testing.T) {
	for _, tc := range []struct {
		body            string
		legacy, compact bool
	}{
		{``, false, false},
		{`null`, false, false},
		{` {"input":null} `, false, false},
		{`{"input":42,"prompt":null}`, false, false},
		{`{"input":[],"prompt":{}}`, false, false},
		{`{"input":[],"messages":null}`, true, false},
		{`{"messages":42}`, true, false},
		{`{"commands":null}`, true, false},
		{`{"pr\u006fmpt":"hello"}`, true, false},
		{`{"\u006dessages":[]}`, true, false},
		{`{"comm\u0061nds":{}}`, true, false},
		{`{"prompt":"hello","prompt":{}}`, false, false},
		{`{"prompt":{},"prompt":"hello"}`, true, false},
		{`{"messages":[{"role":"user","content":"ignored"}],"messages":null}`, true, false},
		{`{"input":[{"type":"compaction_trigger"},{"type":"message"}]}`, false, true},
		{`{"in\u0070ut":[{"t\u0079pe":"compaction_\u0074rigger"},{}]}`, false, true},
		{`{"input":[{"type":"compaction_trigger","type":"message"},{}]}`, false, false},
		{`{"input":[{"type":"message","type":"compaction_trigger"},{}]}`, false, true},
		{`{"input":[{"type":"compaction_trigger"},{}],"input":[]}`, false, false},
		{`{"input":[],"input":[{"type":"compaction_trigger"},{}]}`, false, true},
		{`{"input":[{"type":null},{"type":42},"compaction_trigger"]}`, false, false},
		{`{"input":[{"type":"compaction_trigger"}]}`, false, false},
		{`{"input":[{"content":{"type":"compaction_trigger","messages":[],"prompt":"nested"}}]}`, false, false},
		{`{"input":"quoted \"messages\" and compaction_trigger; slash \\; Unicode \u4e2d"}`, false, false},
	} {
		t.Run(tc.body, func(t *testing.T) {
			body := []byte(tc.body)
			for _, check := range []struct {
				run  func([]byte) ([]byte, bool, error)
				want bool
			}{{normalizeOpenAIResponsesLegacyIngress, tc.legacy}, {NormalizeCompactionTriggerInputOrder, tc.compact}} {
				out, changed, err := check.run(body)
				require.NoError(t, err)
				require.Equal(t, check.want, changed)
				if !changed && len(body) > 0 {
					require.True(t, &body[0] == &out[0], "unchanged output must alias input")
				}
			}
		})
	}
}

func TestOpenAIJSONNoOpErrorsMatchDecoder(t *testing.T) {
	for _, body := range []string{` `, `[]`, `42`, `true`, `"text"`, `{"input":`, `{} {}`, `{} true`, "{}\xff", `{"x":"\q"}`, `{"x":1e}`, "{\"x\":\"\x01\"}", `{"x":` + string(bytes.Repeat([]byte("["), 10001))} {
		t.Run(body[:min(len(body), 40)], func(t *testing.T) {
			var payload map[string]any
			want := decodeOpenAIJSONUseNumber([]byte(body), &payload)
			require.Error(t, want)
			_, changed, err := NormalizeCompactionTriggerInputOrder([]byte(body))
			require.EqualError(t, err, want.Error())
			require.False(t, changed)
			_, changed, err = normalizeOpenAIResponsesLegacyIngress([]byte(body))
			require.EqualError(t, err, "normalize legacy Responses ingress: "+want.Error())
			require.False(t, changed)
		})
	}
}

func TestOpenAIJSONNoOpLargeAllocations(t *testing.T) {
	for _, size := range []int{10, 30} {
		t.Run(fmt.Sprintf("%dMiB", size), func(t *testing.T) {
			// Include frequent escaped quotes, backslashes, and Unicode escapes:
			// escaped ordinary content must not disable the no-op guard.
			text := bytes.Repeat([]byte(`text\n\"quoted\"\\\u4e2d `), (size<<20)/len(`text\n\"quoted\"\\\u4e2d `))
			body := append([]byte(`{"input":[{"type":"message","content":"`), text...)
			body = append(body, []byte(`"}]}`)...)
			require.True(t, json.Valid(body))
			for _, run := range []func([]byte) ([]byte, bool, error){normalizeOpenAIResponsesLegacyIngress, NormalizeCompactionTriggerInputOrder} {
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				out, changed, err := run(body)
				runtime.ReadMemStats(&after)
				require.NoError(t, err)
				require.False(t, changed)
				require.True(t, &body[0] == &out[0], "unchanged output must alias input")
				require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(1<<20), "no-op must not allocate proportional to request size")
			}
		})
	}
}

func FuzzOpenAIJSONNoOpGuard(f *testing.F) {
	for _, seed := range []string{`{}`, `{"prompt":"hello"}`, `{"\u006dessages":[]}`, `{"input":[{"type":"compaction_\u0074rigger"}]}`, `{"x":"\\\"prompt"}`, `{"x":"\ud800"}`} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		candidates := []string{"messages", "prompt", "commands", "compaction_trigger"}
		if !openAIJSONObjectExcludesStrings(body, candidates...) {
			return
		}
		var decoded map[string]any
		require.NoError(t, decodeOpenAIJSONUseNumber(body, &decoded))
		var check func(any)
		check = func(value any) {
			switch v := value.(type) {
			case string:
				for _, candidate := range candidates {
					require.NotEqual(t, candidate, v)
				}
			case map[string]any:
				for key, item := range v {
					check(key)
					check(item)
				}
			case []any:
				for _, item := range v {
					check(item)
				}
			}
		}
		check(decoded)
	})
}
