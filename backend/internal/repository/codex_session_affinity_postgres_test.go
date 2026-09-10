package repository

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func codexSessionAffinityPostgres(t *testing.T) (*sql.DB, service.CodexSessionAffinityRepository) {
	t.Helper()
	db := dailyPoolPostgresDatabase(t)
	for _, name := range []string{"235_codex_daily_session_pool.sql", "237_codex_session_account_affinity.sql"} {
		migration, err := migrations.FS.ReadFile(name)
		require.NoError(t, err)
		for range 2 {
			_, err = db.Exec(string(migration))
			require.NoError(t, err)
		}
	}
	return db, NewCodexSessionAffinityRepository(db)
}

func TestCodexSessionAffinityPostgresConcurrentClaimsKeepOneImmutableWinner(t *testing.T) {
	db, repo := codexSessionAffinityPostgres(t)
	ctx := context.Background()
	const bindingKey = "root-binding"
	const claims = 32

	start := make(chan struct{})
	results := make([]*service.CodexSessionAccountOwner, claims)
	errs := make([]error, claims)
	var wg sync.WaitGroup
	for i := 0; i < claims; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = repo.ClaimSessionOwner(ctx, bindingKey, service.CodexSessionAccountOwner{
				AccountScope: fmt.Sprintf("scope-%02d", i),
				AccountID:    int64(i + 1),
			})
		}()
	}
	close(start)
	wg.Wait()

	for _, err := range errs {
		require.NoError(t, err)
	}
	require.NotNil(t, results[0])
	for _, got := range results[1:] {
		require.Equal(t, results[0], got)
	}

	var count int
	var persisted service.CodexSessionAccountOwner
	require.NoError(t, db.QueryRow(`
		SELECT count(*), min(account_scope), min(account_id)
		FROM codex_session_account_owners
		WHERE binding_key=$1`, bindingKey).Scan(&count, &persisted.AccountScope, &persisted.AccountID))
	require.Equal(t, 1, count)
	require.Equal(t, results[0], &persisted)
}

func TestCodexSessionAffinityPostgresLookupRestoresAcrossRepositoryRestart(t *testing.T) {
	db, repo := codexSessionAffinityPostgres(t)
	ctx := context.Background()
	const bindingKey = "restart-binding"
	wanted := service.CodexSessionAccountOwner{AccountScope: "scope-a", AccountID: 17}

	claimed, err := repo.ClaimSessionOwner(ctx, bindingKey, wanted)
	require.NoError(t, err)
	require.Equal(t, &wanted, claimed)

	restarted := NewCodexSessionAffinityRepository(db)
	found, err := restarted.FindSessionOwner(ctx, bindingKey)
	require.NoError(t, err)
	require.Equal(t, claimed, found)
	missing, err := restarted.FindSessionOwner(ctx, "missing-binding")
	require.NoError(t, err)
	require.Nil(t, missing)

	other, err := restarted.ClaimSessionOwner(ctx, bindingKey, service.CodexSessionAccountOwner{AccountScope: "scope-b", AccountID: 18})
	require.NoError(t, err)
	require.Equal(t, claimed, other, "an existing owner cannot be rebound")
	independent := service.CodexSessionAccountOwner{AccountScope: "scope-b", AccountID: 18}
	otherBinding, err := restarted.ClaimSessionOwner(ctx, "other-binding", independent)
	require.NoError(t, err)
	require.Equal(t, &independent, otherBinding, "a different root binding has an independent owner")
	found, err = restarted.FindSessionOwner(ctx, bindingKey)
	require.NoError(t, err)
	require.Equal(t, claimed, found, "claiming another binding must not alter the original owner")
}

func TestCodexSessionAffinityPostgresBindingIsolationAndHistoricalScopes(t *testing.T) {
	db, repo := codexSessionAffinityPostgres(t)
	ctx := context.Background()

	_, err := db.Exec(`INSERT INTO codex_daily_session_bindings(account_scope,binding_key,session_id) VALUES
		('scope-a','historical-single','session-a'),
		('scope-a','historical-multiple','session-a'),
		('scope-b','historical-multiple','session-b'),
		('scope-c','historical-multiple','session-c'),
		('scope-a','other-binding','session-a')`)
	require.NoError(t, err)

	single, err := repo.FindSessionBindingScopes(ctx, "historical-single")
	require.NoError(t, err)
	require.Equal(t, []string{"scope-a"}, single)
	multiple, err := repo.FindSessionBindingScopes(ctx, "historical-multiple")
	require.NoError(t, err)
	require.Len(t, multiple, 3)
	require.ElementsMatch(t, []string{"scope-a", "scope-b", "scope-c"}, multiple)
	missing, err := repo.FindSessionBindingScopes(ctx, "missing-binding")
	require.NoError(t, err)
	require.Empty(t, missing)
	isolated, err := repo.FindSessionBindingScopes(ctx, "other-binding")
	require.NoError(t, err)
	require.Equal(t, []string{"scope-a"}, isolated)
}

func TestCodexSessionAffinityPostgresClaimCancellationRollsBack(t *testing.T) {
	db, repo := codexSessionAffinityPostgres(t)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := repo.ClaimSessionOwner(canceled, "canceled-binding", service.CodexSessionAccountOwner{AccountScope: "scope", AccountID: 1})
	require.ErrorIs(t, err, context.Canceled)
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM codex_session_account_owners WHERE binding_key='canceled-binding'`).Scan(&count))
	require.Zero(t, count)
	owner, err := repo.FindSessionOwner(context.Background(), "canceled-binding")
	require.NoError(t, err)
	require.Nil(t, owner)
}
