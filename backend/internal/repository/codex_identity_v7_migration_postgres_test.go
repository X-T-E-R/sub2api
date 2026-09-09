package repository

import (
	"strconv"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestCodexIdentityV7CutoverMigrationIsIdempotent(t *testing.T) {
	db := dailyPoolPostgresDatabase(t)
	_, err := db.Exec(`CREATE TABLE settings (
        id BIGSERIAL PRIMARY KEY,
        key VARCHAR(100) NOT NULL UNIQUE,
        value TEXT NOT NULL,
        updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
    )`)
	require.NoError(t, err)
	migration, err := migrations.FS.ReadFile("236_codex_identity_v7_cutover.sql")
	require.NoError(t, err)

	_, err = db.Exec(string(migration))
	require.NoError(t, err)
	const key = "codex_identity_v7_cutover_unix_ms"
	var firstValue string
	var firstUpdatedAt time.Time
	require.NoError(t, db.QueryRow(`SELECT value,updated_at FROM settings WHERE key=$1`, key).Scan(&firstValue, &firstUpdatedAt))
	cutover, err := strconv.ParseInt(firstValue, 10, 64)
	require.NoError(t, err)
	require.Positive(t, cutover)

	// Separate statements get separate clock values; a faulty upsert would be
	// observable after this pause even on coarse-grained test environments.
	time.Sleep(10 * time.Millisecond)
	_, err = db.Exec(string(migration))
	require.NoError(t, err)
	var secondValue string
	var secondUpdatedAt time.Time
	require.NoError(t, db.QueryRow(`SELECT value,updated_at FROM settings WHERE key=$1`, key).Scan(&secondValue, &secondUpdatedAt))
	require.Equal(t, firstValue, secondValue)
	require.True(t, firstUpdatedAt.Equal(secondUpdatedAt), "ON CONFLICT DO NOTHING must preserve the original timestamp")
	var rows int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM settings WHERE key=$1`, key).Scan(&rows))
	require.Equal(t, 1, rows)
}
