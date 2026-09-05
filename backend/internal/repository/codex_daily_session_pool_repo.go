package repository

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

type codexDailySessionRepository struct{ db *sql.DB }

func NewCodexDailySessionRepository(db *sql.DB) service.CodexDailySessionRepository {
	return &codexDailySessionRepository{db: db}
}

func (r *codexDailySessionRepository) FindBinding(ctx context.Context, scope, binding string) (string, error) {
	var session string
	err := r.db.QueryRowContext(ctx, `SELECT session_id FROM codex_daily_session_bindings WHERE account_scope=$1 AND binding_key=$2`, scope, binding).Scan(&session)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return session, err
}

func (r *codexDailySessionRepository) Allocate(ctx context.Context, scope, binding, day string, ownerID int64, min, max int) (string, error) {
	if min < 1 || max < min || max > 1000 {
		return "", fmt.Errorf("invalid Codex daily session range")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	// Serialize only first bindings per real account, including races across midnight.
	// The transaction lock is released on cancellation, rollback and commit.
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, scope); err != nil {
		return "", err
	}
	var session string
	err = tx.QueryRowContext(ctx, `SELECT session_id FROM codex_daily_session_bindings WHERE account_scope=$1 AND binding_key=$2`, scope, binding).Scan(&session)
	if err == nil {
		return session, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	// Management preflight gives actionable errors; this transaction guard also
	// catches simultaneous alias edits and stale scheduler snapshots.
	owner := &service.Account{ID: ownerID, Platform: service.PlatformOpenAI}
	var ownerCredentials, ownerExtra []byte
	if err = tx.QueryRowContext(ctx, `SELECT type,credentials,extra FROM accounts WHERE id=$1 AND platform='openai' AND deleted_at IS NULL AND parent_account_id IS NULL`, ownerID).Scan(&owner.Type, &ownerCredentials, &ownerExtra); err != nil {
		return "", err
	}
	if err = json.Unmarshal(ownerCredentials, &owner.Credentials); err != nil {
		return "", err
	}
	if err = json.Unmarshal(ownerExtra, &owner.Extra); err != nil {
		return "", err
	}
	enabled, _, _, err := service.CodexDailySessionPolicy(owner)
	if err != nil {
		return "", err
	}
	if !enabled || service.CodexDailySessionScope(owner) != scope {
		return "", fmt.Errorf("codex daily session policy changed or is disabled; refresh the selected account")
	}
	// Read only matching credential aliases, not every opted-in account's JSON.
	query := `SELECT id,type,credentials,extra FROM accounts WHERE platform='openai' AND deleted_at IS NULL AND parent_account_id IS NULL AND extra->>'codex_daily_session_pool_enabled'='true' AND `
	var args []any
	if accountID := strings.TrimSpace(owner.GetChatGPTAccountID()); accountID != "" {
		query += `btrim(credentials->>'chatgpt_account_id')=$1 AND btrim(credentials->>'chatgpt_user_id')=$2`
		args = []any{accountID, strings.TrimSpace(owner.GetCredential("chatgpt_user_id"))}
	} else {
		query += `type='setup-token' AND COALESCE(btrim(credentials->>'chatgpt_account_id'),'')='' AND btrim(credentials->>'access_token')=$1`
		args = []any{strings.TrimSpace(owner.GetOpenAIAccessToken())}
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return "", err
	}
	var aliases []*service.Account
	for rows.Next() {
		account := &service.Account{Platform: service.PlatformOpenAI}
		var credentials, extra []byte
		if err = rows.Scan(&account.ID, &account.Type, &credentials, &extra); err != nil {
			_ = rows.Close()
			return "", err
		}
		if err = json.Unmarshal(credentials, &account.Credentials); err != nil {
			_ = rows.Close()
			return "", err
		}
		if err = json.Unmarshal(extra, &account.Extra); err != nil {
			_ = rows.Close()
			return "", err
		}
		if service.CodexDailySessionScope(account) == scope {
			aliases = append(aliases, account)
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return "", err
	}
	if err = service.ValidateCodexDailySessionAliases(aliases); err != nil {
		return "", err
	}
	if len(aliases) == 0 {
		return "", fmt.Errorf("codex daily session policy changed or is disabled; refresh the selected account")
	}
	_, min, max, err = service.CodexDailySessionPolicy(aliases[0])
	if err != nil {
		return "", err
	}
	sampled, err := rand.Int(rand.Reader, big.NewInt(int64(max-min+1)))
	if err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO codex_daily_session_days(account_scope,allocation_day,budget) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, scope, day, min+int(sampled.Int64())); err != nil {
		return "", err
	}
	var budget int
	var sessions []string
	if err = tx.QueryRowContext(ctx, `SELECT budget,sessions FROM codex_daily_session_days WHERE account_scope=$1 AND allocation_day=$2 FOR UPDATE`, scope, day).Scan(&budget, pq.Array(&sessions)); err != nil {
		return "", err
	}
	if len(sessions) < budget {
		session = uuid.NewString()
		sessions = append(sessions, session)
		if _, err = tx.ExecContext(ctx, `UPDATE codex_daily_session_days SET sessions=$3 WHERE account_scope=$1 AND allocation_day=$2`, scope, day, pq.Array(sessions)); err != nil {
			return "", err
		}
	} else {
		slot, sampleErr := rand.Int(rand.Reader, big.NewInt(int64(len(sessions))))
		if sampleErr != nil {
			return "", sampleErr
		}
		session = sessions[slot.Int64()]
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO codex_daily_session_bindings(account_scope,binding_key,session_id) VALUES($1,$2,$3)`, scope, binding, session); err != nil {
		return "", err
	}
	return session, tx.Commit()
}

func (r *codexDailySessionRepository) CleanupDays(ctx context.Context, before string, limit int) (int64, error) {
	if limit < 1 || limit > 1000 {
		return 0, fmt.Errorf("invalid Codex daily cleanup batch")
	}
	result, err := r.db.ExecContext(ctx, `DELETE FROM codex_daily_session_days WHERE (account_scope,allocation_day) IN (SELECT account_scope,allocation_day FROM codex_daily_session_days WHERE allocation_day < $1 ORDER BY allocation_day LIMIT $2 FOR UPDATE SKIP LOCKED)`, before, limit)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
