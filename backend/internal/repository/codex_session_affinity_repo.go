package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

var _ service.CodexSessionAffinityRepository = (*codexDailySessionRepository)(nil)

// NewCodexSessionAffinityRepository exposes the affinity portion of the daily
// session repository without changing the existing daily-pool constructor.
func NewCodexSessionAffinityRepository(db *sql.DB) service.CodexSessionAffinityRepository {
	return &codexDailySessionRepository{db: db}
}

func (r *codexDailySessionRepository) FindSessionOwner(ctx context.Context, bindingKey string) (*service.CodexSessionAccountOwner, error) {
	owner := &service.CodexSessionAccountOwner{}
	err := r.db.QueryRowContext(ctx, `
		SELECT account_scope, account_id
		FROM codex_session_account_owners
		WHERE binding_key=$1`, bindingKey).Scan(&owner.AccountScope, &owner.AccountID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return owner, nil
}

func (r *codexDailySessionRepository) ClaimSessionOwner(ctx context.Context, bindingKey string, candidate service.CodexSessionAccountOwner) (*service.CodexSessionAccountOwner, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err = tx.ExecContext(ctx, `
		INSERT INTO codex_session_account_owners(binding_key, account_scope, account_id)
		VALUES($1, $2, $3)
		ON CONFLICT (binding_key) DO NOTHING`, bindingKey, candidate.AccountScope, candidate.AccountID); err != nil {
		return nil, err
	}

	winner := &service.CodexSessionAccountOwner{}
	if err = tx.QueryRowContext(ctx, `
		SELECT account_scope, account_id
		FROM codex_session_account_owners
		WHERE binding_key=$1`, bindingKey).Scan(&winner.AccountScope, &winner.AccountID); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return winner, nil
}

func (r *codexDailySessionRepository) FindSessionBindingScopes(ctx context.Context, bindingKey string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT DISTINCT account_scope
		FROM codex_daily_session_bindings
		WHERE binding_key=$1`, bindingKey)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	scopes := make([]string, 0)
	for rows.Next() {
		var scope string
		if err := rows.Scan(&scope); err != nil {
			return nil, err
		}
		scopes = append(scopes, scope)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return scopes, nil
}
