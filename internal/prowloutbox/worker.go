package prowloutbox

import (
	"context"
	"database/sql"
	"errors"
	"hash/fnv"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/prowl"
)

type claim struct {
	ID       domain.OutboxID
	Route    domain.RouteID
	Revision domain.Revision
	Number   int
	KeyRef   string
	Category string
	Count    int
	Lease    time.Time
}

// RunOne sends at most one local claim. A crash after claiming is uncertain
// because the provider has no wire idempotency key, even if transport never ran.
func (s *Service) RunOne(ctx context.Context) (bool, error) {
	if !s.enabled || s.client == nil {
		return false, ErrDisabled
	}
	work, err := s.claim(ctx, s.clock.Now())
	if err != nil || work.ID == "" {
		return false, err
	}
	var result prowl.Result
	var sendErr error
	key, err := s.db.ReadSecret(work.KeyRef)
	if err != nil {
		return true, s.complete(ctx, work, s.clock.Now(), prowl.Result{}, "notification_key_unavailable")
	}
	now := s.clock.Now()
	if ctx.Err() != nil || !now.Add(10*time.Second).Before(work.Lease) {
		return true, s.complete(ctx, work, now, prowl.Result{}, "notification_not_sent")
	}
	call, cancel := context.WithTimeout(ctx, 10*time.Second)
	result, sendErr = s.client.Send(call, key, render(work.Category, work.Count))
	cancel()
	if sendErr != nil {
		return true, s.complete(ctx, work, s.clock.Now(), prowl.Result{}, "notification_invalid_request")
	}
	return true, s.complete(ctx, work, s.clock.Now(), result, "")
}

func (s *Service) claim(ctx context.Context, now time.Time) (claim, error) {
	var work claim
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		at := stamp(now)
		if err := settleAbandoned(ctx, tx, at); err != nil {
			return err
		}
		if err := pruneDeliveries(ctx, tx, stamp(now.Add(-30*24*time.Hour))); err != nil {
			return err
		}
		var recent int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM prowl_attempts WHERE sent_at > ?`, stamp(now.Add(-time.Hour))).Scan(&recent); err != nil {
			return err
		}
		if recent >= globalRateCap {
			return nil
		}
		err := tx.QueryRowContext(ctx, `SELECT o.id, o.route_id, o.route_revision, o.attempts, r.key_ref, d.category, d.collected
			FROM notification_outbox o JOIN prowl_deliveries d ON d.outbox_id = o.id JOIN prowl_routes r ON r.id = o.route_id
			WHERE o.state IN ('pending','delayed') AND o.scheduled_at <= ? AND o.expires_at > ? AND o.attempts < ?
			AND r.enabled = 1 AND r.paused = 0 AND r.revision = o.route_revision AND r.not_before <= ?
			AND NOT EXISTS (SELECT 1 FROM prowl_deliveries p JOIN notification_outbox q ON q.id = p.outbox_id
				WHERE p.route_id = r.id AND q.state = 'sending')
			ORDER BY o.scheduled_at, o.id LIMIT 1`, at, at, maxAttempts, at).
			Scan(&work.ID, &work.Route, &work.Revision, &work.Number, &work.KeyRef, &work.Category, &work.Count)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		work.Lease = now.Add(claimLease)
		work.Number++
		if _, err := tx.ExecContext(ctx, `UPDATE notification_outbox SET state = 'sending', attempts = ?, reason = '' WHERE id = ?`, work.Number, work.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE prowl_deliveries SET lease_until = ? WHERE outbox_id = ?`, stamp(work.Lease), work.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE prowl_routes SET not_before = ? WHERE id = ? AND revision = ?`, stamp(now.Add(minSendGap)), work.Route, work.Revision); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO prowl_attempts (outbox_id, number, sent_at, state) VALUES (?, ?, ?, 'sending')`, work.ID, work.Number, at)
		return err
	})
	return work, err
}

func settleAbandoned(ctx context.Context, tx *sql.Tx, now string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE prowl_attempts SET state = 'uncertain', completed_at = ?, reason = 'notification_acknowledgement_unknown'
		WHERE state = 'sending' AND EXISTS (SELECT 1 FROM prowl_deliveries d WHERE d.outbox_id = prowl_attempts.outbox_id AND d.lease_until <= ?)`, now, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE notification_outbox SET state = 'uncertain', reason = 'notification_acknowledgement_unknown'
		WHERE state = 'sending' AND EXISTS (SELECT 1 FROM prowl_deliveries d WHERE d.outbox_id = notification_outbox.id AND d.lease_until <= ?)`, now); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE notification_outbox AS o SET state = 'suppressed', reason = CASE
			WHEN expires_at <= ? THEN 'notification_expired' ELSE 'notification_route_changed' END
		WHERE state IN ('pending','delayed') AND EXISTS (SELECT 1 FROM prowl_deliveries d JOIN prowl_routes r ON r.id = d.route_id
			WHERE d.outbox_id = o.id AND (o.expires_at <= ? OR o.route_revision <> r.revision))`, now, now)
	return err
}

func pruneDeliveries(ctx context.Context, tx *sql.Tx, cutoff string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM notification_outbox WHERE id IN (SELECT o.id FROM notification_outbox o
		JOIN prowl_deliveries d ON d.outbox_id = o.id JOIN audit_events e ON e.id = o.event_id
		WHERE e.occurred_at < ? AND o.state IN ('accepted','uncertain','suppressed','failed')
		ORDER BY e.sequence LIMIT 256)`, cutoff)
	return err
}

