package recoveryservice

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/ledger"
	"github.com/rnodern/rescuarr/internal/store"
)

type EmptyPackKey struct {
	CaseID domain.CaseID
	Job    domain.JobRef
}

type EmptyPackStage string

const (
	EmptyPackRejectSending  EmptyPackStage = "reject_sending"
	EmptyPackRejectVerified EmptyPackStage = "reject_verified"
	EmptyPackSearchSending  EmptyPackStage = "search_sending"
	EmptyPackDone           EmptyPackStage = "done"
)

// EmptyPackCheckpoint holds the original download identity and the exact search
// scope after queue removal. Keep it within recovery and out of browser views.
type EmptyPackCheckpoint struct {
	Key                EmptyPackKey
	Stage              EmptyPackStage
	ConnectionRevision domain.Revision
	Release            string
	DownloadID         string
	QueueIDs           []int64
	SeriesID           int64
	EpisodeIDs         []int64
	SearchIDs          []int64
	CommandID          int64
	StartedAt          time.Time
	manualToken        string
	manualScopeHash    string
	languageRevision   domain.Revision
	outputPath         string
	clientLabel        string
}

type EmptyPackJournal struct {
	local *store.Store
}

func NewEmptyPackJournal(local *store.Store) *EmptyPackJournal {
	return &EmptyPackJournal{local: local}
}

const emptyPackColumns = `case_id, instance_id, instance_epoch, job_id, job_generation, stage,
	connection_revision, release, download_id, queue_ids, series_id, episode_ids, search_ids, command_id, started_at, manual_token`

const emptyPackKeyWhere = `instance_id = ? AND instance_epoch = ? AND job_id = ? AND job_generation = ?`

func (key EmptyPackKey) valid() bool {
	return emptyPackToken(string(key.CaseID), 128) && emptyPackToken(string(key.Job.Instance.ID), 128) &&
		emptyPackToken(string(key.Job.ID), 128) && key.Job.Instance.Epoch > 0 &&
		key.Job.Instance.Epoch <= math.MaxInt64 && key.Job.Generation > 0 && key.Job.Generation <= math.MaxInt64
}

func (key EmptyPackKey) values() []any {
	return []any{key.Job.Instance.ID, key.Job.Instance.Epoch, key.Job.ID, key.Job.Generation}
}

func (j *EmptyPackJournal) Load(ctx context.Context, key EmptyPackKey) (EmptyPackCheckpoint, bool, error) {
	if j == nil || j.local == nil || !key.valid() {
		return EmptyPackCheckpoint{}, false, ledger.ErrInvalid
	}
	var checkpoint EmptyPackCheckpoint
	err := j.local.Read(ctx, func(tx *sql.Tx) error {
		var err error
		checkpoint, err = loadEmptyPack(ctx, tx, key)
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return EmptyPackCheckpoint{}, false, nil
	}
	return checkpoint, err == nil, err
}

func (j *EmptyPackJournal) loadManual(ctx context.Context, id domain.CaseID, token string) (EmptyPackCheckpoint, bool, error) {
	var checkpoint EmptyPackCheckpoint
	err := j.local.Read(ctx, func(tx *sql.Tx) error {
		var err error
		checkpoint, err = scanEmptyPack(tx.QueryRowContext(ctx, `SELECT `+emptyPackColumns+
			` FROM empty_pack_recovery WHERE case_id = ? AND manual_token = ?`, id, token))
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return EmptyPackCheckpoint{}, false, nil
	}
	return checkpoint, err == nil, err
}

// Pending returns recoveries whose rejection can be read back or whose search
// can still be authorised. A recorded search send is never admitted again.
func (j *EmptyPackJournal) Pending(ctx context.Context, limit int) ([]EmptyPackCheckpoint, error) {
	if j == nil || j.local == nil || limit < 1 || limit > 128 {
		return nil, ledger.ErrInvalid
	}
	var checkpoints []EmptyPackCheckpoint
	err := j.local.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+emptyPackColumns+` FROM empty_pack_recovery AS recovery
			WHERE stage IN ('reject_sending', 'reject_verified') AND EXISTS (
				SELECT 1 FROM observe_instances AS i WHERE i.id = recovery.instance_id
				AND i.epoch = recovery.instance_epoch AND i.connection_revision = recovery.connection_revision
				AND i.health <> 'removed'
			) ORDER BY updated_at, case_id, instance_id, instance_epoch, job_id, job_generation LIMIT ?`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			checkpoint, err := scanEmptyPack(rows)
			if err != nil {
				return err
			}
			checkpoints = append(checkpoints, checkpoint)
		}
		return rows.Err()
	})
	return checkpoints, err
}

