package service

import (
	"context"
	"encoding/binary"
	"fmt"
	"strconv"
	"sync/atomic"

	"github.com/google/uuid"
)

const codexIdentityV7CutoverSetting = "codex_identity_v7_cutover_unix_ms"

// Loaded once after migrations, before serving requests. No identity lookup is
// added to the forwarding path, and restarts do not move the cutover.
var codexIdentityV7Cutover atomic.Int64

func (s *SettingService) loadCodexIdentityV7Cutover(ctx context.Context) error {
	raw, err := s.settingRepo.GetValue(ctx, codexIdentityV7CutoverSetting)
	if err != nil {
		return err
	}
	cutover, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || cutover <= 0 {
		return fmt.Errorf("invalid Codex identity v7 cutover setting")
	}
	codexIdentityV7Cutover.Store(cutover)
	return nil
}

func deriveScopedCodexUUID(seed, kind, raw string, cutover int64) string {
	legacy := deriveStableUUIDv4(seed)
	if cutover <= 0 || kind == "installation" {
		return legacy
	}
	original, err := uuid.Parse(raw)
	if err != nil || original.Version() != 7 || original.Variant() != uuid.RFC4122 {
		return legacy
	}
	created := int64(binary.BigEndian.Uint64(original[:8]) >> 16)
	if created < cutover {
		return legacy
	}
	// Preserve native millisecond time, but derive the random portion from the
	// same API-key/account/kind namespace as before. Repeated references and
	// parent/root links therefore stay equal without a per-request registry.
	projected := uuid.MustParse(legacy)
	copy(projected[:6], original[:6])
	projected[6] = projected[6]&0x0f | 0x70
	return projected.String()
}
