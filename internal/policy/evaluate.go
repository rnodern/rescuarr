package policy

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
)

const (
	factAge       = time.Minute
	capacityAge   = 5 * time.Minute
	completionAge = 10 * time.Minute
	coherentGap   = 30 * time.Second
	proposalAge   = 10 * time.Minute
	pollInterval  = 45 * time.Second
)

const (
	UpstreamUnavailable        domain.ReasonCode = "upstream_unavailable"
	AuthenticationFailed       domain.ReasonCode = "authentication_failed"
	ContractChanged            domain.ReasonCode = "contract_changed"
	OwnershipConflict          domain.ReasonCode = "ownership_conflict"
	TargetAmbiguous            domain.ReasonCode = "target_ambiguous"
	UnknownProvenance          domain.ReasonCode = "unknown_provenance"
	RootUnavailable            domain.ReasonCode = "root_path_unavailable"
	PathMappingMissing         domain.ReasonCode = "path_mapping_missing"
	PermissionDenied           domain.ReasonCode = "permission_denied"
	LowCapacity                domain.ReasonCode = "capacity_low"
	DownloadActive             domain.ReasonCode = "download_active"
	PostProcessing             domain.ReasonCode = "post_processing"
	ArrImporting               domain.ReasonCode = "arr_import_in_progress"
	CompletionGrace            domain.ReasonCode = "completion_grace"
	TargetWithdrawn            domain.ReasonCode = "target_withdrawn"
	RequestChanged             domain.ReasonCode = "request_changed"
	AlreadySatisfied           domain.ReasonCode = "already_satisfied"
	SafeImport                 domain.ReasonCode = "safe_import_candidate"
	PartialPack                domain.ReasonCode = "partially_recoverable_pack"
	QualityRejected            domain.ReasonCode = "quality_or_format_rejected"
	ArchiveUnresolved          domain.ReasonCode = "archive_unresolved"
	PasswordRequired           domain.ReasonCode = "password_required"
	SampleOnly                 domain.ReasonCode = "sample_only"
	FileUnreadable             domain.ReasonCode = "file_unreadable"
	DownloadFailed             domain.ReasonCode = "download_failed"
	StalledUnknown             domain.ReasonCode = "stalled_unknown"
	UnsupportedPayload         domain.ReasonCode = "unsupported_payload"
	CompetingDownload          domain.ReasonCode = "competing_download"
	UnknownEvidence            domain.ReasonCode = "evidence_unknown"
	StaleEvidence              domain.ReasonCode = "evidence_stale"
	IncidentOpen               domain.ReasonCode = "incident_open"
	RecoveryPaused             domain.ReasonCode = "recovery_paused"
	UnresolvedOverlap          domain.ReasonCode = "unresolved_overlap"
	UnsupportedEffect          domain.ReasonCode = "unsupported_effect"
	BudgetUnavailable          domain.ReasonCode = "budget_unavailable"
	BudgetExhausted            domain.ReasonCode = "budget_exhausted"
	Cooldown                   domain.ReasonCode = "action_cooldown"
	ActionDenied               domain.ReasonCode = "action_denied"
	CapacityUnknown            domain.ReasonCode = "capacity_unknown"
	CapacityStale              domain.ReasonCode = "capacity_stale"
	ClientRemovalUnsafe        domain.ReasonCode = "client_removal_unsafe"
	SourcePreservationUnknown  domain.ReasonCode = "source_preservation_unknown"
	AwaitingRejectVerification domain.ReasonCode = "awaiting_reject_verification"
	AwaitingArrSearch          domain.ReasonCode = "awaiting_arr_search"
)

type assessment struct {
	input     Input
	result    Result
	wanted    []domain.TargetRef
	targets   map[domain.TargetRef]TargetEvidence
	seenFacts map[domain.EvidenceID]domain.Observation
}

// Evaluate returns the same decision from the same snapshot, policy and time.
// A proposal in Preview mode is informative. All other authority still requires
// lane 06's transactional reservation, current preflight and one-shot dispatch.
func Evaluate(input Input) Result {
	input.Now = input.Now.UTC()
	a := assessment{
		input: input, targets: make(map[domain.TargetRef]TargetEvidence),
		seenFacts: make(map[domain.EvidenceID]domain.Observation),
		result:    Result{State: domain.CaseHeld, Authority: AuthorityNone, NextCheck: input.Now.Add(pollInterval)},
	}
	a.collectScopes()
	a.evaluate()
	a.normalise()
	return a.result
}

