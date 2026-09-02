package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

type openAICyberTranscriptBlockKeys struct {
	lookupKeys          []string
	lookupKeysTruncated bool
}

// Bound the Redis lookup work for a single request while retaining the most
// recent transcript prefixes, where a continuation is most likely to match.
const maxOpenAICyberTranscriptLookupKeys = 256

// deriveOpenAICyberTranscriptBlockKeys returns domain-separated cumulative
// hashes of the canonical transcript. A refused request stores only the final
// digest; a later request can therefore match only when that exact transcript
// is an ancestor. Rewriting the latest user turn does not match an earlier,
// shorter prefix because that shorter digest was never stored.
func deriveOpenAICyberTranscriptBlockKeys(apiKeyID int64, body []byte) openAICyberTranscriptBlockKeys {
	if apiKeyID <= 0 || len(body) == 0 {
		return openAICyberTranscriptBlockKeys{}
	}
	root := openAIRequestPayloadView(body)
	if !root.Exists() || !root.IsObject() {
		return openAICyberTranscriptBlockKeys{}
	}

	h := sha256.New()
	writeCyberHashField(h, "sub2api.cyber-policy.transcript.v2")
	writeCyberHashField(h, strconv.FormatInt(apiKeyID, 10))
	for _, field := range []string{"instructions", "system"} {
		value := root.Get(field)
		if !value.Exists() || (value.Type == gjson.String && strings.TrimSpace(value.String()) == "") {
			continue
		}
		writeCyberHashField(h, field)
		writeCyberHashField(h, canonicalCyberTranscriptValue(value))
	}

	appendSequence := func(sequence gjson.Result) openAICyberTranscriptBlockKeys {
		if !sequence.Exists() || !sequence.IsArray() {
			return openAICyberTranscriptBlockKeys{}
		}
		all := make([]string, 0, min(len(sequence.Array()), maxOpenAICyberTranscriptLookupKeys))
		sequence.ForEach(func(_, item gjson.Result) bool {
			canonical := canonicalCyberTranscriptValue(item)
			if strings.TrimSpace(canonical) == "" {
				return true
			}
			writeCyberHashField(h, "item")
			writeCyberHashField(h, canonical)
			all = append(all, hex.EncodeToString(h.Sum(nil)))
			return true
		})
		result := openAICyberTranscriptBlockKeys{}
		if len(all) > maxOpenAICyberTranscriptLookupKeys {
			result.lookupKeysTruncated = true
			all = all[len(all)-maxOpenAICyberTranscriptLookupKeys:]
		}
		result.lookupKeys = all
		return result
	}

	if messages := root.Get("messages"); messages.Exists() {
		return appendSequence(messages)
	}
	input := root.Get("input")
	if input.IsArray() {
		return appendSequence(input)
	}
	if input.Type == gjson.String && strings.TrimSpace(input.String()) != "" {
		writeCyberHashField(h, "item")
		writeCyberHashField(h, canonicalCyberTranscriptValue(input))
		return openAICyberTranscriptBlockKeys{
			lookupKeys: []string{hex.EncodeToString(h.Sum(nil))},
		}
	}
	return openAICyberTranscriptBlockKeys{}
}

func canonicalCyberTranscriptValue(value gjson.Result) string {
	switch value.Type {
	case gjson.String:
		encoded, _ := json.Marshal(value.String())
		return string(encoded)
	case gjson.JSON:
		return normalizeCompatSeedJSON(json.RawMessage(value.Raw))
	default:
		return strings.TrimSpace(value.Raw)
	}
}

// writeCyberHashField makes every hash input unambiguous even when values
// contain separators. The decimal byte length is itself delimited by a colon.
func writeCyberHashField(h hash.Hash, value string) {
	_, _ = h.Write([]byte(strconv.Itoa(len(value))))
	_, _ = h.Write([]byte{':'})
	_, _ = h.Write([]byte(value))
}
