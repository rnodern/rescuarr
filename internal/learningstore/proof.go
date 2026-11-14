package learningstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/learning"
)

// Keep only fields used by the deterministic classifier. These are bounded
// local identifiers, typed facts and timestamps. Omit private dispatch data,
// authorization request data, and acknowledgement text from the history.
func sanitise(c learning.Candidate, now time.Time) (learning.Candidate, error) {
	p := c.Plan.Proposal
	c.Plan = domain.Intent{ID: c.Plan.ID, Actor: c.Plan.Actor, CreatedAt: c.Plan.CreatedAt.UTC(), Proposal: domain.Proposal{
		ID: p.ID, CaseID: p.CaseID, CaseRevision: p.CaseRevision, DecisionHash: p.DecisionHash, CapabilityRevision: p.CapabilityRevision,
		PolicyRevision: p.PolicyRevision, Operation: p.Operation, ExpiresAt: p.ExpiresAt.UTC(),
	}}
	c.Attempt.Acknowledgement = domain.Acknowledgement{}
	c.Attempt.Prepared = nil
	c.Attempt.SentAt = c.Attempt.SentAt.UTC()
	c.StartedAt = c.StartedAt.UTC()
	c.Sample.RecordedAt = c.Sample.RecordedAt.UTC()
	c.Sample.Outcome.ObservedAt = c.Sample.Outcome.ObservedAt.UTC()
	c.Sample.Eligible, c.Sample.Exclusion = false, ""
	if !validTime(c.StartedAt) || !validTime(c.Sample.RecordedAt) || !validObservationTime(c.Sample.Outcome.ObservedAt, now) || !validRef(c.Sample.Job.Instance) || !c.Sample.Target.Valid() ||
		!validID(string(c.Sample.Job.ID)) || !validScope(c.Sample.Scope) || !validOutcome(c.Sample.Outcome) || !validAction(c.Sample.Treatment) ||
		!validJob(c.FollowupJob) || !validOperation(p.Operation) || !validOperation(c.Attempt.Operation) ||
		!validIDs(string(c.Plan.ID), string(c.Plan.Actor), string(p.ID), string(p.CaseID), p.DecisionHash, string(c.Attempt.ID), string(c.Attempt.IntentID)) ||
		!validEvidence(c.Attempt.Preflight, 2048) {
		return learning.Candidate{}, learning.ErrInvalid
	}
	switch c.Safety {
	case "", learning.UnexpectedEffect, learning.WrongTarget, learning.NeededFileLost, learning.WorsenedQuality:
	default:
		return learning.Candidate{}, learning.ErrInvalid
	}
	for _, proof := range []*learning.Proof{&c.MediaAssociation, &c.ImportHistory, &c.NoReversalThrough, &c.ReplacementGrab,
		&c.HealthyDependency, &c.IndependentRecovery, &c.SafetyEvidence} {
		proof.Observed = proof.Observed.UTC()
		if !validObservationTime(proof.Observed, now) || !validIDs(string(proof.Evidence)) || !validJob(proof.Job) || !validTarget(proof.Target) || !validScope(proof.Scope) {
			return learning.Candidate{}, learning.ErrInvalid
		}
	}
	return c, nil
}

func validOperation(op domain.Operation) bool {
	if !validIDs(string(op.Key)) || !validJob(op.Job) || !validAction(op.Action) || len(op.Targets) > 256 || len(op.Effects) > 256 {
		return false
	}
	for _, target := range op.Targets {
		if !validTarget(target) {
			return false
		}
	}
	for _, effect := range op.Effects {
		if !validEffect(effect) {
			return false
		}
	}
	return true
}

func validOutcome(outcome domain.Outcome) bool {
	if !validIDs(string(outcome.AttemptID)) || !validVerification(outcome.Verification) || len(outcome.Effects) > 256 {
		return false
	}
	for _, result := range outcome.Effects {
		if !validEffect(result.Effect) || !validVerification(result.Verification) || !validEvidence(result.Evidence, 64) {
			return false
		}
	}
	return true
}