// BeginReject records a one-shot queue removal before the caller issues it.
// A duplicate, stale or held checkpoint never authorises another send.
func (j *EmptyPackJournal) BeginReject(ctx context.Context, key EmptyPackKey, checkpoint EmptyPackCheckpoint, now time.Time) (bool, error) {
	if j == nil || j.local == nil || !key.valid() || now.IsZero() ||
		(checkpoint.Key != (EmptyPackKey{}) && checkpoint.Key != key) ||
		(checkpoint.Stage != "" && checkpoint.Stage != EmptyPackRejectSending) ||
		!validEmptyPackIdentity(checkpoint) || len(checkpoint.SearchIDs) > 0 || checkpoint.CommandID != 0 ||
		(checkpoint.manualToken == "") != (checkpoint.manualScopeHash == "") {
		return false, ledger.ErrInvalid
	}
	queue, err := json.Marshal(checkpoint.QueueIDs)
	if err != nil {
		return false, err
	}
	episodes, err := json.Marshal(checkpoint.EpisodeIDs)
	if err != nil {
		return false, err
	}
	wrote := false
	err = j.local.Write(ctx, func(tx *sql.Tx) error {
		if _, err := loadEmptyPack(ctx, tx, key); !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := allowEmptyPackSend(ctx, tx, key.Job.Instance, checkpoint.ConnectionRevision, checkpoint.manualToken != ""); err != nil {
			return err
		}
		if checkpoint.manualToken != "" {
			var approved bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM manual_reviews AS m JOIN observe_cases AS c
				ON c.id = m.case_id AND c.revision = m.case_revision
				WHERE m.case_id = ? AND m.token = ? AND m.state = 'approved' AND m.action = 'empty_pack' AND m.scope_hash = ?
				AND c.instance_id = ? AND c.epoch = ? AND c.state = 'held' AND c.reason = 'no_eligible_files'
				AND c.observation_only = 0 AND c.missing_complete_scans = 0
				AND json_extract(c.payload, '$.Job.Ref.ID') = ?
				AND json_extract(c.payload, '$.Job.Ref.Generation') = ?
				AND COALESCE((SELECT revision FROM language_preferences WHERE singleton = 1), 0) = ?)`,
				key.CaseID, checkpoint.manualToken, checkpoint.manualScopeHash, key.Job.Instance.ID,
				key.Job.Instance.Epoch, key.Job.ID, key.Job.Generation, checkpoint.languageRevision).
				Scan(&approved); err != nil {
				return err
			}
			if !approved {
				return app.ErrConflict
			}
		}
		var active bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM empty_pack_recovery
			WHERE instance_id = ? AND stage <> 'done')`, key.Job.Instance.ID).Scan(&active); err != nil {
			return err
		}
		if active {
			return app.ErrHeld
		}
		at := emptyPackStamp(now)
		_, err := tx.ExecContext(ctx, `INSERT INTO empty_pack_recovery (case_id, instance_id, instance_epoch, job_id,
			job_generation, connection_revision, release, download_id, queue_ids, series_id, episode_ids, stage, started_at, updated_at, manual_token)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'reject_sending', ?, ?, ?)`,
			key.CaseID, key.Job.Instance.ID, key.Job.Instance.Epoch, key.Job.ID, key.Job.Generation,
			checkpoint.ConnectionRevision, checkpoint.Release, checkpoint.DownloadID, string(queue),
			checkpoint.SeriesID, string(episodes), at, at, checkpoint.manualToken)
		if err != nil {
			return err
		}
		if err := appendEmptyPackEvent(ctx, tx, key, "recovery_empty_pack_reject_sending", "", now); err != nil {
			return err
		}
		wrote = true
		return nil
	})
	return err == nil && wrote, err
}

