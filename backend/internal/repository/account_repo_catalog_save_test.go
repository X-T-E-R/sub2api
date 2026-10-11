package repository

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func catalogLockedRows(current any, identityUnchanged bool) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"identity_unchanged", "ollama_group_unchanged", "ollama_proxy_unchanged", "enabled", "rate_sync_enabled", "snapshot", "ollama_session", "ollama_auto", "ollama_snapshot", "model_metadata"}).AddRow(identityUnchanged, false, true, nil, nil, nil, nil, nil, nil, current)
}

// Deterministic adversarial interleaving: the loaded edit predates a catalog
// refresh, while the row-lock SELECT returns the newer committed DB evidence.
func TestLockedCatalogMergeUsesLatestDatabaseAuthority(t *testing.T) {
	newest := []byte(`{"source":"upstream","live":true,"identity":"original-project","model_ids":["bespoke-low","bespoke-high"],"models":{}}`)
	stale := map[string]any{"live": true, "model_ids": []string{"bespoke-low", "bespoke-high", "bespoke-max"}}
	for _, tc := range []struct {
		name              string
		input             map[string]any
		db                any
		identityUnchanged bool
		wantCatalog       bool
	}{
		{"first refresh after GetByID", map[string]any{}, newest, true, true},
		{"resync after GetByID", map[string]any{service.UpstreamModelMetadataExtraKey: stale}, newest, true, true},
		{"explicit null cannot clear", map[string]any{service.UpstreamModelMetadataExtraKey: nil}, newest, true, true},
		{"token rotation not probe equality", map[string]any{}, newest, false, true},
		{"no DB evidence cannot be forged", map[string]any{service.UpstreamModelMetadataExtraKey: stale}, nil, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			t.Cleanup(func() { _ = client.Close() })
			// Column and lock must be in one query; a separate unlocked GetByID is not sufficient.
			mock.ExpectQuery(`(?s)SELECT.*`+regexp.QuoteMeta("extra -> 'upstream_model_metadata'")+`.*FOR NO KEY UPDATE`).WithArgs(int64(69), service.PlatformAntigravity, service.AccountTypeOAuth, `{"access_token":"rotated"}`, nil).WillReturnRows(catalogLockedRows(tc.db, tc.identityUnchanged))
			account := &service.Account{ID: 69, Platform: service.PlatformAntigravity, Type: service.AccountTypeOAuth, Credentials: map[string]any{"access_token": "rotated"}, Extra: tc.input}
			got, err := lockAndMergeAccountProbeExtra(context.Background(), client, account, nil, nil)
			require.NoError(t, err)
			if tc.wantCatalog {
				var expected any
				require.NoError(t, json.Unmarshal(newest, &expected))
				require.Equal(t, expected, got[service.UpstreamModelMetadataExtraKey])
			} else {
				require.NotContains(t, got, service.UpstreamModelMetadataExtraKey)
			}
			require.NotContains(t, got, "quota_daily_limit")
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestRepositorySaveWritesLockedLatestCatalog(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })
	latest := `{"live":true,"model_ids":["bespoke-low","bespoke-high"],"models":{},"identity":"scope"}`
	var decoded any
	require.NoError(t, json.Unmarshal([]byte(latest), &decoded))
	account := &service.Account{ID: 69, Name: "catalog", Platform: service.PlatformAntigravity, Type: service.AccountTypeOAuth, Credentials: map[string]any{"access_token": "rotated"}, Extra: map[string]any{service.UpstreamModelMetadataExtraKey: map[string]any{"live": true, "model_ids": []string{"bespoke-max"}}}, Concurrency: 1, Status: service.StatusActive, Schedulable: true}
	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT.*`+regexp.QuoteMeta("extra -> 'upstream_model_metadata'")+`.*FOR NO KEY UPDATE`).WithArgs(int64(69), service.PlatformAntigravity, service.AccountTypeOAuth, `{"access_token":"rotated"}`, nil).WillReturnRows(catalogLockedRows([]byte(latest), false))
	mock.ExpectExec(`(?s)UPDATE .*accounts.*SET.*extra.*WHERE .*id.*`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`(?s)SELECT .* FROM "accounts" WHERE "id" = \$1`).WithArgs(int64(69)).WillReturnRows(updatedAccountRows(69, `{"upstream_model_metadata":`+latest+`}`))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	repo := newAccountRepositoryWithSQL(client, db, nil)
	require.NoError(t, repo.UpdateWithAccountBillingSettings(context.Background(), account, nil, nil, nil))
	require.Equal(t, decoded, account.Extra[service.UpstreamModelMetadataExtraKey])
	require.NoError(t, mock.ExpectationsWereMet())
}