func (a *assessment) evaluate() {
	i := a.input
	if i.Now.IsZero() || !i.Instance.Ref.Valid() || i.Case.ID == "" || i.Case.Revision == 0 ||
		i.Case.Job.Ref.Instance != i.Instance.Ref || i.Case.Job.Ref.ID == "" || i.Case.Job.Ref.Generation == 0 {
		a.hold(OwnershipConflict)
		return
	}
	a.evaluateEnvironment()
	job := i.Case.Job
	if !a.verified(i.OwnershipVerified) || job.DownloadID == "" {
		a.hold(OwnershipConflict, i.OwnershipVerified.Observation)
	}
	if !job.CompleteSet {
		a.hold(TargetAmbiguous)
	}
	if job.Shared {
		a.find(OwnershipConflict, i.OwnershipVerified.Observation)
	}
	a.evaluateTargets()
	if len(a.wanted) == 0 && len(a.targets) != 0 && !a.containsHold(UnknownEvidence, StaleEvidence, TargetAmbiguous, RequestChanged, ContractChanged, UpstreamUnavailable, AuthenticationFailed) {
		a.result.State = domain.CaseClosed
		return
	}

	phase := i.JobPhase
	if a.requireCurrent(phase.Observation, factAge) && phase.Observation.State == domain.ObservationKnown {
		switch phase.Value {
		case PhaseDownload, PhaseProcess, PhaseImport:
			a.find(domain.ReasonCode(phase.Value), phase.Observation)
			a.wait()
			return
		case PhaseComplete:
			if i.PriorReject == nil && (i.FirstCompletedAt.IsZero() || i.FirstCompletedAt.After(i.PreviousCoherentAt) || !i.PreviousCoherentAt.Before(phase.Observation.ObservedAt) ||
				i.PreviousCoherentAt.IsZero() || phase.Observation.ObservedAt.Sub(i.PreviousCoherentAt) < coherentGap ||
				i.Now.Before(i.FirstCompletedAt.Add(completionAge))) {
				a.find(CompletionGrace, phase.Observation)
				a.wait()
				return
			}
		case PhaseFailed:
		default:
			a.hold(UnsupportedPayload, phase.Observation)
		}
	} else if i.PriorReject == nil || phase.Observation.State != domain.ObservationAbsent || !fresh(phase.Observation, i.Now, factAge) || !a.affects(phase.Observation.Scope) {
		a.hold(UnknownEvidence, phase.Observation)
	}
	if len(a.wanted) == 0 {
		return
	}
	if i.PriorReject != nil {
		a.evaluateSearch(*i.PriorReject)
		return
	}
	if !a.requireCurrent(i.Import.Assessment.Observation, factAge) || i.Import.Assessment.Observation.State != domain.ObservationKnown {
		a.find(StalledUnknown)
		return
	}
	switch i.Import.Assessment.Value {
	case ImportAll, ImportSubset:
		a.evaluateImport()
	case ImportNone:
		a.evaluateReject()
	default:
		a.hold(UnsupportedPayload, i.Import.Assessment.Observation)
	}
}

func (a *assessment) wait() {
	if !a.containsHold(UpstreamUnavailable, AuthenticationFailed, ContractChanged, OwnershipConflict, TargetAmbiguous,
		RootUnavailable, PathMappingMissing, PermissionDenied, LowCapacity, IncidentOpen, RecoveryPaused, UnresolvedOverlap) {
		a.result.State = domain.CaseWaiting
	}
}

func (a *assessment) evaluateEnvironment() {
	i := a.input
	if a.requireCurrent(i.ServiceHealth.Observation, factAge) && i.ServiceHealth.Observation.State == domain.ObservationKnown {
		switch i.ServiceHealth.Value {
		case HealthAvailable:
		case HealthAuthentication:
			a.hold(AuthenticationFailed, i.ServiceHealth.Observation)
		case HealthContract:
			a.hold(ContractChanged, i.ServiceHealth.Observation)
		default:
			a.hold(UpstreamUnavailable, i.ServiceHealth.Observation)
		}
	} else {
		a.hold(UnknownEvidence, i.ServiceHealth.Observation)
	}
	if !a.verified(i.IdentityValid) || i.Instance.ConnectionRevision == 0 {
		a.hold(ContractChanged, i.IdentityValid.Observation)
	}
	if !a.requireCurrent(i.Overlap.Observation, factAge) || i.Overlap.Value {
		a.hold(UnresolvedOverlap, i.Overlap.Observation)
	}
	if i.Policy.Paused || slices.ContainsFunc(i.Policy.PausedScopes, a.affects) {
		a.hold(RecoveryPaused)
	}
	for _, incident := range i.Incidents {
		if incident.Hold && a.affects(incident.Scope) {
			a.addHold(IncidentOpen, incident.Evidence)
			if priority(incident.Reason) == 30 {
				a.addHold(incident.Reason, incident.Evidence)
			}
		}
	}
	for _, fault := range i.DependencyFaults {
		if priority(fault.Reason) == 30 && a.affects(fault.Evidence.Scope) && fresh(fault.Evidence, i.Now, capacityAge) {
			a.hold(fault.Reason, fault.Evidence)
		}
	}
}

