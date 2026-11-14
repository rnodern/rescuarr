package ledger

import (
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
)

func validOperation(op domain.Operation) bool {
	if op.Key == "" || !op.Job.Instance.Valid() || op.Job.ID == "" || op.Job.Generation == 0 || len(op.Targets) == 0 || len(op.Targets) > 128 || len(op.Effects) == 0 || len(op.Effects) > 256 {
		return false
	}
	targets := make(map[domain.TargetRef]bool)
	for _, target := range op.Targets {
		if !target.Valid() || target.Instance != op.Job.Instance || targets[target] {
			return false
		}
		targets[target] = true
	}
	effects := make(map[domain.Effect]bool)
	for _, effect := range op.Effects {
		if effects[effect] || !effectPermitted(op.Action, effect, targets) {
			return false
		}
		effects[effect] = true
	}
	return true
}

func effectPermitted(action domain.Action, effect domain.Effect, targets map[domain.TargetRef]bool) bool {
	switch effect.Kind {
	case domain.EffectLibraryImport:
		return action == domain.ActionImport && targets[effect.Target]
	case domain.EffectSourceRetained:
		return (action == domain.ActionImport || action == domain.ActionReject) && (effect.Target == (domain.TargetRef{}) || targets[effect.Target])
	case domain.EffectQueueRemoval, domain.EffectClientRemoval, domain.EffectDataRemoval, domain.EffectBlocklist:
		return action == domain.ActionReject && (effect.Target == (domain.TargetRef{}) || targets[effect.Target])
	case domain.EffectSearchDispatch, domain.EffectReplacementGrab:
		return action == domain.ActionSearch && targets[effect.Target]
	default:
		return false
	}
}

// Never trust an aggregate acknowledgement as proof. Use only exact, individually
// evidenced effects. Missing, foreign, stale or malformed results keep the scope held.
func normaliseOutcome(attempt domain.Attempt, observed domain.Outcome, now time.Time) domain.Outcome {
	out := domain.Outcome{AttemptID: attempt.ID, ObservedAt: now, Verification: domain.VerificationUnresolved}
	expected := make(map[domain.Effect]bool)
	for _, effect := range attempt.Operation.Effects {
		expected[effect] = true
	}
	verified := make(map[domain.Effect]domain.EffectResult)
	valid := observed.AttemptID == attempt.ID && !observed.ObservedAt.Before(attempt.SentAt) && !observed.ObservedAt.After(now) && len(observed.Effects) <= len(expected)
	if valid {
		for _, result := range observed.Effects {
			if !expected[result.Effect] || verified[result.Effect].Effect != (domain.Effect{}) {
				clear(verified)
				break
			}
			verified[result.Effect] = normaliseEffect(result)
		}
	}
	counts := make(map[domain.Verification]int)
	for _, effect := range attempt.Operation.Effects {
		result, found := verified[effect]
		if !found {
			result = domain.EffectResult{Effect: effect, Verification: domain.VerificationUnresolved}
		}
		counts[result.Verification]++
		out.Effects = append(out.Effects, result)
	}
	if len(counts) == 1 {
		for kind := range counts {
			out.Verification = kind
		}
	} else if counts[domain.VerificationEffect]+counts[domain.VerificationExternal]+counts[domain.VerificationPartial] > 0 {
		out.Verification = domain.VerificationPartial
	}
	return out
}

func normaliseEffect(result domain.EffectResult) domain.EffectResult {
	switch result.Verification {
	case domain.VerificationEffect, domain.VerificationNoEffect, domain.VerificationPartial, domain.VerificationExternal:
		if len(result.Evidence) > 0 && len(result.Evidence) <= maxPreflightFacts {
			for _, id := range result.Evidence {
				if id == "" {
					return domain.EffectResult{Effect: result.Effect, Verification: domain.VerificationUnresolved}
				}
			}
			return result
		}
	}
	return domain.EffectResult{Effect: result.Effect, Verification: domain.VerificationUnresolved}
}
