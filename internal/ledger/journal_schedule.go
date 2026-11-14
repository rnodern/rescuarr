package ledger

import (
	"context"
	"database/sql"
	"errors"
	"hash/fnv"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
)

const schedulerPageSize = 16

type dispatchCursor struct {
	created string
	id      domain.IntentID
}

// Each dispatch page includes expired authorisations so they can be audited
// and retired even while a global pause or unresolved attempt blocks writes.
type dispatchCandidate struct {
	cursor    dispatchCursor
	available bool
	expired   bool
}

func (j *Journal) dispatchCandidates(ctx context.Context, after dispatchCursor, now time.Time) ([]dispatchCandidate, bool, error) {
	var candidates []dispatchCandidate
	var expiredReservation bool
	err := j.local.Read(ctx, func(tx *sql.Tx) error {
		var globallyAvailable bool
		// The scope-lock primary key bounds this check to active reservations.
		if err := tx.QueryRowContext(ctx, `SELECT mode IN ('assist', 'autonomous') AND NOT reconcile_required
			AND NOT paused AND (SELECT count(*) FROM action_scope_locks WHERE kind = 'instance') < 2,
			EXISTS (SELECT 1 FROM action_scope_locks AS l JOIN action_intents AS i ON i.id = l.intent_id
				WHERE l.kind = 'instance' AND i.state = 'reserved' AND i.lease_until <= ?)
			FROM app_state JOIN action_pause USING (singleton) WHERE singleton = 1`, journalTime(now)).
			Scan(&globallyAvailable, &expiredReservation); err != nil {
			return err
		}
		if expiredReservation {
			return nil
		}
		// The explicit active-state predicate admits the existing partial
		// index so completed action history does not enter the work scan.
		const query = `SELECT i.created_at, i.id, json_extract(i.proposal, '$.ExpiresAt'),
				? AND NOT EXISTS (SELECT 1 FROM action_scope_locks AS l WHERE
					(l.kind = 'instance' AND l.scope_key = json_extract(i.proposal, '$.Operation.Job.Instance.ID'))
					OR (l.kind = 'job' AND l.scope_key = i.physical_job)
					OR (l.kind = 'client' AND l.scope_key = i.client))
			FROM action_intents AS i
			WHERE i.state = 'authorized' AND i.state NOT IN ('completed', 'cancelled')
				AND (i.created_at, i.id) > (?, ?)
				AND NOT EXISTS (SELECT 1 FROM action_attempts AS a WHERE a.intent_id = i.id)
			ORDER BY i.created_at, i.id LIMIT ?`
		rows, err := tx.QueryContext(ctx, query, globallyAvailable, after.created, after.id, schedulerPageSize)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var candidate dispatchCandidate
			var expiresAt string
			if err := rows.Scan(&candidate.cursor.created, &candidate.cursor.id, &expiresAt, &candidate.available); err != nil {
				return err
			}
			expiry, err := time.Parse(time.RFC3339Nano, expiresAt)
			if err != nil {
				return ErrJournal
			}
			candidate.expired = !now.Before(expiry)
			candidates = append(candidates, candidate)
		}
		return rows.Err()
	})
	return candidates, expiredReservation, err
}

func (j *Journal) expireAuthorization(ctx context.Context, id domain.IntentID, now time.Time) error {
	return j.local.Write(ctx, func(tx *sql.Tx) error {
		row, err := readIntent(ctx, tx, id)
		if err != nil {
			return err
		}
		if row.intent.State != domain.IntentAuthorized || now.Before(row.intent.Proposal.ExpiresAt) {
			return nil
		}
		var hasAttempt bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM action_attempts WHERE intent_id = ?)`, id).Scan(&hasAttempt); err != nil {
			return err
		}
		if hasAttempt {
			return ErrJournal
		}
		return cancelIntent(ctx, tx, row, "authorization_expired_unsent", now)
	})
}

func (j *Journal) scheduledAttempt(ctx context.Context, now time.Time) (domain.AttemptID, error) {
	var id domain.AttemptID
	err := j.local.Read(ctx, func(tx *sql.Tx) error {
		// Match the lease rules at discovery and recheck in BeginReconcile.
		// At most two instance locks can belong to outstanding attempts.
		// SQL uses the journal's fixed-width timestamp for durable deadlines.
		// Legacy outcomes have no pacing metadata and are due once lease-ready.
		const query = `WITH due AS (
			SELECT a.id, a.sent_at, i.lease_until, coalesce(json_extract(a.outcome, '$.nextRead'), '') AS next_read
			FROM action_scope_locks AS l
			JOIN action_intents AS i ON i.id = l.intent_id
			JOIN action_attempts AS a ON a.intent_id = i.id
			WHERE l.kind = 'instance'
			  AND i.state IN ('sending', 'acknowledged', 'rejected_before_effect', 'uncertain', 'reconciling')
			  AND i.lease_until <= ? AND a.sent_at <= ?
		)
		SELECT id FROM due WHERE next_read <= ?
		ORDER BY max(sent_at, lease_until, next_read), id LIMIT 1`
		err := tx.QueryRowContext(ctx, query, journalTime(now), journalTime(now.Add(-initialReadback)),
			journalTime(now)).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	return id, err
}

var readbackStages = [...]time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute}

func readbackDelay(id domain.AttemptID, stage int) time.Duration {
	delay := readbackStages[stage-1]
	// Spread attempts predictably across restarts. Including the stage means
	// successive reads of the same attempt do not all use the same offset.
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(id))
	_, _ = hash.Write([]byte{byte(stage)})
	return delay + time.Duration(hash.Sum32()%uint32(delay/time.Second/5+1))*time.Second
}
