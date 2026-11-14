package learning

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
)

const (
	MaxReplayCases = 100
	MaxFrozenBytes = 1 << 20
)

type PolicySnapshot struct {
	Revision domain.Revision
	Frozen   json.RawMessage
}

type FrozenCase struct {
	ID          domain.CaseID
	Fingerprint domain.PatternID
	Evidence    json.RawMessage
	Complete    bool
}

type Prediction struct {
	State          domain.CaseState
	Reason         domain.ReasonCode
	AlreadyAllowed []domain.Action
	Holds          []domain.ReasonCode
}

// Evaluator receives only frozen snapshots and explicit time. Wire a pure
// deterministic policy evaluator here, with no network or dispatch dependency.
type Evaluator func(FrozenCase, PolicySnapshot, time.Time) (Prediction, error)

type ReplayRow struct {
	Case    domain.CaseID
	Before  Prediction
	After   Prediction
	Pattern domain.PatternID
	Skipped domain.ReasonCode
}

type ReplayReport struct {
	At             time.Time
	BeforeRevision domain.Revision
	AfterRevision  domain.Revision
	Total          int
	Evaluated      int
	Rows           []ReplayRow
}

func Replay(ctx context.Context, frozen []FrozenCase, before, after PolicySnapshot, now time.Time, evaluator Evaluator, patterns []View) (ReplayReport, error) {
	if evaluator == nil || len(frozen) > MaxReplayCases || len(patterns) > MaxReplayCases || now.IsZero() ||
		before.Revision == 0 || after.Revision == 0 || !validJSON(before.Frozen) || !validJSON(after.Frozen) {
		return ReplayReport{}, ErrInvalid
	}
	usedBytes := len(before.Frozen) + len(after.Frozen)
	if usedBytes > MaxFrozenBytes {
		return ReplayReport{}, ErrInvalid
	}
	ordered := slices.Clone(frozen)
	slices.SortFunc(ordered, func(a, b FrozenCase) int { return cmp.Compare(a.ID, b.ID) })
	report := ReplayReport{At: now.UTC(), BeforeRevision: before.Revision, AfterRevision: after.Revision, Total: len(ordered)}
	for index, item := range ordered {
		if err := ctx.Err(); err != nil {
			return ReplayReport{}, err
		}
		usedBytes += len(item.Evidence)
		if usedBytes > MaxFrozenBytes || item.ID == "" || (index > 0 && ordered[index-1].ID == item.ID) {
			return ReplayReport{}, ErrInvalid
		}
		row := ReplayRow{Case: item.ID}
		if !item.Complete || !validJSON(item.Evidence) {
			row.Skipped = "replay_evidence_incomplete"
		} else {
			var errBefore, errAfter error
			row.Before, errBefore = predict(evaluator, item, before, now)
			row.After, errAfter = predict(evaluator, item, after, now)
			if errBefore != nil || errAfter != nil {
				row.Before, row.After, row.Skipped = Prediction{}, Prediction{}, "replay_evaluation_failed"
			} else {
				report.Evaluated++
				if view, found := matchingPattern(patterns, item.Fingerprint); found && Recommends(view, row.After) {
					row.Pattern = view.Pattern.ID
				}
			}
		}
		report.Rows = append(report.Rows, row)
	}
	return report, ctx.Err()
}

func validJSON(encoded json.RawMessage) bool {
	return len(encoded) > 0 && len(encoded) <= MaxFrozenBytes && json.Valid(encoded)
}

func predict(evaluator Evaluator, frozen FrozenCase, policy PolicySnapshot, now time.Time) (Prediction, error) {
	frozen.Evidence, policy.Frozen = slices.Clone(frozen.Evidence), slices.Clone(policy.Frozen)
	prediction, err := evaluator(frozen, policy, now.UTC())
	prediction.Holds = slices.Clone(prediction.Holds)
	prediction.AlreadyAllowed = slices.Clone(prediction.AlreadyAllowed)
	slices.Sort(prediction.Holds)
	slices.Sort(prediction.AlreadyAllowed)
	prediction.Holds, prediction.AlreadyAllowed = slices.Compact(prediction.Holds), slices.Compact(prediction.AlreadyAllowed)
	return prediction, err
}

func matchingPattern(patterns []View, id domain.PatternID) (View, bool) {
	index := slices.IndexFunc(patterns, func(view View) bool { return view.Pattern.ID == id })
	if index < 0 || slices.ContainsFunc(patterns[index+1:], func(view View) bool { return view.Pattern.ID == id }) {
		return View{}, false
	}
	return patterns[index], true
}

// Recommends reports only treatments independently permitted by the supplied
// policy decision. Incident patterns can prioritise a diagnostic read only.
func Recommends(view View, predicted Prediction) bool {
	if view.Status != Recommended || len(predicted.Holds) > 0 || predicted.Reason != view.Pattern.Signature.Reason {
		return false
	}
	switch view.Pattern.Signature.Family {
	case IncidentCause:
		return true
	case OriginalImport:
		return slices.Contains(predicted.AlreadyAllowed, domain.ActionImport)
	case Replacement:
		return slices.Contains(predicted.AlreadyAllowed, domain.ActionSearch)
	default:
		return false
	}
}
