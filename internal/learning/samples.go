package learning

import (
	"slices"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
)

const (
	ImportVerification = 30 * time.Minute
	ImportStability    = 24 * time.Hour
	ReplacementWindow  = 7 * 24 * time.Hour
	SampleWindow       = 30 * 24 * time.Hour
)

// Classify keeps unsuccessful or ambiguous evidence visible without admitting
// it as attributable success. The caller persists the source proof separately.
func Classify(pattern Fingerprint, candidate Candidate, now time.Time) SampleResult {
	result := SampleResult{Sample: candidate.Sample, State: SampleNeutral}
	fingerprint, err := BuildFingerprint(pattern.Signature)
	if err != nil || fingerprint.ID != pattern.ID {
		return result.excluding("sample_unbound")
	}
	return classify(fingerprint, candidate, now)
}

func classify(pattern Fingerprint, candidate Candidate, now time.Time) SampleResult {
	result := SampleResult{Sample: candidate.Sample, State: SampleNeutral}
	result.Sample.Eligible = false
	if !validSample(pattern, candidate, now) {
		return result.excluding("sample_unbound")
	}
	if validSafety(candidate.Safety) && proofInWindow(candidate.SafetyEvidence, candidate.StartedAt, now) &&
		proofTargets(candidate.SafetyEvidence, candidate.Sample.Job, candidate.Sample.Target) {
		result.State, result.Reason = SampleContradiction, domain.ReasonCode(candidate.Safety)
		result.Sample.Exclusion = result.Reason
		result.Sample.RecordedAt = minTime(candidate.Sample.RecordedAt, candidate.SafetyEvidence.Observed)
		return result
	}
	if candidate.Override {
		return result.excluding("operator_override")
	}
	if !candidate.IdentityVerified {
		return result.excluding("identity_unverified")
	}
	if pattern.Signature.Family == IncidentCause {
		return classifyIncident(result, candidate, now)
	}
	if !boundPlan(pattern, candidate, now) {
		return result.excluding("plan_unverified")
	}
	outcome := candidate.Sample.Outcome
	if outcome.Verification == domain.VerificationExternal || outcome.Verification == domain.VerificationUnresolved {
		return result.excluding("outcome_unattributed")
	}
	// Anchor sample age and UTC-day counting to the durable outcome observation,
	// never to the later poll which happened to establish supporting evidence.
	result.Sample.RecordedAt = minTime(candidate.Sample.RecordedAt, outcome.ObservedAt)
	switch outcome.Verification {
	case domain.VerificationPartial, domain.VerificationNoEffect:
		if !verifiedFailure(candidate.Plan.Proposal.Operation, outcome) {
			return result.excluding("outcome_unattributed")
		}
		return result.including(SampleFailure)
	case domain.VerificationEffect:
		if !allEffects(candidate.Plan.Proposal.Operation, outcome) {
			return result.excluding("effects_unverified")
		}
	default:
		return result.excluding("outcome_unattributed")
	}
	if pattern.Signature.Family == OriginalImport {
		verifiedBy := candidate.StartedAt.Add(ImportVerification)
		if outcome.ObservedAt.After(verifiedBy) || !separateProof(candidate.MediaAssociation, candidate.ImportHistory,
			candidate.StartedAt, outcome.ObservedAt) || !proofTargets(candidate.MediaAssociation, candidate.Sample.Job, candidate.Sample.Target) ||
			!proofTargets(candidate.ImportHistory, candidate.Sample.Job, candidate.Sample.Target) ||
			!hasEffect(outcome, domain.EffectLibraryImport, candidate.Sample.Target, candidate.MediaAssociation.Evidence) {
			return result.excluding("import_evidence_missing")
		}
		stableFrom := maxTime(candidate.MediaAssociation.Observed, candidate.ImportHistory.Observed).Add(ImportStability)
		if !proofInWindow(candidate.NoReversalThrough, stableFrom, now) ||
			!proofTargets(candidate.NoReversalThrough, candidate.Sample.Job, candidate.Sample.Target) ||
			candidate.NoReversalThrough.Evidence == candidate.MediaAssociation.Evidence ||
			candidate.NoReversalThrough.Evidence == candidate.ImportHistory.Evidence {
			return result.excluding("import_observation_pending")
		}
		return result.including(SampleSuccess)
	}
	if !validJob(candidate.FollowupJob, candidate.Sample.Job.Instance) || candidate.FollowupJob == candidate.Sample.Job ||
		!separateProof(candidate.ReplacementGrab, candidate.MediaAssociation, outcome.ObservedAt, now) ||
		!proofTargets(candidate.ReplacementGrab, candidate.FollowupJob, candidate.Sample.Target) ||
		!hasEffect(outcome, domain.EffectSearchDispatch, candidate.Sample.Target, candidate.ReplacementGrab.Evidence) ||
		candidate.ReplacementGrab.Observed.Before(outcome.ObservedAt) ||
		!proofTargets(candidate.MediaAssociation, candidate.FollowupJob, candidate.Sample.Target) ||
		candidate.MediaAssociation.Observed.Before(candidate.ReplacementGrab.Observed) ||
		candidate.MediaAssociation.Observed.After(candidate.StartedAt.Add(ReplacementWindow)) ||
		!separateProof(candidate.MediaAssociation, candidate.ImportHistory, candidate.ReplacementGrab.Observed, now) ||
		!proofTargets(candidate.ImportHistory, candidate.FollowupJob, candidate.Sample.Target) ||
		candidate.ImportHistory.Observed.After(candidate.StartedAt.Add(ReplacementWindow)) {
		return result.excluding("replacement_evidence_missing")
	}
	return result.including(SampleSuccess)
}

