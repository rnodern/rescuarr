package learningstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/learning"
)

// View rebuilds the explainable pattern detail using current epochs and an
// explicit clock while reading a consistent SQLite snapshot.
func (s *Store) View(ctx context.Context, id domain.PatternID, current learning.Epochs, now time.Time) (learning.View, error) {
	if !validPatternID(id) || !validEpochs(current) || !validTime(now) {
		return learning.View{}, learning.ErrInvalid
	}
	var view learning.View
	err := s.local.Read(ctx, func(tx *sql.Tx) error {
		p, err := readPattern(ctx, tx, id)
		if err != nil {
			return err
		}
		reconcile, err := restoreHold(ctx, tx)
		if err != nil {
			return err
		}
		view, err = evaluate(ctx, tx, p, current, now, reconcile)
		return err
	})
	return view, err
}

// List returns at most limit pattern summaries for the instance, ordered by
// ID. Pass the last returned ID as after to fetch the next page.
func (s *Store) List(ctx context.Context, current learning.Epochs, now time.Time, after domain.PatternID, limit int) ([]learning.View, error) {
	if !validEpochs(current) || !validTime(now) || (after != "" && !validPatternID(after)) || limit <= 0 || limit > maxPage {
		return nil, learning.ErrInvalid
	}
	var views []learning.View
	err := s.local.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT "+patternColumns+" FROM learning_patterns WHERE instance_id = ? AND id > ? ORDER BY id LIMIT ?",
			current.Installation.ID, after, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		var patterns []patternRecord
		for rows.Next() {
			p, err := scanPattern(rows)
			if err != nil {
				return err
			}
			patterns = append(patterns, p)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		reconcile, err := restoreHold(ctx, tx)
		if err != nil {
			return err
		}
		for _, p := range patterns {
			view, err := evaluate(ctx, tx, p, current, now, reconcile)
			if err != nil {
				return err
			}
			view.Samples = nil
			views = append(views, view)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return views, nil
}

func restoreHold(ctx context.Context, tx *sql.Tx) (bool, error) {
	var held bool
	err := tx.QueryRowContext(ctx, `SELECT reconcile_required FROM app_state WHERE singleton = 1`).Scan(&held)
	return held, err
}

func evaluate(ctx context.Context, tx *sql.Tx, p patternRecord, current learning.Epochs, now time.Time, reconcile bool) (learning.View, error) {
	if current.Installation.ID != p.pattern.Signature.Epochs.Installation.ID {
		return learning.View{}, ErrNotFound
	}
	if p.changed > stamp(now) {
		return learning.View{}, learning.ErrInvalid
	}
	rows, err := tx.QueryContext(ctx, `SELECT candidate FROM learning_samples WHERE pattern_id = ? AND available_at <= ? LIMIT ?`,
		p.id, stamp(now), learning.MaxCandidates+1)
	if err != nil {
		return learning.View{}, err
	}
	defer rows.Close()
	var candidates []learning.Candidate
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			return learning.View{}, err
		}
		var candidate learning.Candidate
		if len(encoded) > maxCandidateBytes || len(candidates) >= learning.MaxCandidates || json.Unmarshal([]byte(encoded), &candidate) != nil {
			return learning.View{}, ErrHistory
		}
		gateTreatmentAttribution(p.pattern.Signature.Family, &candidate)
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return learning.View{}, err
	}
	view, err := learning.Evaluate(learning.Input{Pattern: p.pattern, Current: current, Controls: p.controls, Candidates: candidates, Now: now, LastMatchAt: p.lastMatch})
	if err != nil {
		return learning.View{}, err
	}
	reset := ""
	if !p.controls.ResetAt.IsZero() {
		reset = stamp(p.controls.ResetAt)
	}
	if view.Status != learning.Invalidated && p.safety > reset && p.safety <= stamp(now) {
		view.Status, view.Explanation = learning.Quarantined, "learning_safety_contradiction"
	}
	if view.Status == learning.Collecting && p.pattern.Signature.Family == learning.OriginalImport {
		view.Explanation = "learning_dispatch_fingerprint_unverified"
	}
	if view.Status == learning.Recommended {
		switch {
		case reconcile:
			view.Status, view.Explanation = learning.Suspended, "learning_restore_reconciliation_required"
		case p.saturated > stamp(now.Add(-learning.SampleWindow)) && p.saturated > reset && p.saturated <= stamp(now):
			view.Status, view.Explanation = learning.Suspended, "learning_history_capacity_exceeded"
		}
	}
	return view, nil
}

// NoteMatch stores the latest use of an exact fingerprint. It does not count as
// an observation or change the operator control revision.
func (s *Store) NoteMatch(ctx context.Context, id domain.PatternID, at time.Time) error {
	if !validPatternID(id) || !validTime(at) {
		return learning.ErrInvalid
	}
	return s.local.Write(ctx, func(tx *sql.Tx) error {
		updated, err := tx.ExecContext(ctx, `UPDATE learning_patterns SET last_match_at = max(last_match_at, ?) WHERE id = ?`, stamp(at), id)
		if err != nil {
			return err
		}
		count, err := updated.RowsAffected()
		if err == nil && count != 1 {
			return ErrNotFound
		}
		return err
	})
}

// Snapshot pins exact pattern status for replay in a single read transaction.
// The value can be reused after controls or evidence change.
type Snapshot struct {
	at       time.Time
	patterns []learning.View
}

func (s *Store) Snapshot(ctx context.Context, ids []domain.PatternID, current learning.Epochs, at time.Time) (Snapshot, error) {
	if !validTime(at) || !validEpochs(current) || len(ids) > learning.MaxReplayCases {
		return Snapshot{}, learning.ErrInvalid
	}
	ids = slices.Clone(ids)
	slices.Sort(ids)
	result := Snapshot{at: at.UTC()}
	err := s.local.Read(ctx, func(tx *sql.Tx) error {
		reconcile, err := restoreHold(ctx, tx)
		if err != nil {
			return err
		}
		for index, id := range ids {
			if !validPatternID(id) || index > 0 && id == ids[index-1] {
				return learning.ErrInvalid
			}
			p, err := readPattern(ctx, tx, id)
			if err != nil {
				return err
			}
			view, err := evaluate(ctx, tx, p, current, at, reconcile)
			if err != nil {
				return err
			}
			view.Samples = nil
			result.patterns = append(result.patterns, view)
		}
		return nil
	})
	if err != nil {
		return Snapshot{}, err
	}
	return result, nil
}

// Replay runs a pure evaluator against caller-frozen, sanitised case and policy
// evidence and the captured learning snapshot. It has no SQLite or transport.
func Replay(ctx context.Context, snapshot Snapshot, cases []learning.FrozenCase, before, after learning.PolicySnapshot, evaluator learning.Evaluator) (learning.ReplayReport, error) {
	if len(cases) > learning.MaxReplayCases {
		return learning.ReplayReport{}, learning.ErrInvalid
	}
	for _, item := range cases {
		if !validID(string(item.ID)) || item.Fingerprint != "" && !validPatternID(item.Fingerprint) {
			return learning.ReplayReport{}, learning.ErrInvalid
		}
	}
	return learning.Replay(ctx, cases, before, after, snapshot.at, evaluator, snapshot.patterns)
}

func validEpochs(epochs learning.Epochs) bool {
	return validRef(epochs.Installation) && epochs.Adapter > 0 && epochs.Config > 0 && epochs.Policy > 0
}
