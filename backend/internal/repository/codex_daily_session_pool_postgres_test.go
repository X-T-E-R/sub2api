package repository

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func dailyPoolPostgresDatabase(t *testing.T) *sql.DB {
	t.Helper()
	raw := os.Getenv("CODEX_DAILY_SESSION_TEST_DSN")
	if raw == "" {
		t.Skip("CODEX_DAILY_SESSION_TEST_DSN is not set")
	}
	target, err := url.Parse(raw)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", target.Hostname())
	require.Equal(t, "/codex_daily_session_test", target.Path)
	db, err := sql.Open("postgres", raw)
	require.NoError(t, err)
	schema := "daily_pool_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = db.Exec(`CREATE SCHEMA ` + schema)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	query := target.Query()
	query.Set("search_path", schema)
	target.RawQuery = query.Encode()
	db, err = sql.Open("postgres", target.String())
	require.NoError(t, err)
	db.SetMaxOpenConns(16)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func dailyPoolPostgres(t *testing.T) (*sql.DB, service.CodexDailySessionRepository) {
	t.Helper()
	db := dailyPoolPostgresDatabase(t)
	// Only the account columns consumed by policy validation; no user DB schema.
	_, err := db.Exec(`CREATE TABLE accounts(id BIGINT PRIMARY KEY, platform TEXT, type TEXT, credentials JSONB, extra JSONB, parent_account_id BIGINT, deleted_at TIMESTAMPTZ)`)
	require.NoError(t, err)
	migration, err := migrations.FS.ReadFile("235_codex_daily_session_pool.sql")
	require.NoError(t, err)
	for range 2 {
		_, err = db.Exec(string(migration))
		require.NoError(t, err)
	}
	return db, NewCodexDailySessionRepository(db)
}

func requireUUIDV7(t *testing.T, value string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(value)
	require.NoError(t, err)
	require.Equal(t, uuid.Version(7), id.Version())
	require.Equal(t, uuid.RFC4122, id.Variant())
	return id
}

func TestCodexDailyPoolPostgresLegacyV4BindingIsPreserved(t *testing.T) {
	db, repo := dailyPoolPostgres(t)
	ctx := context.Background()
	_, err := db.Exec(`INSERT INTO accounts VALUES
 (1,'openai','oauth','{"chatgpt_account_id":"legacy","chatgpt_user_id":"user"}','{"codex_fingerprint_mode":"session","codex_daily_session_pool_enabled":true,"codex_daily_session_pool_min":1,"codex_daily_session_pool_max":1}',NULL,NULL)`)
	require.NoError(t, err)
	account := &service.Account{
		ID:          1,
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "legacy", "chatgpt_user_id": "user"},
	}
	scope := service.CodexDailySessionScope(account)
	binding := "api-key-legacy:root-legacy"
	legacy := uuid.New().String()
	require.Equal(t, uuid.Version(4), uuid.MustParse(legacy).Version())
	_, err = db.Exec(`INSERT INTO codex_daily_session_bindings(account_scope,binding_key,session_id) VALUES($1,$2,$3)`, scope, binding, legacy)
	require.NoError(t, err)

	got, err := repo.Allocate(ctx, scope, binding, "not-a-date", account.ID, 1, 1)
	require.NoError(t, err)
	require.Equal(t, legacy, got, "restoring a historical binding must not project it into UUIDv7")

	var persisted string
	require.NoError(t, db.QueryRow(`SELECT session_id FROM codex_daily_session_bindings WHERE account_scope=$1 AND binding_key=$2`, scope, binding).Scan(&persisted))
	require.Equal(t, legacy, persisted)
	var days int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM codex_daily_session_days`).Scan(&days))
	require.Zero(t, days, "an existing binding must not create a new daily allocation")
}

func TestCodexDailyPoolPostgresV7SlotsPreserveBudgetAndIsolateBindings(t *testing.T) {
	db, repo := dailyPoolPostgres(t)
	ctx := context.Background()
	_, err := db.Exec(`INSERT INTO accounts VALUES
 (1,'openai','oauth','{"chatgpt_account_id":"A","chatgpt_user_id":"user"}','{"codex_fingerprint_mode":"session","codex_daily_session_pool_enabled":true,"codex_daily_session_pool_min":2,"codex_daily_session_pool_max":2}',NULL,NULL),
 (2,'openai','oauth','{"chatgpt_account_id":"B","chatgpt_user_id":"user"}','{"codex_fingerprint_mode":"session","codex_daily_session_pool_enabled":true,"codex_daily_session_pool_min":2,"codex_daily_session_pool_max":2}',NULL,NULL)`)
	require.NoError(t, err)
	accountA := &service.Account{ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "A", "chatgpt_user_id": "user"}}
	accountB := &service.Account{ID: 2, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "B", "chatgpt_user_id": "user"}}
	scopeA := service.CodexDailySessionScope(accountA)
	scopeB := service.CodexDailySessionScope(accountB)
	day := "2026-09-10"

	first, err := repo.Allocate(ctx, scopeA, "api-key-a:root-a", day, accountA.ID, 2, 2)
	require.NoError(t, err)
	second, err := repo.Allocate(ctx, scopeA, "api-key-b:root-b", day, accountA.ID, 2, 2)
	require.NoError(t, err)
	require.NotEqual(t, first, second, "distinct API-key bindings fill distinct slots before the budget is full")
	requireUUIDV7(t, first)
	requireUUIDV7(t, second)

	var beforeBudget int
	var beforeSessions pq.StringArray
	require.NoError(t, db.QueryRow(`SELECT budget,sessions FROM codex_daily_session_days WHERE account_scope=$1 AND allocation_day=$2`, scopeA, day).Scan(&beforeBudget, &beforeSessions))
	require.Equal(t, 2, beforeBudget)
	require.Len(t, beforeSessions, 2)

	reused, err := repo.Allocate(ctx, scopeA, "api-key-c:root-c", day, accountA.ID, 2, 2)
	require.NoError(t, err)
	require.Contains(t, []string(beforeSessions), reused, "a full day's budget must reuse an existing slot")
	var afterBudget int
	var afterSessions pq.StringArray
	require.NoError(t, db.QueryRow(`SELECT budget,sessions FROM codex_daily_session_days WHERE account_scope=$1 AND allocation_day=$2`, scopeA, day).Scan(&afterBudget, &afterSessions))
	require.Equal(t, beforeBudget, afterBudget)
	require.Equal(t, []string(beforeSessions), []string(afterSessions), "same-day allocation must not reset or replace the persisted slot list")

	var apiKeyBindings int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM codex_daily_session_bindings WHERE account_scope=$1`, scopeA).Scan(&apiKeyBindings))
	require.Equal(t, 3, apiKeyBindings, "different API keys/roots need independent durable bindings")
	gotFirst, err := repo.FindBinding(ctx, scopeA, "api-key-a:root-a")
	require.NoError(t, err)
	require.Equal(t, first, gotFirst)

	otherAccount, err := repo.Allocate(ctx, scopeB, "api-key-a:root-a", day, accountB.ID, 2, 2)
	require.NoError(t, err)
	requireUUIDV7(t, otherAccount)
	require.NotContains(t, []string(afterSessions), otherAccount, "account scopes must not share daily-session slots")
	var otherAccountSlots int
	require.NoError(t, db.QueryRow(`SELECT cardinality(sessions) FROM codex_daily_session_days WHERE account_scope=$1 AND allocation_day=$2`, scopeB, day).Scan(&otherAccountSlots))
	require.Equal(t, 1, otherAccountSlots)

	nextDay := "2026-09-11"
	next, err := repo.Allocate(ctx, scopeA, "api-key-a:root-next-day", nextDay, accountA.ID, 2, 2)
	require.NoError(t, err)
	requireUUIDV7(t, next)
	require.NotContains(t, []string(afterSessions), next, "a new day must start with a new v7 slot")
	var nextDaySlots int
	require.NoError(t, db.QueryRow(`SELECT cardinality(sessions) FROM codex_daily_session_days WHERE account_scope=$1 AND allocation_day=$2`, scopeA, nextDay).Scan(&nextDaySlots))
	require.Equal(t, 1, nextDaySlots)
}

