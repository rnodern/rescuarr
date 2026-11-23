package observeservice

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/arrwire"
	"github.com/rnodern/rescuarr/internal/domain"
)

// AutomaticEmptyPackCases reads recent, fully mapped Sonarr cases for a worker
// that separately qualifies the installation and revalidates the live queue.
func (s *Service) AutomaticEmptyPackCases(ctx context.Context, after domain.CaseID, limit int) ([]domain.Case, error) {
	return s.AutomaticEmptyPackCasesFor(ctx, "", after, limit)
}

func (s *Service) AutomaticEmptyPackCasesFor(ctx context.Context, id domain.InstanceID, after domain.CaseID, limit int) ([]domain.Case, error) {
	if len(id) > 128 || len(after) > 128 || limit <= 0 || limit > 25 {
		return nil, ErrInvalid
	}
	var cases []domain.Case
	now := s.clock.Now()
	err := s.store.Read(ctx, func(tx *sql.Tx) error {
		instances, err := contractInstances(ctx, tx, domain.ProductSonarr, arrwire.ContractSonarrV5)
		if err != nil || len(instances) == 0 {
			return err
		}
		where := ` WHERE c.id > ? AND (? = '' OR i.id = ?) AND c.state = 'held' AND c.reason = 'no_eligible_files'
			AND c.observation_only = 0 AND c.missing_complete_scans = 0 AND i.product = 'sonarr'
			AND i.id IN (` + placeholders(len(instances)) + `) AND i.health = 'available' AND i.coverage = 'known'
			AND i.generation = c.last_seen_generation AND i.epoch = c.epoch
			AND i.checked_at > ? AND i.checked_at <= ?
			ORDER BY c.id LIMIT ?`
		args := append([]any{after, id, id}, instances...)
		args = append(args, stamp(now.Add(-recoveryCaseAge)), stamp(now), limit)
		rows, err := tx.QueryContext(ctx, caseDisplayQuery+where, args...)
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
	if !ref.Valid() || revision == 0 {
		return domain.Instance{}, app.ErrHeld
	}
	instance, err := s.CurrentRecoveryInstance(ctx, ref.ID)
	if err == nil && (instance.Ref != ref || instance.ConnectionRevision != revision) {
		return domain.Instance{}, app.ErrHeld
	}
	return instance, err
}

// CurrentRecoveryInstance resolves the selected connection without exposing
// its private credentials to the browser.
func (s *Service) CurrentRecoveryInstance(ctx context.Context, id domain.InstanceID) (domain.Instance, error) {
	if id == "" || len(id) > 128 {
		return domain.Instance{}, app.ErrHeld
	}
	instance := domain.Instance{Ref: domain.InstanceRef{ID: id}}
	err := s.store.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT epoch, connection_revision, product, display_name, base_url, secret_ref, version
			FROM observe_instances WHERE id = ? AND health = 'available' AND coverage = 'known'`,
			id).Scan(&instance.Ref.Epoch, &instance.ConnectionRevision, &instance.Product, &instance.DisplayName, &instance.BaseURL,
			&instance.SecretRef, &instance.UpstreamVersion)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Instance{}, app.ErrHeld
	}
	return instance, err
}

// contractInstances lists the connections whose recorded build belongs to a
// contract. Case queries filter on these IDs in SQL so that paging stays exact.
func contractInstances(ctx context.Context, tx *sql.Tx, product domain.Product, contract arrwire.Contract) ([]any, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, version FROM observe_instances WHERE product = ? ORDER BY id`, product)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []any
	for rows.Next() {
		var id, version string
		if err := rows.Scan(&id, &version); err != nil {
			return nil, err
		}
		if arrwire.ContractFor(product, version) == contract {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}
