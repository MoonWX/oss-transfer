package legacyimport

import (
	"context"
	"errors"

	"github.com/MoonWX/oss-transfer/internal/state"
)

// RuntimeProfile identifies the native execution defaults documented alongside
// this importer. It does not replace preserved node or task rate limits.
const RuntimeProfile = "go-runtime-defaults-1"

type RuntimeAcceptance struct {
	MigrationID string `json:"migrationId"`
	Profile     string `json:"profile"`
}

func (p *Prepared) AcceptRuntimeDefaults(ctx context.Context) error {
	if p.closed {
		return errors.New("migration preparation is closed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.Master.Update(func(tx *state.Tx) error {
		return tx.Put("migration-sections", "runtime-acceptance", RuntimeAcceptance{MigrationID: p.Manifest.MigrationID, Profile: RuntimeProfile})
	})
}
