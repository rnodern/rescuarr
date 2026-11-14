package observeservice

import (
	"context"
	"database/sql"
	"errors"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
)

// AutomaticEmptyPackCases reads recent, fully mapped Sonarr cases for a worker
// that separately qualifies the installation and revalidates the live queue.
func (s *Service) AutomaticEmptyPackCases(ctx context.Context, after domain.CaseID, limit int) ([]domain.Case, error) {
	if len(after) > 128 || limit <= 0 || limit > 25 {
		return nil, ErrInvalid
	}
	var cases []domain.Case
	now := s.clock.Now()
	err := s.store.Read(ctx, func(tx *sql.Tx) error {
		const where = ` WHERE c.id > ? AND c.state = 'held' AND c.reason = 'no_eligible_files'
			AND c.observation_only = 0 AND c.missing_complete_scans = 0 AND i.product = 'sonarr'
			AND i.version = '5.0.0.2941' AND i.health = 'available' AND i.coverage = 'known'
			AND i.generation = c.last_seen_generation AND i.epoch = c.epoch
			AND i.checked_at > ? AND i.checked_at <= ?
			ORDER BY c.id LIMIT ?`
		rows, err := tx.QueryContext(ctx, caseDisplayQuery+where, after, stamp(now.Add(-recoveryCaseAge)), stamp(now), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			item, err := scanCaseDisplay(rows)
			if err != nil {
				return err
			}
			cases = append(cases, item)
		}
		return rows.Err()
	})
	return cases, err
}

// RecoveryInstance resolves the current private connection for an operation
// whose queue item may already have disappeared.
func (s *Service) RecoveryInstance(ctx context.Context, ref domain.InstanceRef, revision domain.Revision) (domain.Instance, error) {
	instance := domain.Instance{Ref: ref, ConnectionRevision: revision}
	if !ref.Valid() || revision == 0 {
		return domain.Instance{}, app.ErrHeld
	}
	err := s.store.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT product, display_name, base_url, secret_ref, version
			FROM observe_instances WHERE id = ? AND epoch = ? AND connection_revision = ? AND health = 'available' AND coverage = 'known'`,
			ref.ID, ref.Epoch, revision).Scan(&instance.Product, &instance.DisplayName, &instance.BaseURL, &instance.SecretRef, &instance.UpstreamVersion)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Instance{}, app.ErrHeld
	}
	return instance, err
}
