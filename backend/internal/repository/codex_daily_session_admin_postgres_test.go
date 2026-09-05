package repository

import (
	"context"
	"encoding/json"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestCodexDailyPoolPostgresAdminInactiveAliasPolicy(t *testing.T) {
	db := dailyPoolPostgresDatabase(t)
	poolRepo := NewCodexDailySessionRepository(db)
	ctx := context.Background()
	// Exercise the actual Ent selectors and account repository, including its
	// ACTIVE-only ListByPlatform contract and soft-delete interceptor.
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	require.NoError(t, client.Schema.Create(ctx))
	for _, name := range []string{"036_scheduler_outbox.sql", "152_scheduler_outbox_dedup_key.sql", "153_scheduler_outbox_pending_dedup_key_index_notx.sql", "235_codex_daily_session_pool.sql"} {
		body, err := migrations.FS.ReadFile(name)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, string(body))
		require.NoError(t, err)
	}
	credentials := map[string]any{"chatgpt_account_id": "same-account", "chatgpt_user_id": "same-user"}
	extra := map[string]any{"codex_fingerprint_mode": "session", service.CodexDailySessionEnabledKey: true, service.CodexDailySessionMinKey: 5, service.CodexDailySessionMaxKey: 5}
	credentialsJSON, err := json.Marshal(credentials)
	require.NoError(t, err)
	extraJSON, err := json.Marshal(extra)
	require.NoError(t, err)
	for _, id := range []int64{1, 2} {
		_, err := db.Exec(`INSERT INTO accounts(id,name,platform,type,status,credentials,extra,created_at,updated_at) VALUES($1,'daily-alias','openai','oauth','active',$2,$3,NOW(),NOW())`, id, string(credentialsJSON), string(extraJSON))
		require.NoError(t, err)
	}
	accountRepo := NewAdminAccountRepository(client, db, nil)
	admin := service.NewAdminService(nil, nil, accountRepo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	_, err = client.Account.UpdateOneID(2).SetStatus(service.StatusDisabled).Save(ctx)
	require.NoError(t, err)
	active, err := accountRepo.ListByPlatform(ctx, "openai")
	require.NoError(t, err)
	require.Len(t, active, 1, "faithful production ACTIVE-only predicate")
	interval := func(n int) map[string]any {
		return map[string]any{service.CodexDailySessionMinKey: n, service.CodexDailySessionMaxKey: n}
	}
	err = admin.UpdateAccountExtra(ctx, 1, interval(10))
	if err == nil {
		saved, readErr := accountRepo.GetByID(ctx, 1)
		require.NoError(t, readErr)
		_, allocationErr := poolRepo.Allocate(ctx, service.CodexDailySessionScope(saved), "conflicting-admin-save", "2026-09-05", 1, 10, 10)
		t.Logf("management accepted A=10/B=5 after B disabled; allocation error: %v", allocationErr)
	}
	require.ErrorContains(t, err, "identical daily session", "inactive opted-in alias still owns shared account policy")
	a, err := accountRepo.GetByID(ctx, 1)
	require.NoError(t, err)
	_, min, _, err := service.CodexDailySessionPolicy(a)
	require.NoError(t, err)
	require.Equal(t, 5, min, "conflict rejected before persistence")
	scope := service.CodexDailySessionScope(a)
	_, err = poolRepo.Allocate(ctx, scope, "root-before-bulk", "2026-09-05", 1, 5, 5)
	require.NoError(t, err)
	result, err := admin.BulkUpdateAccounts(ctx, &service.BulkUpdateAccountsInput{AccountIDs: []int64{1, 2}, Extra: interval(10)})
	require.NoError(t, err)
	require.Equal(t, 2, result.Success)
	_, err = poolRepo.Allocate(ctx, scope, "root-after-bulk", "2026-09-06", 1, 10, 10)
	require.NoError(t, err)
	var budget int
	require.NoError(t, db.QueryRow(`SELECT budget FROM codex_daily_session_days WHERE allocation_day='2026-09-06'`).Scan(&budget))
	require.Equal(t, 10, budget)
	_, err = client.Account.UpdateOneID(2).SetStatus(service.StatusError).Save(ctx)
	require.NoError(t, err)
	err = admin.UpdateAccountExtra(ctx, 1, interval(20))
	require.ErrorContains(t, err, "identical daily session")
	_, err = db.Exec(`UPDATE accounts SET deleted_at=NOW() WHERE id=2`)
	require.NoError(t, err)
	require.NoError(t, admin.UpdateAccountExtra(ctx, 1, interval(20)), "soft-deleted alias is excluded by both management and allocation")
	_, err = poolRepo.Allocate(ctx, scope, "root-after-delete", "2026-09-07", 1, 20, 20)
	require.NoError(t, err)
}
