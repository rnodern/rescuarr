package prowloutbox

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/prowl"
	"github.com/rnodern/rescuarr/internal/store"
)

// Append publishes an existing domain transition and its selected notifications
// in the caller's transaction. It must be used instead of a separate AppendEvent.
func (s *Service) Append(ctx context.Context, tx *sql.Tx, event domain.Event) error {
	if err := store.AppendEvent(ctx, tx, event); err != nil {
		return err
	}
	category, name, group := eventRoute(event)
	if category == 0 {
		return nil
	}
	routes, err := matchingRoutes(ctx, tx, category)
	if err != nil {
		return err
	}
	for _, route := range routes {
		due, slot, err := deliveryTime(route, name, event.OccurredAt)
		if err != nil {
			return err
		}
		// Digest rows are kept separate across quiet periods and categories.
		dedupe := name + "/" + group
		if name == "incident" || name == "resolution" {
			dedupe += "/" + stamp(event.OccurredAt.Truncate(time.Hour))
		}
		if slot != "" {
			nameKey := "digest/" + name
			dedupe = nameKey + "/" + slot
		}
		if _, err := s.enqueue(ctx, tx, route, event, name, dedupe, due, slot != ""); err != nil {
			return err
		}
	}
	return nil
}

func matchingRoutes(ctx context.Context, tx *sql.Tx, category Category) ([]Route, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, revision, timezone, quiet_start, quiet_end, digest FROM prowl_routes
		WHERE enabled = 1 AND (mask & ?) <> 0 ORDER BY id`, category)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var routes []Route
	for rows.Next() {
		var r Route
		if err := rows.Scan(&r.ID, &r.Revision, &r.Timezone, &r.QuietStart, &r.QuietEnd, &r.Digest); err != nil {
			return nil, err
		}
		routes = append(routes, r)
	}
	return routes, rows.Err()
}

func eventRoute(event domain.Event) (Category, string, string) {
	switch event.Kind {
	case "action_required", "action_unresolved":
		return Attention, "attention", string(event.CaseID)
	case "recovery_verified":
		return Recovery, "recovery", string(event.IntentID)
	case "incident_opened", "incident_reopened":
		return Incident, "incident", string(event.IncidentID)
	case "incident_resolved":
		return Resolution, "resolution", string(event.IncidentID)
	case "automation_paused":
		return Pause, "pause", string(event.ID)
	default:
		return 0, "", ""
	}
}

func deliveryTime(route Route, category string, now time.Time) (time.Time, string, error) {
	zone, err := time.LoadLocation(route.Timezone)
	if err != nil {
		return time.Time{}, "", ErrInvalid
	}
	due := now.UTC()
	if route.Digest && category == "recovery" {
		due = due.Truncate(time.Hour).Add(time.Hour)
	}
	// Iterating actual minutes rather than constructing an ambiguous local wall
	// clock handles daylight saving folds and nonexistent quiet-hour endpoints.
	for until := due.Add(27 * time.Hour); quiet(route, due.In(zone)) && due.Before(until); {
		due = due.Truncate(time.Minute).Add(time.Minute)
	}
	if route.Digest && category == "recovery" {
		return due, stamp(due), nil
	}
	return due, "", nil
}

func quiet(route Route, at time.Time) bool {
	minute := at.Hour()*60 + at.Minute()
	if route.QuietStart <= route.QuietEnd {
		return minute >= route.QuietStart && minute < route.QuietEnd
	}
	return minute >= route.QuietStart || minute < route.QuietEnd
}

func (s *Service) enqueue(ctx context.Context, tx *sql.Tx, route Route, event domain.Event, category, dedupe string, due time.Time, digest bool) (domain.OutboxID, error) {
	if dedupe == "" {
		return "", ErrInvalid
	}
	if strings.HasSuffix(dedupe, "/") {
		dedupe += string(event.ID)
	}
	hash := sha256.Sum256([]byte(dedupe))
	key := fmt.Sprintf("%x", hash)
	var existing domain.OutboxID
	err := tx.QueryRowContext(ctx, `SELECT id FROM notification_outbox WHERE route_id = ? AND route_revision = ? AND dedupe_key = ?`,
		route.ID, route.Revision, key).Scan(&existing)
	if err == nil {
		if !digest {
			return existing, nil
		}
		_, err = tx.ExecContext(ctx, `UPDATE prowl_deliveries SET collected = collected + 1 WHERE outbox_id = ? AND collected < 10000
			AND EXISTS (SELECT 1 FROM notification_outbox WHERE id = ? AND state IN ('pending','delayed'))`, existing, existing)
		return existing, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM notification_outbox WHERE route_id = ? AND state IN ('pending','delayed','sending')`, route.ID).Scan(&count); err != nil {
		return "", err
	}
	if count >= maxPending {
		return "", nil
	}
	id := domain.OutboxID(store.NewID())
	if digest {
		category = "digest"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notification_outbox (id, event_id, route_id, route_revision, dedupe_key,
		state, scheduled_at, expires_at, attempts, reason) VALUES (?, ?, ?, ?, ?, 'pending', ?, ?, 0, '')`, id, event.ID, route.ID,
		route.Revision, key, stamp(due), stamp(due.Add(72*time.Hour))); err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO prowl_deliveries (outbox_id, route_id, category) VALUES (?, ?, ?)`, id, route.ID, category)
	return id, err
}

