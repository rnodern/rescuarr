package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
)

func AppendEvent(ctx context.Context, tx *sql.Tx, event domain.Event, outbox ...domain.OutboxItem) error {
	if event.ID == "" || event.Kind == "" || event.OccurredAt.IsZero() {
		return errors.New("audit event requires an identity, kind and timestamp")
	}
	evidence, err := json.Marshal(event.Evidence)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO audit_events (id, kind, actor, scope_kind, scope_key, instance_id, instance_epoch,
			case_id, incident_id, intent_id, reason, evidence, occurred_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.ID, event.Kind, event.Actor, event.Scope.Kind, event.Scope.Key, event.Scope.Instance.ID, event.Scope.Instance.Epoch,
		event.CaseID, event.IncidentID, event.IntentID, event.Reason, string(evidence), utc(event.OccurredAt))
	if err != nil {
		return err
	}
	for _, item := range outbox {
		if item.EventID != event.ID || item.ID == "" || item.RouteID == "" || item.DedupeKey == "" ||
			item.ScheduledAt.IsZero() || !item.ExpiresAt.After(item.ScheduledAt) {
			return errors.New("invalid notification outbox item")
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO notification_outbox (id, event_id, route_id, route_revision, dedupe_key,
				state, scheduled_at, expires_at, attempts, reason)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (route_id, route_revision, dedupe_key) DO NOTHING`,
			item.ID, item.EventID, item.RouteID, item.RouteRevision, item.DedupeKey, item.State,
			utc(item.ScheduledAt), utc(item.ExpiresAt), item.Attempts, item.Reason)
		if err != nil {
			return err
		}
	}
	return nil
}

func utc(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }
