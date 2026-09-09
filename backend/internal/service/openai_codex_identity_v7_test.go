package service

import (
	"encoding/binary"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestScopedCodexUUIDV7PreservesLegacyAndTime(t *testing.T) {
	const cutover int64 = 1800000000000
	makeID := func(ms int64) string {
		id := uuid.MustParse("00000000-0000-7abc-8def-012345678901")
		var timestamp [8]byte
		binary.BigEndian.PutUint64(timestamp[:], uint64(ms)<<16)
		copy(id[:6], timestamp[:6])
		return id.String()
	}
	for _, raw := range []string{makeID(cutover - 1), "conversation-without-time", "8ab0330d-1699-445d-b891-ab945b889eb9"} {
		require.Equal(t, deriveStableUUIDv4("same-seed"), deriveScopedCodexUUID("same-seed", "session", raw, cutover))
	}
	raw := makeID(cutover)
	mapped := deriveScopedCodexUUID("scope-a", "session", raw, cutover)
	id := uuid.MustParse(mapped)
	require.Equal(t, uuid.Version(7), id.Version())
	require.Equal(t, uuid.RFC4122, id.Variant())
	require.Equal(t, cutover, int64(binary.BigEndian.Uint64(id[:8])>>16))
	require.NotEqual(t, raw, mapped)
	require.Equal(t, mapped, deriveScopedCodexUUID("scope-a", "session", raw, cutover), "retry/restart must not randomize identity")
	require.NotEqual(t, mapped, deriveScopedCodexUUID("scope-b", "session", raw, cutover), "credential/API-key scopes stay isolated")
	require.Equal(t, deriveStableUUIDv4("scope-a"), deriveScopedCodexUUID("scope-a", "installation", raw, cutover))
	require.Equal(t, deriveStableUUIDv4("scope-a"), deriveScopedCodexUUID("scope-a", "session", raw, 0))
}