// Advance moves exactly one checkpoint forward. Queue and episode identity were
// fixed before removal. Only a search reservation can add candidate search IDs.
func (j *EmptyPackJournal) Advance(ctx context.Context, key EmptyPackKey, expected EmptyPackStage, next EmptyPackCheckpoint, now time.Time) (bool, error) {
	if j == nil || j.local == nil || !key.valid() || now.IsZero() ||
		(next.Key != (EmptyPackKey{}) && next.Key != key) {
		return false, ledger.ErrInvalid
	}
	wrote := false
	err := j.local.Write(ctx, func(tx *sql.Tx) error {
		checkpoint, err := loadEmptyPack(ctx, tx, key)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if checkpoint.Stage != expected {
			return nil
		}
		if now.Before(checkpoint.StartedAt) {
			return ledger.ErrInvalid
		}
		var kind domain.EventKind
		var reason domain.ReasonCode
		searchIDs := checkpoint.SearchIDs
		switch {
		case expected == EmptyPackRejectSending && next.Stage == EmptyPackRejectVerified:
			if len(next.SearchIDs) != 0 || next.CommandID != 0 {
				return ledger.ErrInvalid
			}
			kind = "recovery_empty_pack_reject_verified"
		case expected == EmptyPackRejectVerified && next.Stage == EmptyPackSearchSending:
			if next.CommandID != 0 || !validPackSearchIDs(next.SearchIDs, checkpoint.EpisodeIDs) {
				return ledger.ErrInvalid
			}
			if err := allowEmptyPackSend(ctx, tx, key.Job.Instance, checkpoint.ConnectionRevision, checkpoint.manualToken != ""); err != nil {
				return err
			}
			if checkpoint.manualToken != "" {
				approved, err := manualPackSearchIDs(ctx, tx, checkpoint)
				if err != nil {
					return err
				}
				if slices.ContainsFunc(next.SearchIDs, func(id int64) bool { return !slices.Contains(approved, id) }) {
					return app.ErrConflict
				}
			}
			searchIDs, kind = next.SearchIDs, "recovery_empty_pack_search_sending"
		case expected == EmptyPackRejectVerified && next.Stage == EmptyPackDone:
			if len(next.SearchIDs) != 0 || next.CommandID != 0 {
				return ledger.ErrInvalid
			}
			kind, reason = "recovery_empty_pack_done", "empty_pack_no_search_needed"
			if checkpoint.manualToken != "" {
				reason = "empty_pack_no_approved_search_needed"
			}
		case expected == EmptyPackSearchSending && next.Stage == EmptyPackDone:
			if next.CommandID <= 0 || next.CommandID > math.MaxInt32 ||
				(len(next.SearchIDs) != 0 && !slices.Equal(next.SearchIDs, searchIDs)) {
				return ledger.ErrInvalid
			}
			kind, reason = "recovery_empty_pack_done", "empty_pack_search_acknowledged"
		default:
			return ledger.ErrInvalid
		}
		if searchIDs == nil {
			searchIDs = []int64{}
		}
		data, err := json.Marshal(searchIDs)
		if err != nil {
			return err
		}
		values := []any{next.Stage, string(data), next.CommandID, emptyPackStamp(now)}
		values = append(values, key.values()...)
		values = append(values, key.CaseID, expected)
		result, err := tx.ExecContext(ctx, `UPDATE empty_pack_recovery
			SET stage = ?, search_ids = ?, command_id = ?, updated_at = ?
			WHERE `+emptyPackKeyWhere+` AND case_id = ? AND stage = ?`, values...)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil || count == 0 {
			return err
		}
		if next.Stage == EmptyPackDone && checkpoint.manualToken != "" {
			result, err := tx.ExecContext(ctx, `UPDATE manual_reviews SET state = 'completed', updated_at = ?
				WHERE case_id = ? AND token = ? AND state = 'approved'`, emptyPackStamp(now), key.CaseID, checkpoint.manualToken)
			if err != nil {
				return err
			}
			rows, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if rows != 1 {
				return app.ErrConflict
			}
		}
		if err := appendEmptyPackEvent(ctx, tx, key, kind, reason, now); err != nil {
			return err
		}
		wrote = true
		return nil
	})
	return err == nil && wrote, err
}