// QueueTest records a generic test for the explicitly selected current route.
// Authentication and CSRF stay with the owner-only route invoking this method.
func (s *Service) QueueTest(ctx context.Context, id domain.RouteID, revision domain.Revision, actor domain.ActorID) (domain.OutboxID, error) {
	if id == "" || len(id) > 128 || revision == 0 || actor == "" {
		return "", ErrInvalid
	}
	var delivery domain.OutboxID
	now := s.clock.Now().UTC()
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		route := Route{ID: id}
		if err := tx.QueryRowContext(ctx, `SELECT revision, enabled, paused, timezone, quiet_start, quiet_end FROM prowl_routes WHERE id = ?`, id).
			Scan(&route.Revision, &route.Enabled, &route.Paused, &route.Timezone, &route.QuietStart, &route.QuietEnd); err != nil {
			return err
		}
		if route.Revision != revision || !route.Enabled || route.Paused {
			return ErrDisabled
		}
		var recent int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM prowl_deliveries d JOIN notification_outbox o ON o.id = d.outbox_id
			JOIN audit_events e ON e.id = o.event_id WHERE d.route_id = ? AND d.category = 'test' AND e.occurred_at > ?`,
			id, stamp(now.Add(-time.Minute))).Scan(&recent); err != nil {
			return err
		}
		if recent > 0 {
			return ErrLimit
		}
		event := domain.Event{ID: domain.EventID(store.NewID()), Kind: "notification_test_requested", Actor: actor, OccurredAt: now}
		if err := store.AppendEvent(ctx, tx, event); err != nil {
			return err
		}
		due, _, err := deliveryTime(route, "test", now)
		if err != nil {
			return err
		}
		delivery, err = s.enqueue(ctx, tx, route, event, "test", "test/"+string(event.ID), due, false)
		if err == nil && delivery == "" {
			return ErrLimit
		}
		return err
	})
	return delivery, err
}

func render(category string, count int) prowl.Message {
	event, description := "Rescuarr update", "Open the Rescuarr console to review activity."
	switch category {
	case "attention":
		event = "Recovery needs attention"
	case "recovery":
		event = "Recovery verified"
	case "incident":
		event = "Recovery incident opened"
	case "resolution":
		event = "Recovery incident resolved"
	case "pause":
		event = "Automation paused"
	case "test":
		event, description = "Test notification", "Rescuarr can submit a Prowl notification."
	case "digest":
		event, description = "Recovery digest", fmt.Sprintf("Verified recovery events: %d.", count)
	}
	return prowl.Message{Application: "Rescuarr", Event: event, Description: description}
}