func validSample(pattern Fingerprint, c Candidate, now time.Time) bool {
	sample := c.Sample
	return sample.Pattern == pattern.ID && validJob(sample.Job, pattern.Signature.Epochs.Installation) &&
		sample.Target.Valid() && sample.Target.Instance == sample.Job.Instance &&
		((pattern.Signature.Product == domain.ProductSonarr && sample.Target.Kind == domain.TargetEpisode) ||
			(pattern.Signature.Product == domain.ProductRadarr && sample.Target.Kind == domain.TargetMovie)) &&
		validCausalScope(sample.Scope, sample.Job.Instance) && (pattern.Signature.CausalScope == (domain.Scope{}) || sample.Scope == pattern.Signature.CausalScope) &&
		!now.IsZero() && !c.StartedAt.IsZero() && !sample.RecordedAt.Before(c.StartedAt) && !sample.RecordedAt.After(now)
}

func validJob(job domain.JobRef, instance domain.InstanceRef) bool {
	return job.Instance == instance && job.ID != "" && job.Generation > 0
}

func classifyIncident(result SampleResult, c Candidate, now time.Time) SampleResult {
	if c.Sample.Treatment != "" || c.Sample.Outcome.AttemptID != "" || !validJob(c.FollowupJob, c.Sample.Job.Instance) || c.FollowupJob == c.Sample.Job ||
		!separateProof(c.HealthyDependency, c.IndependentRecovery, c.StartedAt, now) ||
		c.HealthyDependency.Scope != c.Sample.Scope || c.IndependentRecovery.Scope != c.Sample.Scope ||
		!proofTargets(c.IndependentRecovery, c.FollowupJob, c.Sample.Target) ||
		c.IndependentRecovery.Observed.Before(c.HealthyDependency.Observed) {
		return result.excluding("incident_resolution_unverified")
	}
	result.Sample.RecordedAt = minTime(c.Sample.RecordedAt, c.IndependentRecovery.Observed)
	return result.including(SampleSuccess)
}

func validSafety(finding SafetyFinding) bool {
	switch finding {
	case UnexpectedEffect, WrongTarget, NeededFileLost, WorsenedQuality:
		return true
	default:
		return false
	}
}

