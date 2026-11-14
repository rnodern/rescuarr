package learning

import (
	"cmp"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
)

const (
	MinimumSuccesses = 5
	MinimumDays      = 3
	FailureWindow    = 7 * 24 * time.Hour
	MaxCandidates    = 4096
)

type Status string

const (
	Collecting  Status = "collecting"
	Recommended Status = "recommended"
	Suspended   Status = "suspended"
	Quarantined Status = "quarantined"
	Stale       Status = "stale"
	Invalidated Status = "invalidated"
	Paused      Status = "paused"
)

type Controls struct {
	Revision domain.Revision
	Paused   bool
	ResetAt  time.Time
}

type Input struct {
	Pattern     Fingerprint
	Current     Epochs
	Controls    Controls
	Candidates  []Candidate
	Now         time.Time
	LastMatchAt time.Time
}

type Counts struct {
	Successes      int
	Failures       int
	Neutral        int
	Excluded       int
	DistinctDays   int
	RecentFailures int
	Contradictions int
}

type View struct {
	Pattern        Fingerprint
	Controls       Controls
	Status         Status
	Explanation    domain.ReasonCode
	Counts         Counts
	FirstSampleAt  time.Time
	LastSampleAt   time.Time
	LastEligibleAt time.Time
	FirstVerified  time.Time
	LastVerified   time.Time
	LastMatchAt    time.Time
	Samples        []SampleResult
}

// Evaluate rebuilds a pattern from a bounded local history. Passing the same
// frozen inputs after process restart reproduces the same view and decision.
func Evaluate(input Input) (View, error) {
	fingerprint, err := BuildFingerprint(input.Pattern.Signature)
	if err != nil || fingerprint.ID != input.Pattern.ID || input.Now.IsZero() || input.Controls.ResetAt.After(input.Now) ||
		!validEpochs(input.Current) || len(input.Candidates) > MaxCandidates || input.LastMatchAt.After(input.Now) {
		return View{}, ErrInvalid
	}
	view := View{Pattern: fingerprint, Controls: input.Controls, LastMatchAt: input.LastMatchAt.UTC()}
	for _, candidate := range input.Candidates {
		row := classify(fingerprint, candidate, input.Now)
		// Keep only samples actually attributable to the evaluated fingerprint in this view.
		if row.Reason != "sample_unbound" {
			view.Samples = append(view.Samples, row)
		} else {
			view.Counts.Excluded++
		}
	}
	slices.SortFunc(view.Samples, compareSamples)
	countSamples(&view, input.Now)
	view.selectStatus(input)
	return view, nil
}

func countSamples(view *View, now time.Time) {
	first := now.Add(-SampleWindow)
	recent := now.Add(-FailureWindow)
	days := make(map[string]bool)
	lineage := make(map[domain.TargetRef]bool)
	jobs := make(map[domain.JobRef]bool)
	attempts := make(map[domain.AttemptID]bool)
	type contradiction struct {
		job    domain.JobRef
		target domain.TargetRef
		reason domain.ReasonCode
	}
	contradictions := make(map[contradiction]bool)
	for i := range view.Samples {
		row := &view.Samples[i]
		sample := row.Sample
		at := sample.RecordedAt.UTC()
		if view.FirstSampleAt.IsZero() || at.Before(view.FirstSampleAt) {
			view.FirstSampleAt = at
		}
		if at.After(view.LastSampleAt) {
			view.LastSampleAt = at
		}
		if !view.Controls.ResetAt.IsZero() && !at.After(view.Controls.ResetAt) {
			*row = row.excluding("sample_before_reset")
			view.Counts.Excluded++
			continue
		}
		if row.State == SampleContradiction {
			key := contradiction{job: sample.Job, target: sample.Target, reason: row.Reason}
			if contradictions[key] {
				*row = row.excluding("duplicate_safety_finding")
				view.Counts.Excluded++
			} else {
				contradictions[key] = true
				view.Counts.Contradictions++
			}
			continue
		}
		if !sample.Eligible {
			view.Counts.Neutral++
			continue
		}
		if at.After(view.LastEligibleAt) {
			view.LastEligibleAt = at
		}
		if !at.After(first) {
			*row = row.excluding("sample_window_expired")
			view.Counts.Excluded++
			continue
		}
		attemptID := sample.Outcome.AttemptID
		if lineage[sample.Target] || jobs[sample.Job] || (attemptID != "" && attempts[attemptID]) {
			*row = row.excluding("duplicate_recovery_lineage")
			view.Counts.Excluded++
			continue
		}
		lineage[sample.Target], jobs[sample.Job] = true, true
		if attemptID != "" {
			attempts[attemptID] = true
		}
		if view.LastVerified.IsZero() || at.After(view.LastVerified) {
			view.LastVerified = at
		}
		if view.FirstVerified.IsZero() || at.Before(view.FirstVerified) {
			view.FirstVerified = at
		}
		if row.State == SampleSuccess {
			view.Counts.Successes++
			days[at.Format("2006-01-02")] = true
		} else {
			view.Counts.Failures++
			if !at.Before(recent) {
				view.Counts.RecentFailures++
			}
		}
	}
	view.Counts.DistinctDays = len(days)
}