func (s *Service) complete(ctx context.Context, work claim, now time.Time, result prowl.Result, local domain.ReasonCode) error {
	journal, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	state, when, pause, reason := classify(work, now, result, local)
	return s.db.Write(journal, func(tx *sql.Tx) error {
		changed, err := tx.ExecContext(journal, `UPDATE notification_outbox SET state = ?, reason = ?, scheduled_at =
			CASE WHEN ? <> '' THEN ? ELSE scheduled_at END WHERE id = ? AND attempts = ? AND state = 'sending'`, state, reason, when, when, work.ID, work.Number)
		if err != nil {
			return err
		}
		if err := oneChanged(changed); err != nil {
			return err
		}
		updated, err := tx.ExecContext(journal, `UPDATE prowl_attempts SET completed_at = ?, state = ?, reason = ?, provider_code = ?
			WHERE outbox_id = ? AND number = ? AND state = 'sending'`, stamp(now), state, reason, result.ProviderCode, work.ID, work.Number)
		if err != nil {
			return err
		}
		if err := oneChanged(updated); err != nil {
			return err
		}
		if pause {
			_, err := tx.ExecContext(journal, `UPDATE prowl_routes SET paused = 1 WHERE id = ? AND revision = ?`, work.Route, work.Revision)
			return err
		}
		if result.Status == prowl.Quota || (result.Status == prowl.Accepted && result.Remaining == 0 && result.ResetAt.After(now)) {
			resume := result.ResetAt
			if when != "" {
				parsed, err := time.Parse(time.RFC3339Nano, when)
				if err != nil {
					return err
				}
				resume = parsed
			}
			if resume.After(now.Add(24 * time.Hour)) {
				resume = now.Add(24 * time.Hour)
			}
			_, err := tx.ExecContext(journal, `UPDATE prowl_routes SET not_before = max(not_before, ?) WHERE id = ? AND revision = ?`, stamp(resume), work.Route, work.Revision)
			return err
		}
		return nil
	})
}

func classify(work claim, now time.Time, result prowl.Result, local domain.ReasonCode) (state domain.OutboxState, when string, pause bool, reason domain.ReasonCode) {
	var delay time.Duration
	switch {
	case local == "notification_key_unavailable", local == "notification_not_sent":
		delay, reason = time.Minute, local
	case local != "":
		return domain.OutboxFailed, "", false, local
	case result.Status == prowl.Accepted:
		return domain.OutboxAccepted, "", false, "notification_provider_accepted"
	case result.Status == prowl.InvalidKey:
		return domain.OutboxFailed, "", true, "notification_key_invalid"
	case result.Status == prowl.Rejected:
		return domain.OutboxFailed, "", false, "notification_rejected"
	case result.Status == prowl.Quota:
		delay, reason = max(time.Minute, result.RetryAfter, result.ResetAt.Sub(now)), "notification_quota"
		if result.RetryAfter <= 0 && !result.ResetAt.After(now) {
			delay = time.Hour
		}
	case result.Status == prowl.Transient:
		delay, reason = max(time.Minute<<uint(work.Number-1), result.RetryAfter), "notification_transient"
	default:
		return domain.OutboxUncertain, "", false, "notification_acknowledgement_unknown"
	}
	if work.Number >= maxAttempts {
		return domain.OutboxFailed, "", false, "notification_retry_exhausted"
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(work.ID))
	due := now.Add(min(delay+time.Duration(h.Sum32()%20)*time.Second, 24*time.Hour))
	return domain.OutboxDelayed, stamp(due), false, reason
}

type Status struct {
	ID        domain.OutboxID
	Route     domain.RouteID
	Category  string
	State     domain.OutboxState
	Count     int
	Attempts  int
	Reason    domain.ReasonCode
	Scheduled time.Time
}

func (s *Service) Recent(ctx context.Context, route domain.RouteID, limit int) ([]Status, error) {
	if route == "" || len(route) > 128 || limit <= 0 || limit > 100 {
		return nil, ErrInvalid
	}
	var statuses []Status
	err := s.db.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT o.id, o.route_id, d.category, o.state, d.collected, o.attempts, o.reason, o.scheduled_at
			FROM notification_outbox o JOIN prowl_deliveries d ON d.outbox_id = o.id JOIN audit_events e ON e.id = o.event_id
			WHERE o.route_id = ? ORDER BY e.sequence DESC LIMIT ?`, route, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item Status
			var at string
			if err := rows.Scan(&item.ID, &item.Route, &item.Category, &item.State, &item.Count, &item.Attempts, &item.Reason, &at); err != nil {
				return err
			}
			item.Scheduled, err = time.Parse(time.RFC3339Nano, at)
			if err != nil {
				return err
			}
			statuses = append(statuses, item)
		}
		return rows.Err()
	})
	return statuses, err
}
