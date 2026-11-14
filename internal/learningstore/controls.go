package learningstore

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/learning"
	"github.com/rnodern/rescuarr/internal/store"
)

// Apply persists the control and its immutable audit event in one local
// transaction. ExpectedRevision protects against stale operator requests.
func (s *Store) Apply(ctx context.Context, id domain.PatternID, command learning.Command) (learning.Controls, error) {
	if !validPatternID(id) || !validID(string(command.Actor)) || !validID(string(command.Event)) ||
		!validTime(command.At) || command.ExpectedRevision >= math.MaxInt64 {
		return learning.Controls{}, learning.ErrInvalid
	}
	var controls learning.Controls
	err := s.local.Write(ctx, func(tx *sql.Tx) error {
		p, err := readPattern(ctx, tx, id)
		if err != nil {
			return err
		}
		if p.controls.Revision != command.ExpectedRevision {
			return learning.ErrConflict
		}
		at := stamp(command.At)
		if at < p.changed {
			return learning.ErrInvalid
		}
		updated, audit, err := learning.Apply(p.pattern, p.controls, command)
		if err != nil {
			return err
		}
		var existing bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM audit_events WHERE id = ?)`, command.Event).Scan(&existing); err != nil {
			return err
		}
		if existing {
			return learning.ErrConflict
		}
		if err := store.AppendEvent(ctx, tx, audit.Event); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO learning_control_audit (pattern_id, revision, event_id, change) VALUES (?, ?, ?, ?)`,
			id, updated.Revision, audit.Event.ID, audit.Change); err != nil {
			return err
		}
		reset := ""
		if !updated.ResetAt.IsZero() {
			reset = stamp(updated.ResetAt)
		}
		changed, err := tx.ExecContext(ctx, `UPDATE learning_patterns SET control_revision = ?, paused = ?, reset_at = ?, changed_at = ?
			WHERE id = ? AND control_revision = ?`, updated.Revision, updated.Paused, reset, at, id, command.ExpectedRevision)
		if err != nil {
			return err
		}
		count, err := changed.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return learning.ErrConflict
		}
		controls = updated
		return nil
	})
	return controls, err
}

// ControlEvent binds a pattern revision to its operator audit event.
type ControlEvent struct {
	Revision domain.Revision
	Audit    learning.Audit
}

// Audit returns the next bounded page of exact-pattern control events.
func (s *Store) Audit(ctx context.Context, id domain.PatternID, after domain.Revision, limit int) ([]ControlEvent, error) {
	if !validPatternID(id) || after > math.MaxInt64 || limit <= 0 || limit > maxPage {
		return nil, learning.ErrInvalid
	}
	var events []ControlEvent
	err := s.local.Read(ctx, func(tx *sql.Tx) error {
		var found int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM learning_patterns WHERE id = ?`, id).Scan(&found); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT c.revision, c.change, e.id, e.sequence, e.kind, e.actor,
			e.scope_kind, e.instance_id, e.instance_epoch, e.reason, e.occurred_at
			FROM learning_control_audit c JOIN audit_events e ON e.id = c.event_id
			WHERE c.pattern_id = ? AND c.revision > ? ORDER BY c.revision LIMIT ?`, id, after, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			item := ControlEvent{Audit: learning.Audit{Pattern: id}}
			e := &item.Audit.Event
			var at string
			if err := rows.Scan(&item.Revision, &item.Audit.Change, &e.ID, &e.Sequence, &e.Kind, &e.Actor,
				&e.Scope.Kind, &e.Scope.Instance.ID, &e.Scope.Instance.Epoch, &e.Reason, &at); err != nil {
				return err
			}
			e.OccurredAt, err = time.Parse(timeLayout, at)
			if err != nil {
				return ErrHistory
			}
			events = append(events, item)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return events, nil
}
