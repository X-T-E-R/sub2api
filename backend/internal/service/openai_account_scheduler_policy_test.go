package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestBuildOpenAIAccountLoadPlan_OpenAIStatsDoNotAffectLunaRanking(t *testing.T) {
	accounts := []*Account{
		{ID: 1, Platform: PlatformOpenAI, Priority: 0},
		{ID: 2, Platform: PlatformOpenAI, Priority: 1},
	}
	loadMap := map[int64]*AccountLoadInfo{
		1: {AccountID: 1, LoadRate: 0},
		2: {AccountID: 2, LoadRate: 0},
	}
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights = config.GatewayOpenAIWSSchedulerScoreWeights{
		Priority:  1,
		ErrorRate: 100,
		TTFT:      100,
	}

	baselineScheduler := &defaultOpenAIAccountScheduler{
		service: &OpenAIGatewayService{cfg: cfg},
		stats:   newOpenAIAccountRuntimeStats(),
	}
	observedStats := newOpenAIAccountRuntimeStats()
	badTTFT := 60_000
	for i := 0; i < 5; i++ {
		observedStats.report(1, false, &badTTFT)
	}
	observedStats.report(2, true, intPtrForTest(10))
	observedScheduler := &defaultOpenAIAccountScheduler{
		service: &OpenAIGatewayService{cfg: cfg},
		stats:   observedStats,
	}
	req := OpenAIAccountScheduleRequest{
		Platform:       PlatformOpenAI,
		RequestedModel: "gpt-5.6-luna",
	}

	baseline := baselineScheduler.buildOpenAIAccountLoadPlan(context.Background(), req, accounts, loadMap)
	observed := observedScheduler.buildOpenAIAccountLoadPlan(context.Background(), req, accounts, loadMap)
	baselineScores := openAIPlanScores(baseline)
	observedScores := openAIPlanScores(observed)

	// A bad Astra observation on account 1 must not alter Luna's OpenAI rank.
	require.Equal(t, baselineScores, observedScores)
	// Priority remains an active, non-observational ranking weight.
	require.Greater(t, observedScores[1], observedScores[2])
}

func TestBuildOpenAIAccountSchedulerScoreSnapshot_StatsWeightsOnlyIgnoredForOpenAI(t *testing.T) {
	weights := GatewayOpenAIWSSchedulerScoreWeightsView{
		Load:          2,
		ErrorRate:     3,
		TTFT:          4,
		Previous:      5,
		SessionSticky: 7,
	}
	openAIAccounts := []*Account{
		{ID: 11, Platform: PlatformOpenAI},
		{ID: 12, Platform: PlatformOpenAI},
	}
	loadMap := map[int64]*AccountLoadInfo{
		11: {AccountID: 11, LoadRate: 0},
		12: {AccountID: 12, LoadRate: 0},
	}
	openAIScores := buildOpenAIAccountSchedulerScoreSnapshot(openAIAccounts, loadMap, weights, true, defaultOpenAIOAuthSchedulingRateMultiplier)
	openAIWithoutStatsWeights := weights
	openAIWithoutStatsWeights.ErrorRate = 0
	openAIWithoutStatsWeights.TTFT = 0
	openAIScoresWithoutStatsWeights := buildOpenAIAccountSchedulerScoreSnapshot(openAIAccounts, loadMap, openAIWithoutStatsWeights, true, defaultOpenAIOAuthSchedulingRateMultiplier)

	require.Equal(t, openAIScoresWithoutStatsWeights[11].BaseScore, openAIScores[11].BaseScore)
	require.Equal(t, openAIScores[11].BaseScore+weights.Previous+weights.SessionSticky, openAIScores[11].StickyScore)
	require.False(t, openAIScores[11].StickyScoreInfinity)

	grokAccounts := []*Account{
		{ID: 21, Platform: PlatformGrok},
	}
	grokScores := buildOpenAIAccountSchedulerScoreSnapshot(grokAccounts, map[int64]*AccountLoadInfo{
		21: {AccountID: 21, LoadRate: 0},
	}, weights, false, defaultOpenAIOAuthSchedulingRateMultiplier)
	require.Equal(t, 2+3+2.0, grokScores[21].BaseScore)
}