func compareSamples(x, y SampleResult) int {
	a, b := x.Sample, y.Sample
	// Prefer the most recent observation for a lineage. On a tie prefer failure
	// over success, then stable identifiers for deterministic replay.
	order := cmp.Or(b.RecordedAt.Compare(a.RecordedAt), cmp.Compare(sampleRank(x.State), sampleRank(y.State)),
		cmp.Compare(a.Job.ID, b.Job.ID), cmp.Compare(a.Job.Generation, b.Job.Generation), cmp.Compare(a.Target.Kind, b.Target.Kind),
		cmp.Compare(a.Target.SeriesID, b.Target.SeriesID), cmp.Compare(a.Target.ID, b.Target.ID), cmp.Compare(a.Outcome.AttemptID, b.Outcome.AttemptID), cmp.Compare(x.Reason, y.Reason))
	if order != 0 {
		return order
	}
	left, _ := json.Marshal(x)
	right, _ := json.Marshal(y)
	return cmp.Compare(string(left), string(right))
}

func sampleRank(state SampleState) int {
	switch state {
	case SampleContradiction:
		return 0
	case SampleFailure:
		return 1
	case SampleSuccess:
		return 2
	default:
		return 3
	}
}

func (v *View) selectStatus(input Input) {
	c := v.Counts
	switch {
	case input.Current != v.Pattern.Signature.Epochs:
		v.Status, v.Explanation = Invalidated, "learning_epoch_changed"
	case c.Contradictions > 0:
		v.Status, v.Explanation = Quarantined, "learning_safety_contradiction"
	case input.Controls.Paused:
		v.Status, v.Explanation = Paused, "learning_paused"
	case c.RecentFailures >= 2:
		v.Status, v.Explanation = Suspended, "learning_recent_failures"
	case c.Successes+c.Failures == 0 && !v.LastEligibleAt.IsZero():
		v.Status, v.Explanation = Stale, "learning_no_current_verified_samples"
	case c.Successes >= MinimumSuccesses && c.DistinctDays >= MinimumDays && 10*c.Successes >= 9*(c.Successes+c.Failures):
		v.Status, v.Explanation = Recommended, "learning_threshold_met"
	default:
		v.Status, v.Explanation = Collecting, "learning_threshold_not_met"
	}
}

type Change string

const (
	Pause  Change = "pause"
	Resume Change = "resume"
	Reset  Change = "reset"
)

var ErrConflict = errors.New("learning revision conflict")

type Command struct {
	ExpectedRevision domain.Revision
	Change           Change
	Actor            domain.ActorID
	Reason           domain.ReasonCode
	Event            domain.EventID
	At               time.Time
}

type Audit struct {
	Pattern domain.PatternID
	Change  Change
	Event   domain.Event
}

// Apply changes local controls. Persist the returned controls and audit together
// with a revision check. No stored sample needs to be discarded on reset.
func Apply(pattern Fingerprint, current Controls, command Command) (Controls, Audit, error) {
	if command.ExpectedRevision != current.Revision {
		return current, Audit{}, ErrConflict
	}
	checked, err := BuildFingerprint(pattern.Signature)
	if err != nil || checked.ID != pattern.ID || command.Actor == "" || command.Event == "" || command.At.IsZero() ||
		command.At.Before(current.ResetAt) || current.Revision == ^domain.Revision(0) || !allowedControlReason(command.Reason) {
		return current, Audit{}, ErrInvalid
	}
	updated := current
	switch command.Change {
	case Pause:
		updated.Paused = true
	case Resume:
		updated.Paused = false
	case Reset:
		updated.ResetAt = command.At.UTC()
	default:
		return current, Audit{}, ErrInvalid
	}
	updated.Revision++
	return updated, Audit{Pattern: pattern.ID, Change: command.Change, Event: domain.Event{
		ID: command.Event, Kind: domain.EventKind("learning_" + command.Change), Actor: command.Actor,
		Scope:  domain.Scope{Kind: domain.ScopeInstance, Instance: pattern.Signature.Epochs.Installation},
		Reason: command.Reason, OccurredAt: command.At.UTC(),
	}}, nil
}

func allowedControlReason(reason domain.ReasonCode) bool {
	switch reason {
	case "operator_request", "wrong_identification", "missing_evidence", "upstream_changed", "treatment_failed", "retention_changed":
		return true
	default:
		return false
	}
}
