package service

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
)

const (
	warmupPromptTemplateCount   = 6
	warmupMaxResponseExcerptLen = 120
)

// WarmupPrompt holds a single randomized mental-math micro-task and its deterministic answer.
type WarmupPrompt struct {
	Prompt         string
	PromptSummary  string
	ExpectedAnswer string
}

func cryptoRandIntRange(minVal, maxVal int) (int, error) {
	if maxVal < minVal {
		return 0, fmt.Errorf("invalid range [%d, %d]", minVal, maxVal)
	}
	if maxVal == minVal {
		return minVal, nil
	}
	span := big.NewInt(int64(maxVal - minVal + 1))
	n, err := rand.Int(rand.Reader, span)
	if err != nil {
		return 0, fmt.Errorf("crypto/rand int: %w", err)
	}
	return minVal + int(n.Int64()), nil
}

func generateWarmupPromptByTemplate(templateIdx int) (WarmupPrompt, error) {
	switch templateIdx % warmupPromptTemplateCount {
	case 0:
		a, err := cryptoRandIntRange(1000, 9999)
		if err != nil {
			return WarmupPrompt{}, err
		}
		b, err := cryptoRandIntRange(1000, 9999)
		if err != nil {
			return WarmupPrompt{}, err
		}
		return WarmupPrompt{
			Prompt:         fmt.Sprintf("你好。请心算 %d+%d，不使用计算器或工具，只回复结果。", a, b),
			PromptSummary:  fmt.Sprintf("add4d: %d+%d", a, b),
			ExpectedAnswer: strconv.Itoa(a + b),
		}, nil

	case 1:
		a, err := cryptoRandIntRange(5000, 9999)
		if err != nil {
			return WarmupPrompt{}, err
		}
		b, err := cryptoRandIntRange(1000, 4999)
		if err != nil {
			return WarmupPrompt{}, err
		}
		return WarmupPrompt{
			Prompt:         fmt.Sprintf("Quick mental math: compute %d - %d without tools and reply with the integer only.", a, b),
			PromptSummary:  fmt.Sprintf("sub4d: %d-%d", a, b),
			ExpectedAnswer: strconv.Itoa(a - b),
		}, nil

	case 2:
		a, err := cryptoRandIntRange(12, 49)
		if err != nil {
			return WarmupPrompt{}, err
		}
		b, err := cryptoRandIntRange(12, 49)
		if err != nil {
			return WarmupPrompt{}, err
		}
		c, err := cryptoRandIntRange(11, 99)
		if err != nil {
			return WarmupPrompt{}, err
		}
		return WarmupPrompt{
			Prompt:         fmt.Sprintf("请直接心算 %d×%d+%d 的结果，不使用任何工具，只回复最终数字。", a, b, c),
			PromptSummary:  fmt.Sprintf("muladd: %d*%d+%d", a, b, c),
			ExpectedAnswer: strconv.Itoa(a*b + c),
		}, nil

	case 3:
		n1, err := cryptoRandIntRange(11, 99)
		if err != nil {
			return WarmupPrompt{}, err
		}
		n2, err := cryptoRandIntRange(11, 99)
		if err != nil {
			return WarmupPrompt{}, err
		}
		n3, err := cryptoRandIntRange(11, 99)
		if err != nil {
			return WarmupPrompt{}, err
		}
		n4, err := cryptoRandIntRange(11, 99)
		if err != nil {
			return WarmupPrompt{}, err
		}
		return WarmupPrompt{
			Prompt:         fmt.Sprintf("Add these four numbers mentally: %d, %d, %d, %d. Reply with only the sum.", n1, n2, n3, n4),
			PromptSummary:  fmt.Sprintf("sum4: %d+%d+%d+%d", n1, n2, n3, n4),
			ExpectedAnswer: strconv.Itoa(n1 + n2 + n3 + n4),
		}, nil

	case 4:
		a, err := cryptoRandIntRange(400, 1999)
		if err != nil {
			return WarmupPrompt{}, err
		}
		b, err := cryptoRandIntRange(17, 47)
		if err != nil {
			return WarmupPrompt{}, err
		}
		return WarmupPrompt{
			Prompt:         fmt.Sprintf("Compute %d mod %d mentally and output only the remainder.", a, b),
			PromptSummary:  fmt.Sprintf("mod: %d%%%d", a, b),
			ExpectedAnswer: strconv.Itoa(a % b),
		}, nil

	default:
		s, err := cryptoRandIntRange(13, 199)
		if err != nil {
			return WarmupPrompt{}, err
		}
		d, err := cryptoRandIntRange(7, 39)
		if err != nil {
			return WarmupPrompt{}, err
		}
		return WarmupPrompt{
			Prompt:         fmt.Sprintf("等差数列 %d, %d, %d, %d 的下一项是多少？只回复数字。", s, s+d, s+2*d, s+3*d),
			PromptSummary:  fmt.Sprintf("seq: %d,+%d", s, d),
			ExpectedAnswer: strconv.Itoa(s + 4*d),
		}, nil
	}
}

func generateUniqueWarmupPrompt(seen *sync.Map) (WarmupPrompt, error) {
	for attempt := 0; attempt < 32; attempt++ {
		idx, err := cryptoRandIntRange(0, warmupPromptTemplateCount-1)
		if err != nil {
			return WarmupPrompt{}, err
		}
		prompt, err := generateWarmupPromptByTemplate(idx)
		if err != nil {
			return WarmupPrompt{}, err
		}
		if seen == nil {
			return prompt, nil
		}
		if _, loaded := seen.LoadOrStore(prompt.Prompt, struct{}{}); !loaded {
			return prompt, nil
		}
	}
	idx, err := cryptoRandIntRange(0, warmupPromptTemplateCount-1)
	if err != nil {
		return WarmupPrompt{}, err
	}
	return generateWarmupPromptByTemplate(idx)
}

func buildWarmupMetadataUserID() string {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		fallback := strings.ReplaceAll(uuid.NewString()+uuid.NewString(), "-", "")
		return "user_" + fallback + "_account__session_" + uuid.NewString()
	}
	return "user_" + hex.EncodeToString(buf[:]) + "_account__session_" + uuid.NewString()
}

func matchWarmupExpectedAnswer(responseText, expectedAnswer string) bool {
	expected := strings.TrimSpace(expectedAnswer)
	if expected == "" {
		return false
	}
	if strings.Contains(responseText, expected) {
		return true
	}
	cleaned := strings.ReplaceAll(responseText, ",", "")
	cleaned = strings.ReplaceAll(cleaned, "，", "")
	return strings.Contains(cleaned, expected)
}

func formatWarmupResponseExcerpt(text string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	normalized := strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
	runes := []rune(normalized)
	if len(runes) <= maxRunes {
		return normalized
	}
	if maxRunes <= 3 {
		return string(runes[:maxRunes])
	}
	return string(runes[:maxRunes-3]) + "..."
}
