//go:build unit

package antigravity

import (
	"encoding/json"
	"testing"
)

func TestQuotaProjectRepresentationsPreserveSubscription(t *testing.T) {
	for _, tc := range []struct{ project, want string }{
		{`"project-a"`, "project-a"},
		{`{"id":"project-b","name":"display"}`, "project-b"},
		{`null`, ""},
		{`{}`, ""},
		{`{"id":42}`, ""},
	} {
		t.Run(tc.project, func(t *testing.T) {
			var result LoadCodeAssistResponse
			err := json.Unmarshal([]byte(`{"cloudaicompanionProject":`+tc.project+`,"paidTier":{"id":"g1-pro-tier","availableCredits":[{"creditType":"PROMPT","creditAmount":"0"}]}}`), &result)
			if err != nil || result.CloudAICompanionProject != tc.want || result.GetTier() != "g1-pro-tier" || len(result.GetAvailableCredits()) != 1 {
				t.Fatalf("optional project lost subscription: %+v, %v", result, err)
			}
		})
	}
}