func TestCodexDailyPoolPostgresConcurrencyDurability(t *testing.T) {
	db, repo := dailyPoolPostgres(t)
	ctx := context.Background()
	_, err := db.Exec(`INSERT INTO accounts VALUES
 (1,'openai','oauth','{"chatgpt_account_id":"A","chatgpt_user_id":"user"}','{"codex_fingerprint_mode":"session","codex_daily_session_pool_enabled":true,"codex_daily_session_pool_min":5,"codex_daily_session_pool_max":5}',NULL,NULL),
 (2,'openai','oauth','{"chatgpt_account_id":"B","chatgpt_user_id":"user"}','{"codex_fingerprint_mode":"session","codex_daily_session_pool_enabled":true,"codex_daily_session_pool_min":1,"codex_daily_session_pool_max":1}',NULL,NULL)`)
	require.NoError(t, err)
	scopeA := service.CodexDailySessionScope(&service.Account{Platform: "openai", Type: "oauth", Credentials: map[string]any{"chatgpt_account_id": "A", "chatgpt_user_id": "user"}})
	scopeB := service.CodexDailySessionScope(&service.Account{Platform: "openai", Type: "oauth", Credentials: map[string]any{"chatgpt_account_id": "B", "chatgpt_user_id": "user"}})
	// Independent oracle: every first root must bind once; the union of new IDs
	// in a day cannot exceed its frozen budget, regardless of racing processes.
	run := func(roots []string, day string, min, max int) []string {
		t.Helper()
		results := make([]string, len(roots))
		errs := make([]error, len(roots))
		var wg sync.WaitGroup
		for i, root := range roots {
			wg.Go(func() { results[i], errs[i] = repo.Allocate(ctx, scopeA, root, day, 1, min, max) })
		}
		wg.Wait()
		for _, err := range errs {
			require.NoError(t, err)
		}
		return results
	}
	same := make([]string, 40)
	for i := range same {
		same[i] = "api-key-1:root-1"
	}
	values := run(same, "2026-09-05", 5, 5)
	for _, value := range values {
		require.Equal(t, values[0], value)
	}
	roots := make([]string, 80)
	for i := range roots {
		roots[i] = fmt.Sprintf("api-key-%d:root-%d", i%3, i+2)
	}
	values = append(values, run(roots, "2026-09-05", 5, 5)...)
	distinct := make(map[string]bool)
	for _, value := range values {
		requireUUIDV7(t, value)
		distinct[value] = true
	}
	require.Len(t, distinct, 5)
	var budget, slots, bindings int
	require.NoError(t, db.QueryRow(`SELECT budget,cardinality(sessions) FROM codex_daily_session_days WHERE account_scope=$1`, scopeA).Scan(&budget, &slots))
	require.Equal(t, 5, budget)
	require.Equal(t, 5, slots)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM codex_daily_session_bindings`).Scan(&bindings))
	require.Equal(t, 81, bindings)
	_, err = db.Exec(`UPDATE accounts SET extra=jsonb_set(jsonb_set(extra,'{codex_daily_session_pool_min}','1'),'{codex_daily_session_pool_max}','1') WHERE id=1`)
	require.NoError(t, err)
	changed, err := repo.Allocate(ctx, scopeA, "changed-range", "2026-09-05", 1, 1, 1)
	require.NoError(t, err)
	require.True(t, distinct[changed], "same-day config edits cannot replace the pool")
	_, err = db.Exec(`UPDATE accounts SET extra=jsonb_set(jsonb_set(extra,'{codex_daily_session_pool_min}','2'),'{codex_daily_session_pool_max}','2') WHERE id=1`)
	require.NoError(t, err)
	tomorrow := run([]string{"tomorrow-1", "tomorrow-2", "tomorrow-3"}, "2026-09-06", 2, 2)
	tomorrowDistinct := make(map[string]bool)
	for _, value := range tomorrow {
		requireUUIDV7(t, value)
		require.False(t, distinct[value])
		tomorrowDistinct[value] = true
	}
	require.Len(t, tomorrowDistinct, 2)
	resumed, err := repo.Allocate(ctx, scopeA, same[0], "2026-09-06", 1, 2, 2)
	require.NoError(t, err)
	require.Equal(t, values[0], resumed)
	other, err := repo.Allocate(ctx, scopeB, same[0], "2026-09-05", 2, 1, 1)
	require.NoError(t, err)
	require.NotEqual(t, resumed, other)
	// Fresh repository has no process state. Cleanup removes only old day metadata.
	restarted := NewCodexDailySessionRepository(db)
	count, err := restarted.CleanupDays(ctx, "2026-09-06", 1)
	require.NoError(t, err)
	require.EqualValues(t, 1, count)
	restored, err := restarted.FindBinding(ctx, scopeA, same[0])
	require.NoError(t, err)
	require.Equal(t, resumed, restored)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = restarted.Allocate(canceled, scopeA, "canceled-root", "2026-09-06", 1, 2, 2)
	require.Error(t, err)
	absent, err := restarted.FindBinding(ctx, scopeA, "canceled-root")
	require.NoError(t, err)
	require.Empty(t, absent)
	_, err = db.Exec(`UPDATE accounts SET extra=jsonb_set(extra,'{codex_daily_session_pool_enabled}','false') WHERE id=1`)
	require.NoError(t, err)
	resumed, err = restarted.Allocate(ctx, scopeA, same[0], "not-a-calendar-date", 1, 1, 1)
	require.NoError(t, err)
	require.Equal(t, restored, resumed, "old binding precedes both date and disabled policy")
	_, err = restarted.Allocate(ctx, scopeA, "stale-enabled-new-root", "2026-09-06", 1, 2, 2)
	require.ErrorContains(t, err, "policy changed or is disabled")
}

func TestCodexDailyPoolPostgresAliasConflict(t *testing.T) {
	db, repo := dailyPoolPostgres(t)
	ctx := context.Background()
	_, err := db.Exec(`INSERT INTO accounts VALUES
 (1,'openai','oauth','{"chatgpt_account_id":"same","chatgpt_user_id":"user"}','{"codex_fingerprint_mode":"session","codex_daily_session_pool_enabled":true,"codex_daily_session_pool_min":5,"codex_daily_session_pool_max":5}',NULL,NULL),
 (2,'openai','oauth','{"chatgpt_account_id":"same","chatgpt_user_id":"user"}','{"codex_fingerprint_mode":"session","codex_daily_session_pool_enabled":true,"codex_daily_session_pool_min":10,"codex_daily_session_pool_max":10}',NULL,NULL)`)
	require.NoError(t, err)
	account := &service.Account{Platform: "openai", Type: "oauth", Credentials: map[string]any{"chatgpt_account_id": "same", "chatgpt_user_id": "user"}}
	scope := service.CodexDailySessionScope(account)
	_, err = repo.Allocate(ctx, scope, "root", "2026-09-05", 1, 5, 5)
	require.ErrorContains(t, err, "identical daily session")
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM codex_daily_session_bindings`).Scan(&count))
	require.Zero(t, count)
	_, err = db.Exec(`UPDATE accounts SET extra=jsonb_set(jsonb_set(extra,'{codex_daily_session_pool_min}','5'),'{codex_daily_session_pool_max}','5') WHERE id=2`)
	require.NoError(t, err)
	_, err = repo.Allocate(ctx, scope, "root", "2026-09-05", 1, 10, 10)
	require.NoError(t, err)
	require.NoError(t, db.QueryRow(`SELECT budget FROM codex_daily_session_days`).Scan(&count))
	require.Equal(t, 5, count, "current coherent persisted policy wins over stale scheduler snapshot")
	_, err = db.Exec(`UPDATE accounts SET extra=jsonb_set(extra,'{codex_daily_session_pool_max}','10')`)
	require.NoError(t, err)
	for day := 6; day <= 17; day++ {
		date := fmt.Sprintf("2026-09-%02d", day)
		_, err = repo.Allocate(ctx, scope, "sampled-"+date, date, 1, 5, 10)
		require.NoError(t, err)
		var budget, slots int
		require.NoError(t, db.QueryRow(`SELECT budget,cardinality(sessions) FROM codex_daily_session_days WHERE allocation_day=$1`, date).Scan(&budget, &slots))
		require.GreaterOrEqual(t, budget, 5)
		require.LessOrEqual(t, budget, 10)
		require.Equal(t, 1, slots, "a minimum budget never synthesizes sessions without incoming roots")
	}
	_, err = db.Exec(`UPDATE accounts SET extra=jsonb_set(extra,'{codex_daily_session_pool_enabled}','false') WHERE id=1`)
	require.NoError(t, err)
	_, err = repo.Allocate(ctx, scope, "disabled-alias-new-root", "2026-09-18", 1, 5, 10)
	require.ErrorContains(t, err, "policy changed or is disabled", "enabled sibling cannot opt a disabled selected alias back in")
}