// CancelUnsent releases a reservation only when the caller can prove the
// transport did not start. Lost or ambiguous responses must keep the fence.
func (j *EmptyPackJournal) CancelUnsent(ctx context.Context, key EmptyPackKey, stage EmptyPackStage, now time.Time) (bool, error) {
	if j == nil || j.local == nil || !key.valid() || now.IsZero() ||
		(stage != EmptyPackRejectSending && stage != EmptyPackSearchSending) {
		return false, ledger.ErrInvalid
	}
	wrote := false
	err := j.local.Write(ctx, func(tx *sql.Tx) error {
		const where = ` WHERE ` + emptyPackKeyWhere + ` AND case_id = ? AND stage = ? AND updated_at <= ?`
		at := emptyPackStamp(now)
		values := append(key.values(), key.CaseID, stage, at)
		query, reason := `DELETE FROM empty_pack_recovery`, domain.ReasonCode("empty_pack_reject_unsent")
		if stage == EmptyPackSearchSending {
			query = `UPDATE empty_pack_recovery SET stage = 'reject_verified', search_ids = '[]', updated_at = ?`
			values = append([]any{at}, values...)
			reason = "empty_pack_search_unsent"
		}
		result, err := tx.ExecContext(ctx, query+where, values...)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil || count == 0 {
			return err
		}
		if err := appendEmptyPackEvent(ctx, tx, key, "recovery_empty_pack_send_cancelled", reason, now); err != nil {
			return err
		}
		wrote = true
		return nil
	})
	return err == nil && wrote, err
}

func (j *EmptyPackJournal) manualSearchScope(ctx context.Context, cp EmptyPackCheckpoint) ([]int64, error) {
	var ids []int64
	err := j.local.Read(ctx, func(tx *sql.Tx) error {
		var err error
		ids, err = manualPackSearchIDs(ctx, tx, cp)
		return err
	})
	return ids, err
}

func manualPackSearchIDs(ctx context.Context, tx *sql.Tx, cp EmptyPackCheckpoint) ([]int64, error) {
	var data []byte
	err := tx.QueryRowContext(ctx, `SELECT search_ids FROM manual_reviews
		WHERE case_id = ? AND token = ? AND state = 'approved'`, cp.Key.CaseID, cp.manualToken).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, app.ErrHeld
	}
	if err != nil {
		return nil, err
	}
	var ids []int64
	if err := json.Unmarshal(data, &ids); err != nil || len(ids) > 0 && !validPackSearchIDs(ids, cp.EpisodeIDs) {
		return nil, ledger.ErrJournal
	}
	return ids, nil
}