func TestDefaultOpenAIAccountScheduler_GrokStickyEscapeStillUsesStats(t *testing.T) {
	stats := newOpenAIAccountRuntimeStats()
	for i := 0; i < 4; i++ {
		stats.report(31, false, nil)
	}
	errorRate, _, _ := stats.snapshot(31)
	require.Greater(t, errorRate, 0.5)
	scheduler := &defaultOpenAIAccountScheduler{stats: stats, service: &OpenAIGatewayService{cfg: &config.Config{}}}
	reason, _, _, shouldEscape := scheduler.shouldEscapeStickyAccount(31, scheduler.service.openAIStickyEscapeConfig())
	require.True(t, shouldEscape, "reason=%q", reason)
	require.True(t, openAIAdaptiveSchedulerStatsEnabled(PlatformGrok))
	require.False(t, openAIAdaptiveSchedulerStatsEnabled(PlatformOpenAI))
}

type schedulerPolicyErrorCache struct {
	GatewayCache
	err error
}

func (c schedulerPolicyErrorCache) GetSessionAccountID(context.Context, int64, string) (int64, error) {
	return 0, c.err
}

type schedulerPolicyAccountRepo struct {
	schedulerTestOpenAIAccountRepo
	err     error
	missing bool
}

func (r schedulerPolicyAccountRepo) GetByID(context.Context, int64) (*Account, error) {
	if r.err != nil {
		return nil, r.err
	}
	if r.missing {
		return nil, ErrAccountNotFound
	}
	return r.schedulerTestOpenAIAccountRepo.GetByID(context.Background(), 1)
}

func TestDefaultOpenAIAccountScheduler_StrictStickyAdmissionPropagatesReadErrors(t *testing.T) {
	readErr := errors.New("database unavailable")
	groupID := int64(902)
	account := Account{ID: 41, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, GroupIDs: []int64{groupID}}
	scheduler := &defaultOpenAIAccountScheduler{service: &OpenAIGatewayService{
		accountRepo: schedulerPolicyAccountRepo{
			schedulerTestOpenAIAccountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{account}},
			err:                            readErr,
		},
		cache: &schedulerPolicyErrorCache{err: readErr},
	}}

	_, _, err := scheduler.selectBySessionHash(context.Background(), OpenAIAccountScheduleRequest{
		GroupID:               &groupID,
		Platform:              PlatformOpenAI,
		StickyAccountID:       account.ID,
		SessionHash:           "strict_read_error",
		StrictSessionAffinity: true,
	})
	require.ErrorIs(t, err, readErr)
}

func TestDefaultOpenAIAccountScheduler_StrictStickyAdmissionTreatsMissingAsNoSelection(t *testing.T) {
	groupID := int64(903)
	scheduler := &defaultOpenAIAccountScheduler{service: &OpenAIGatewayService{
		accountRepo: schedulerPolicyAccountRepo{missing: true},
	}}

	selection, escaped, err := scheduler.selectBySessionHash(context.Background(), OpenAIAccountScheduleRequest{
		GroupID:               &groupID,
		Platform:              PlatformOpenAI,
		StickyAccountID:       51,
		SessionHash:           "strict_missing",
		StrictSessionAffinity: true,
	})
	require.NoError(t, err)
	require.Nil(t, selection)
	require.False(t, escaped)
}

func TestDefaultOpenAIAccountScheduler_StrictStickyAdmissionPropagatesRecheckReadErrors(t *testing.T) {
	readErr := errors.New("database unavailable during recheck")
	groupID := int64(904)
	account := &Account{ID: 61, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, GroupIDs: []int64{groupID}}
	scheduler := &defaultOpenAIAccountScheduler{service: &OpenAIGatewayService{
		accountRepo: schedulerPolicyAccountRepo{
			schedulerTestOpenAIAccountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{*account}},
			err:                            readErr,
		},
		schedulerSnapshot: &SchedulerSnapshotService{cache: &openAISnapshotCacheStub{
			accountsByID: map[int64]*Account{account.ID: account},
		}},
	}}

	_, _, err := scheduler.selectBySessionHash(context.Background(), OpenAIAccountScheduleRequest{
		GroupID:               &groupID,
		Platform:              PlatformOpenAI,
		StickyAccountID:       account.ID,
		SessionHash:           "strict_recheck_read_error",
		StrictSessionAffinity: true,
	})
	require.ErrorIs(t, err, readErr)
}
