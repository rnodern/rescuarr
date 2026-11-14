package learningstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/learning"
)

// Record classifies typed proof and stores a bounded projection of it. An
// attributable treatment also has to match an uncontradicted, completed local
// action journal entry. Callers supply independently established supporting
// proof from the corresponding readback contract.
func (s *Store) Record(ctx context.Context, pattern learning.Fingerprint, candidate learning.Candidate, now time.Time) (learning.SampleResult, error) {
	canonical, err := learning.BuildFingerprint(pattern.Signature)
	if err != nil || canonical.ID != pattern.ID || !validTime(now) {
		return learning.SampleResult{}, learning.ErrInvalid
	}
	candidate, err = sanitise(candidate, now)
	if err != nil {
		return learning.SampleResult{}, err
	}
	result := learning.Classify(canonical, candidate, now)
	if result.Reason == "sample_unbound" {
		return learning.SampleResult{}, learning.ErrInvalid
	}
	signature, err := json.Marshal(canonical.Signature)
	if err != nil {
		return learning.SampleResult{}, err
	}
	var saturated bool
	err = s.local.Write(ctx, func(tx *sql.Tx) error {
		if result.Sample.Eligible && canonical.Signature.Family != learning.IncidentCause {
			matched, err := journalMatches(ctx, tx, candidate)
			if err != nil {
				return err
			}
			if !matched {
				candidate.DispatchVerified = false
				result = learning.Classify(canonical, candidate, now)
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO learning_patterns (id, instance_id, signature) VALUES (?, ?, ?)
			ON CONFLICT (id) DO NOTHING`, canonical.ID, canonical.Signature.Epochs.Installation.ID, string(signature))
		if err != nil {
			return err
		}
		key, err := sampleKey(result, candidate)
		if err != nil {
			return err
		}
		body, err := json.Marshal(candidate)
		if err != nil || len(body) > maxCandidateBytes {
			return learning.ErrInvalid
		}
		saturated, err = storeSample(ctx, tx, key, string(body), result, now)
		return err
	})
	if err != nil {
		return learning.SampleResult{}, err
	}
	if saturated {
		return learning.SampleResult{}, ErrCapacity
	}
	return result, nil
}

func storeSample(ctx context.Context, tx *sql.Tx, key, candidate string, result learning.SampleResult, now time.Time) (bool, error) {
	pattern := result.Sample.Pattern
	var found bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM learning_samples WHERE pattern_id = ? AND sample_key = ?)`, pattern, key).Scan(&found); err != nil || found {
		return false, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM learning_samples WHERE pattern_id = ?`, pattern).Scan(&count); err != nil {
		return false, err
	}
	if count >= learning.MaxCandidates {
		// Keep active-window samples and all unreset contradictions. If even
		// that history cannot fit, explicitly hold the pattern rather than
		// allowing a truncated history to create a recommendation.
		pruned, err := tx.ExecContext(ctx, `DELETE FROM learning_samples WHERE pattern_id = ?
			AND ((state != 'contradiction' AND recorded_at <= ?) OR recorded_at <= (SELECT reset_at FROM learning_patterns WHERE id = ?))
			AND sample_key != coalesce((SELECT sample_key FROM learning_samples WHERE pattern_id = ?
				AND state IN ('verified_success', 'verified_failure') ORDER BY recorded_at DESC, sample_key DESC LIMIT 1), '')`,
			pattern, stamp(now.Add(-learning.SampleWindow)), pattern, pattern)
		if err != nil {
			return false, err
		}
		removed, err := pruned.RowsAffected()
		if err != nil {
			return false, err
		}
		if count-int(removed) >= learning.MaxCandidates {
			column := "saturated_at"
			if result.State == learning.SampleContradiction {
				column = "saturated_safety_at"
			}
			_, err := tx.ExecContext(ctx, "UPDATE learning_patterns SET "+column+" = max("+column+", ?) WHERE id = ?", stamp(result.Sample.RecordedAt), pattern)
			return true, err
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO learning_samples (pattern_id, sample_key, state, recorded_at, available_at, candidate)
		VALUES (?, ?, ?, ?, ?, ?)`, pattern, key, result.State, stamp(result.Sample.RecordedAt), stamp(now), candidate)
	return false, err
}

func sampleKey(result learning.SampleResult, candidate learning.Candidate) (string, error) {
	sample := result.Sample
	key := struct {
		Job       domain.JobRef
		Target    domain.TargetRef
		Treatment domain.Action
		Attempt   domain.AttemptID
		State     learning.SampleState
		StartedAt time.Time
		Safety    domain.ReasonCode
		Evidence  domain.EvidenceID
	}{Job: sample.Job, Target: sample.Target, Treatment: sample.Treatment, State: result.State}
	switch result.State {
	case learning.SampleContradiction:
		key.Safety, key.Evidence = result.Reason, candidate.SafetyEvidence.Evidence
	default:
		key.Attempt = sample.Outcome.AttemptID
		if key.Attempt == "" {
			key.StartedAt = candidate.StartedAt
		}
	}
	body, err := json.Marshal(key)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(body)
	return hex.EncodeToString(hash[:]), nil
}

func validTime(at time.Time) bool {
	return !at.IsZero() && at.UTC().Year() >= 1 && at.UTC().Year() <= 9999
}