func validEffect(effect domain.Effect) bool {
	switch effect.Kind {
	case domain.EffectLibraryImport, domain.EffectQueueRemoval, domain.EffectClientRemoval, domain.EffectDataRemoval,
		domain.EffectSourceRetained, domain.EffectBlocklist, domain.EffectSearchDispatch, domain.EffectReplacementGrab:
		return validTarget(effect.Target)
	default:
		return false
	}
}

func validVerification(value domain.Verification) bool {
	switch value {
	case "", domain.VerificationEffect, domain.VerificationPartial, domain.VerificationNoEffect, domain.VerificationExternal, domain.VerificationUnresolved:
		return true
	default:
		return false
	}
}

func validAction(action domain.Action) bool {
	return action == "" || action == domain.ActionImport || action == domain.ActionSearch || action == domain.ActionReject
}

func validEvidence(ids []domain.EvidenceID, max int) bool {
	if len(ids) > max {
		return false
	}
	for _, id := range ids {
		if !validID(string(id)) {
			return false
		}
	}
	return true
}

func validRef(ref domain.InstanceRef) bool { return ref.Valid() && validID(string(ref.ID)) }

func validJob(job domain.JobRef) bool {
	return job == (domain.JobRef{}) || validRef(job.Instance) && validID(string(job.ID)) && job.Generation > 0
}

func validTarget(target domain.TargetRef) bool {
	return target == (domain.TargetRef{}) || target.Valid() && validRef(target.Instance)
}

func validScope(scope domain.Scope) bool {
	if scope == (domain.Scope{}) {
		return true
	}
	if !validRef(scope.Instance) {
		return false
	}
	switch scope.Kind {
	case domain.ScopeInstance:
		return scope.Key == ""
	case domain.ScopeClient, domain.ScopeRoot, domain.ScopeVolume:
		return validID(scope.Key)
	default:
		return false
	}
}

func validIDs(ids ...string) bool {
	for _, id := range ids {
		if id != "" && !validID(id) {
			return false
		}
	}
	return true
}

func journalMatches(ctx context.Context, tx *sql.Tx, c learning.Candidate) (bool, error) {
	var proposal, outcome, actor, intent, created, sent, preflight string
	err := tx.QueryRowContext(ctx, `SELECT i.proposal, i.actor, i.id, i.created_at, a.sent_at, a.preflight, a.outcome
		FROM action_attempts a JOIN action_intents i ON i.id = a.intent_id
		WHERE a.id = ? AND i.state = 'completed' AND a.conflicting_readback = 0`, c.Attempt.ID).
		Scan(&proposal, &actor, &intent, &created, &sent, &preflight, &outcome)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var p domain.Proposal
	var o domain.Outcome
	var evidence []domain.EvidenceID
	if json.Unmarshal([]byte(proposal), &p) != nil || json.Unmarshal([]byte(outcome), &o) != nil {
		return false, ErrHistory
	}
	if len(preflight) > 0 && preflight[0] == '[' {
		err = json.Unmarshal([]byte(preflight), &evidence)
	} else {
		var proof struct{ Evidence []domain.EvidenceID }
		err = json.Unmarshal([]byte(preflight), &proof)
		evidence = proof.Evidence
	}
	if err != nil {
		return false, ErrHistory
	}
	o.ObservedAt = o.ObservedAt.UTC()
	plan := c.Plan.Proposal
	return intent == string(c.Plan.ID) && intent == string(c.Attempt.IntentID) && actor == string(c.Plan.Actor) &&
		created == stamp(c.Plan.CreatedAt) && sent == stamp(c.Attempt.SentAt) && slices.Equal(evidence, c.Attempt.Preflight) &&
		p.ID == plan.ID && p.CaseID == plan.CaseID && p.CaseRevision == plan.CaseRevision && p.DecisionHash == plan.DecisionHash &&
		p.PolicyRevision == plan.PolicyRevision && p.CapabilityRevision == plan.CapabilityRevision && p.ExpiresAt.Equal(plan.ExpiresAt) &&
		reflect.DeepEqual(p.Operation, plan.Operation) && reflect.DeepEqual(o, c.Sample.Outcome), nil
}

func validObservationTime(at, now time.Time) bool {
	return at.IsZero() || validTime(at) && !at.After(now)
}