func (a *assessment) evaluateTargets() {
	i := a.input
	listed := make(map[domain.TargetRef]domain.Target)
	for _, target := range i.Case.Targets {
		if _, duplicate := listed[target.Ref]; duplicate {
			a.hold(TargetAmbiguous)
		}
		listed[target.Ref] = target
	}
	members := make(map[domain.TargetRef]bool)
	for _, member := range i.Case.Job.Members {
		if _, present := listed[member.Target]; !present || member.QueueID <= 0 || member.SnapshotGeneration == 0 {
			a.hold(TargetAmbiguous)
		}
		members[member.Target] = true
	}
	for _, target := range i.Targets {
		t := target.Target
		registered, present := listed[t.Ref]
		if !present || !members[t.Ref] || t.Ref.Instance != i.Instance.Ref || !validProductTarget(i.Instance.Product, t.Ref) || registered.Root != t.Root {
			a.hold(TargetAmbiguous)
			continue
		}
		if _, duplicate := a.targets[t.Ref]; duplicate {
			a.hold(TargetAmbiguous)
			continue
		}
		a.targets[t.Ref] = target
		if !a.requireCurrent(target.Intent.Observation, factAge) || !a.requireCurrent(t.Monitored.Observation, factAge) ||
			!a.requireCurrent(t.FileID.Observation, factAge) {
			continue
		}
		if target.Intent.Observation.State != domain.ObservationKnown || t.Monitored.Observation.State != domain.ObservationKnown {
			a.hold(UnknownEvidence, target.Intent.Observation, t.Monitored.Observation)
			continue
		}
		if target.Intent.Value == NeedWithdrawn && !t.Monitored.Value {
			a.find(TargetWithdrawn, target.Intent.Observation)
			continue
		}
		if !t.Monitored.Value || t.ProfileRevision == 0 || target.ProfileAtGrab == 0 || target.ProfileAtGrab != t.ProfileRevision {
			a.hold(RequestChanged, target.Intent.Observation, t.Monitored.Observation)
			continue
		}
		switch target.Intent.Value {
		case NeedSatisfied:
			if t.FileID.Value <= 0 || t.FileID.Observation.State != domain.ObservationKnown {
				a.hold(TargetAmbiguous, t.FileID.Observation)
				continue
			}
			a.find(AlreadySatisfied, target.Intent.Observation, t.FileID.Observation)
			continue
		case NeedFirstImport, NeedUpgrade:
			if (target.Intent.Value == NeedFirstImport && (t.FileID.Value != 0 || t.FileID.Observation.State != domain.ObservationAbsent)) ||
				(target.Intent.Value == NeedUpgrade && (t.FileID.Value <= 0 || t.FileID.Observation.State != domain.ObservationKnown)) {
				a.hold(RequestChanged, target.Intent.Observation, t.FileID.Observation)
				continue
			}
		default:
			a.hold(UnknownEvidence, target.Intent.Observation)
			continue
		}
		if !a.requireCurrent(target.CompetingGrab.Observation, factAge) {
			continue
		}
		if target.CompetingGrab.Value {
			a.hold(CompetingDownload, target.CompetingGrab.Observation)
		}
		a.wanted = append(a.wanted, t.Ref)
	}
	if len(listed) == 0 || len(listed) != len(a.targets) || (i.Instance.Product == domain.ProductRadarr && len(listed) != 1) {
		a.hold(TargetAmbiguous)
	}
	slices.SortFunc(a.wanted, compareTarget)
}

func validProductTarget(product domain.Product, target domain.TargetRef) bool {
	return target.Valid() && ((product == domain.ProductSonarr && target.Kind == domain.TargetEpisode) ||
		(product == domain.ProductRadarr && target.Kind == domain.TargetMovie))
}

func (a *assessment) requireCurrent(observation domain.Observation, maxAge time.Duration) bool {
	if fresh(observation, a.input.Now, maxAge) && a.affects(observation.Scope) {
		a.remember(observation)
		return true
	}
	if stale(observation, a.input.Now, maxAge) {
		a.hold(StaleEvidence, observation)
	} else {
		a.hold(UnknownEvidence, observation)
	}
	return false
}

func (a *assessment) remember(observation domain.Observation) {
	if previous, exists := a.seenFacts[observation.ID]; exists && previous != observation {
		a.hold(UnknownEvidence, observation)
	}
	a.seenFacts[observation.ID] = observation
}

func fresh(observation domain.Observation, now time.Time, maxAge time.Duration) bool {
	return observation.ID != "" && observation.Fresh(now) && now.Sub(observation.ObservedAt) <= maxAge
}

func known[T any](fact domain.Fact[T], now time.Time, maxAge time.Duration) bool {
	return fact.Observation.State == domain.ObservationKnown && fresh(fact.Observation, now, maxAge)
}

func stale(observation domain.Observation, now time.Time, maxAge time.Duration) bool {
	return observation.State == domain.ObservationStale ||
		((observation.State == domain.ObservationKnown || observation.State == domain.ObservationAbsent) &&
			!observation.ObservedAt.IsZero() && (now.Sub(observation.ObservedAt) > maxAge || !now.Before(observation.ExpiresAt)))
}

func compareTarget(a, b domain.TargetRef) int {
	return cmp.Or(cmp.Compare(a.Instance.ID, b.Instance.ID), cmp.Compare(a.Instance.Epoch, b.Instance.Epoch),
		cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.SeriesID, b.SeriesID), cmp.Compare(a.ID, b.ID))
}

func digest(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err) // private canonical inputs are concrete JSON types
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
