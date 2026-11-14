package observeservice

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/store"
)

var ErrCredentialCleanup = errors.New("connection removed but private credential cleanup failed")

func connectionSecret(id domain.InstanceID) string { return "arr-" + string(id) + ".key" }

// RemoveInstance retires the local connection while retaining its case IDs and
// activity. It performs no Arr requests.
func (s *Service) RemoveInstance(ctx context.Context, id domain.InstanceID, actor domain.ActorID) error {
	if id == "" || len(id) > 128 {
		return app.ErrNotFound
	}
	if actor == "" {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active[id] != nil || s.recoveryReads[id] != 0 {
		return app.ErrConflict
	}
	ref := domain.InstanceRef{ID: id}
	var secret string
	var cleanSecret bool
	now := s.clock.Now()
	err := s.store.Write(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT epoch, secret_ref FROM observe_instances WHERE id = ? AND health <> ?`,
			id, removedConnection).Scan(&ref.Epoch, &secret)
		if errors.Is(err, sql.ErrNoRows) {
			return app.ErrNotFound
		}
		if err != nil {
			return err
		}
		var activeIntent bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM action_intents
			WHERE state NOT IN ('completed', 'cancelled') AND json_extract(proposal, '$.Operation.Job.Instance.ID') = ?)`,
			id).Scan(&activeIntent); err != nil {
			return err
		}
		if activeIntent {
			return app.ErrConflict
		}
		if secret == connectionSecret(id) {
			if err := tx.QueryRowContext(ctx, `SELECT NOT EXISTS (SELECT 1 FROM observe_instances WHERE id <> ? AND secret_ref = ?)`,
				id, secret).Scan(&cleanSecret); err != nil {
				return err
			}
		}
		// The retired row keeps history readable and frees the unique active URL.
		if _, err := tx.ExecContext(ctx, `UPDATE observe_instances SET health = ?, coverage = 'unknown', secret_ref = '',
			base_url = 'removed:' || id, connection_revision = connection_revision + 1 WHERE id = ?`, removedConnection, id); err != nil {
			return err
		}
		if err := closeRemovedConnectionCases(ctx, tx, ref, actor, now); err != nil {
			return err
		}
		if err := store.AppendEvent(ctx, tx, domain.Event{ID: domain.EventID(store.NewID()), Kind: "observe_instance_removed",
			Actor: actor, Scope: domain.Scope{Kind: domain.ScopeInstance, Instance: ref}, OccurredAt: now}); err != nil {
			return err
		}
		return bump(ctx, tx)
	})
	if err == nil && cleanSecret {
		if cleanupErr := s.store.RemoveSecret(secret); cleanupErr != nil {
			return ErrCredentialCleanup
		}
	}
	return err
}

func closeRemovedConnectionCases(ctx context.Context, tx *sql.Tx, ref domain.InstanceRef, actor domain.ActorID, now time.Time) error {
	const reason = "connection_removed"
	rows, err := tx.QueryContext(ctx, `UPDATE observe_cases SET state = 'closed', reason = ?, revision = revision + 1, updated_at = ?,
		payload = json_set(json_remove(payload, '$.Display.EpisodeCheck'), '$.State', 'closed', '$.Reason', ?,
			'$.Revision', revision + 1, '$.Job.CompleteSet', json('false'), '$.NextCheckAt', ?)
		WHERE instance_id = ? AND state <> 'closed' RETURNING id`, reason, stamp(now), reason, stamp(time.Time{}), ref.ID)
	if err != nil {
		return err
	}
	var closed []domain.CaseID
	for rows.Next() {
		var id domain.CaseID
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		closed = append(closed, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, id := range closed {
		if err := store.AppendEvent(ctx, tx, domain.Event{ID: domain.EventID(store.NewID()), Kind: "recovery_connection_removed",
			Actor: actor, Scope: domain.Scope{Kind: domain.ScopeInstance, Instance: ref},
			CaseID: id, Reason: reason, OccurredAt: now}); err != nil {
			return err
		}
	}
	return nil
}