func boundPlan(pattern Fingerprint, c Candidate, now time.Time) bool {
	sample, plan := c.Sample, c.Plan
	proposal := plan.Proposal
	op := proposal.Operation
	outcome := sample.Outcome
	attempt := c.Attempt
	return c.QualifiedPlan && c.DispatchVerified && plan.Actor != "" && plan.ID != "" && proposal.ID != "" && proposal.CaseID != "" &&
		proposal.CaseRevision > 0 && proposal.DecisionHash != "" && proposal.CapabilityRevision > 0 && proposal.PolicyRevision > 0 &&
		!plan.CreatedAt.IsZero() && !plan.CreatedAt.After(c.StartedAt) && c.StartedAt.Before(proposal.ExpiresAt) &&
		outcome.AttemptID != "" && attempt.ID == outcome.AttemptID && attempt.IntentID == plan.ID && attempt.SentAt.Equal(c.StartedAt) &&
		sameOperation(attempt.Operation, op) && evidencedWithin(attempt.Preflight, 2048) && !outcome.ObservedAt.Before(c.StartedAt) && !outcome.ObservedAt.After(sample.RecordedAt) &&
		!outcome.ObservedAt.After(now) && op.Job == sample.Job && slices.Contains(op.Targets, sample.Target) &&
		sample.Treatment == op.Action && len(op.Effects) > 0 && len(op.Effects) <= 256 &&
		((pattern.Signature.Family == OriginalImport && op.Action == domain.ActionImport &&
			slices.Contains(op.Effects, domain.Effect{Kind: domain.EffectLibraryImport, Target: sample.Target}) &&
			slices.Contains(op.Effects, domain.Effect{Kind: domain.EffectSourceRetained})) ||
			(pattern.Signature.Family == Replacement && op.Action == domain.ActionSearch &&
				slices.Contains(op.Effects, domain.Effect{Kind: domain.EffectSearchDispatch, Target: sample.Target})))
}

func sameOperation(a, b domain.Operation) bool {
	return a.Key != "" && a.Key == b.Key && a.Job == b.Job && a.Action == b.Action &&
		slices.Equal(a.Targets, b.Targets) && slices.Equal(a.Effects, b.Effects)
}

func allEffects(op domain.Operation, outcome domain.Outcome) bool {
	if len(outcome.Effects) != len(op.Effects) {
		return false
	}
	seen := make(map[domain.Effect]bool, len(op.Effects))
	for _, effect := range outcome.Effects {
		if !slices.Contains(op.Effects, effect.Effect) || seen[effect.Effect] ||
			effect.Verification != domain.VerificationEffect || !evidenced(effect.Evidence) {
			return false
		}
		seen[effect.Effect] = true
	}
	return true
}

func verifiedFailure(op domain.Operation, outcome domain.Outcome) bool {
	if len(outcome.Effects) == 0 || len(outcome.Effects) > len(op.Effects) {
		return false
	}
	seen := make(map[domain.Effect]bool, len(outcome.Effects))
	failure := false
	for _, effect := range outcome.Effects {
		if seen[effect.Effect] || !slices.Contains(op.Effects, effect.Effect) {
			return false
		}
		seen[effect.Effect] = true
		if (effect.Verification == domain.VerificationNoEffect || effect.Verification == domain.VerificationPartial) && evidenced(effect.Evidence) {
			failure = true
		}
	}
	return failure
}

func evidenced(ids []domain.EvidenceID) bool {
	return evidencedWithin(ids, 64)
}

func evidencedWithin(ids []domain.EvidenceID, max int) bool {
	if len(ids) == 0 || len(ids) > max {
		return false
	}
	unique := make(map[domain.EvidenceID]bool, len(ids))
	for _, id := range ids {
		if id == "" || unique[id] {
			return false
		}
		unique[id] = true
	}
	return true
}

func hasEffect(outcome domain.Outcome, kind domain.EffectKind, target domain.TargetRef, evidence domain.EvidenceID) bool {
	return slices.ContainsFunc(outcome.Effects, func(effect domain.EffectResult) bool {
		return effect.Effect == (domain.Effect{Kind: kind, Target: target}) && slices.Contains(effect.Evidence, evidence)
	})
}

func proofTargets(proof Proof, job domain.JobRef, target domain.TargetRef) bool {
	return proof.Job == job && proof.Target == target
}

func proofInWindow(proof Proof, from, to time.Time) bool {
	return proof.Evidence != "" && !proof.Observed.IsZero() && !proof.Observed.Before(from) && !proof.Observed.After(to)
}

func separateProof(a, b Proof, from, to time.Time) bool {
	return a.Evidence != b.Evidence && proofInWindow(a, from, to) && proofInWindow(b, from, to)
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.IsZero() || b.Before(a) {
		return b
	}
	return a
}

func (r SampleResult) excluding(reason domain.ReasonCode) SampleResult {
	r.Sample.Eligible = false
	r.Sample.Exclusion, r.Reason = reason, reason
	return r
}

func (r SampleResult) including(state SampleState) SampleResult {
	r.Sample.Eligible, r.State, r.Sample.Exclusion = true, state, ""
	return r
}