func allowEmptyPackSend(ctx context.Context, tx *sql.Tx, instance domain.InstanceRef, revision domain.Revision, manual bool) error {
	mode := domain.ModeAutonomous
	if manual {
		mode = domain.ModeAssist
	}
	var allowed bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM observe_instances AS i, app_state AS s, action_pause AS p
		WHERE i.id = ? AND i.epoch = ? AND i.connection_revision = ? AND i.product = 'sonarr'
		AND i.health = 'available' AND s.mode = ? AND s.reconcile_required = 0 AND p.paused = 0
		AND NOT EXISTS (SELECT 1 FROM action_intents AS intent
			WHERE intent.state NOT IN ('completed', 'cancelled')
			AND json_extract(intent.proposal, '$.Operation.Job.Instance.ID') = i.id)
	)`, instance.ID, instance.Epoch, revision, mode).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return app.ErrHeld
	}
	return nil
}

func loadEmptyPack(ctx context.Context, tx *sql.Tx, key EmptyPackKey) (EmptyPackCheckpoint, error) {
	checkpoint, err := scanEmptyPack(tx.QueryRowContext(ctx, `SELECT `+emptyPackColumns+
		` FROM empty_pack_recovery WHERE `+emptyPackKeyWhere, key.values()...))
	if err == nil && checkpoint.Key != key {
		return EmptyPackCheckpoint{}, app.ErrConflict
	}
	return checkpoint, err
}

func scanEmptyPack(row interface{ Scan(...any) error }) (EmptyPackCheckpoint, error) {
	var checkpoint EmptyPackCheckpoint
	var queue, episodes, search, started string
	instance := &checkpoint.Key.Job.Instance
	err := row.Scan(&checkpoint.Key.CaseID, &instance.ID, &instance.Epoch, &checkpoint.Key.Job.ID,
		&checkpoint.Key.Job.Generation, &checkpoint.Stage, &checkpoint.ConnectionRevision, &checkpoint.Release,
		&checkpoint.DownloadID, &queue, &checkpoint.SeriesID, &episodes, &search, &checkpoint.CommandID, &started, &checkpoint.manualToken)
	if err != nil {
		return EmptyPackCheckpoint{}, err
	}
	for _, field := range []struct {
		payload string
		output  *[]int64
	}{{queue, &checkpoint.QueueIDs}, {episodes, &checkpoint.EpisodeIDs}, {search, &checkpoint.SearchIDs}} {
		if err := json.Unmarshal([]byte(field.payload), field.output); err != nil {
			return EmptyPackCheckpoint{}, ledger.ErrJournal
		}
	}
	checkpoint.StartedAt, err = time.Parse(time.RFC3339Nano, started)
	if err != nil || checkpoint.StartedAt.IsZero() || !checkpoint.Key.valid() || !validEmptyPackIdentity(checkpoint) ||
		(len(checkpoint.SearchIDs) > 0 && !validPackSearchIDs(checkpoint.SearchIDs, checkpoint.EpisodeIDs)) {
		return EmptyPackCheckpoint{}, ledger.ErrJournal
	}
	return checkpoint, nil
}

func validEmptyPackIdentity(checkpoint EmptyPackCheckpoint) bool {
	return checkpoint.ConnectionRevision > 0 && checkpoint.ConnectionRevision <= math.MaxInt64 &&
		emptyPackToken(checkpoint.Release, 300) && emptyPackToken(checkpoint.DownloadID, 256) &&
		checkpoint.SeriesID > 0 && checkpoint.SeriesID <= math.MaxInt32 &&
		validEmptyPackIDs(checkpoint.QueueIDs, int64(math.MaxInt32)) &&
		validEmptyPackIDs(checkpoint.EpisodeIDs, int64(math.MaxInt32))
}

func emptyPackToken(token string, maxLength int) bool {
	return token != "" && len(token) <= maxLength && token == strings.TrimSpace(token) && !strings.ContainsAny(token, "\x00\r\n")
}

func validEmptyPackIDs(ids []int64, maxID int64) bool {
	if len(ids) == 0 || len(ids) > 128 {
		return false
	}
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id < 1 || id > maxID || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

func validPackSearchIDs(searchIDs, episodeIDs []int64) bool {
	if !validEmptyPackIDs(searchIDs, math.MaxInt32) {
		return false
	}
	for _, id := range searchIDs {
		if !slices.Contains(episodeIDs, id) {
			return false
		}
	}
	return true
}

func emptyPackStamp(now time.Time) string {
	return now.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func appendEmptyPackEvent(ctx context.Context, tx *sql.Tx, key EmptyPackKey, kind domain.EventKind, reason domain.ReasonCode, now time.Time) error {
	return store.AppendEvent(ctx, tx, domain.Event{
		ID: domain.EventID(store.NewID()), Kind: kind, Reason: reason, Actor: "rescuarr-auto-empty-pack",
		Scope:  domain.Scope{Kind: domain.ScopeInstance, Instance: key.Job.Instance},
		CaseID: key.CaseID, OccurredAt: now,
	})
}
