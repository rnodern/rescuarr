package policy

import (
	"slices"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
)

func (a *assessment) evaluateImport() {
	i := a.input
	partial := i.Import.Assessment.Value == ImportSubset
	reason := SafeImport
	if partial {
		reason = PartialPack
	}
	a.find(reason, i.Import.Assessment.Observation)
	if i.JobPhase.Value != PhaseComplete || len(i.Import.Candidates) == 0 {
		a.hold(TargetAmbiguous)
		return
	}
	targets := make(map[domain.TargetRef]bool)
	candidateKeys := make([]string, 0, len(i.Import.Candidates))
	for _, candidate := range i.Import.Candidates {
		if candidate.Key == "" || slices.Contains(candidateKeys, candidate.Key) || len(candidate.Targets) == 0 {
			a.hold(TargetAmbiguous)
		}
		candidateKeys = append(candidateKeys, candidate.Key)
		if !a.requireArr(candidate.Approved.Observation) || !a.verified(candidate.Approved) ||
			!a.verified(candidate.PathContained) || !a.verified(candidate.StableAndUsable) {
			a.hold(TargetAmbiguous, candidate.Approved.Observation, candidate.PathContained.Observation, candidate.StableAndUsable.Observation)
		}
		for _, ref := range candidate.Targets {
			if !slices.Contains(a.wanted, ref) || targets[ref] {
				a.hold(TargetAmbiguous)
			}
			if target, ok := a.targets[ref]; !ok || candidate.Profiles[ref] != target.Target.ProfileRevision {
				a.hold(RequestChanged)
			}
			targets[ref] = true
		}
	}
	if (!partial && (len(targets) != len(a.wanted) || len(targets) != len(a.targets))) ||
		(partial && (i.Instance.Product != domain.ProductSonarr || len(targets) >= len(a.targets))) {
		a.hold(TargetAmbiguous)
	}
	capability := i.Capabilities[ImportOriginal]
	if (partial || i.Case.Job.Shared || i.Case.Job.Protocol == domain.ProtocolTorrent) && !capability.CoversFullJobPreservation {
		a.hold(SourcePreservationUnknown)
	}
	selected := make([]domain.TargetRef, 0, len(targets))
	effects := []domain.Effect{{Kind: domain.EffectSourceRetained}}
	for _, target := range a.wanted {
		if targets[target] {
			selected = append(selected, target)
			effects = append(effects, domain.Effect{Kind: domain.EffectLibraryImport, Target: target})
		}
	}
	slices.Sort(candidateKeys)
	a.plan(ImportOriginal, domain.ActionImport, selected, effects, candidateKeys)
}

func (a *assessment) evaluateReject() {
	i := a.input
	failure := i.Failure
	if !eligibleFailure(failure.Reason) || !a.requireArr(failure.Primary.Observation) || !a.verified(failure.Primary) {
		a.find(StalledUnknown, i.Import.Assessment.Observation)
		return
	}
	a.find(failure.Reason, failure.Primary.Observation, failure.Corroborated.Observation)
	corroboration := failure.Corroborated.Observation
	if !a.verified(failure.Corroborated) || !independent(failure.Primary.Observation, corroboration) ||
		corroboration.Scope != i.Case.Job.Client || corroboration.Source != domain.Source(domain.ProductSAB) ||
		corroboration.SourceRecord != i.Case.Job.DownloadID || failure.Primary.Observation.SourceRecord != i.Case.Job.DownloadID {
		a.hold(UnknownEvidence, failure.Corroborated.Observation)
	}
	if i.JobPhase.Value != PhaseFailed && i.JobPhase.Value != PhaseComplete {
		a.hold(UnknownEvidence, i.JobPhase.Observation)
	}
	job := i.Case.Job
	if job.Release.GrabHistoryID <= 0 || job.Release.IndexerID <= 0 || job.Release.GUID == "" {
		a.hold(UnknownProvenance)
	}
	if len(a.wanted) != len(a.targets) || job.Shared || !job.CompleteSet {
		a.hold(OwnershipConflict)
	}
	if !a.requireCurrent(i.Seeding.Observation, factAge) || i.Seeding.Value || job.Protocol != domain.ProtocolUsenet {
		a.hold(ClientRemovalUnsafe, i.Seeding.Observation)
	}
	if !a.availableForSearch(a.wanted) {
		return
	}
	retain := []domain.Effect{{Kind: domain.EffectQueueRemoval}, {Kind: domain.EffectBlocklist}, {Kind: domain.EffectSourceRetained}}
	remove := []domain.Effect{{Kind: domain.EffectQueueRemoval}, {Kind: domain.EffectBlocklist}, {Kind: domain.EffectClientRemoval}, {Kind: domain.EffectDataRemoval}}
	path, effects := RejectRetain, retain
	retaining := i.Capabilities[RejectRetain]
	if !a.certified(retaining, RejectRetain, retain) || !a.searchPathSupported(retaining) {
		path, effects = RejectRemove, remove
		capability := i.Capabilities[path]
		if !capability.DedicatedUsenetRemoval || !a.verified(i.DedicatedUsenet) {
			a.hold(ClientRemovalUnsafe, i.DedicatedUsenet.Observation)
		}
	}
	if !a.searchPathSupported(i.Capabilities[path]) {
		a.hold(UnsupportedEffect)
	}
	a.plan(path, domain.ActionReject, a.wanted, effects, nil)
}

func (a *assessment) evaluateSearch(prior PriorReject) {
	i := a.input
	capability, present := i.Capabilities[prior.Path]
	if !present || prior.Job != i.Case.Job.Ref || prior.CapabilityRevision != capability.Revision ||
		prior.OwnedSearch != capability.SearchAuthority || !a.verifiedReject(prior) {
		a.hold(AwaitingRejectVerification)
		return
	}
	for _, target := range a.wanted {
		if !slices.Contains(prior.Targets, target) {
			a.hold(TargetAmbiguous)
		}
	}
	if prior.OwnedSearch == SearchByArr {
		a.find(AwaitingArrSearch)
		if len(a.result.Holds) == 0 {
			a.result.State = domain.CaseWaitingReplacement
		}
		return
	}
	if prior.OwnedSearch != SearchByRescuarr || !a.availableForSearch(a.wanted) {
		a.hold(UnsupportedEffect)
		return
	}
	if i.Instance.Product == domain.ProductRadarr && len(a.wanted) != 1 {
		a.hold(TargetAmbiguous)
	}
	effects := make([]domain.Effect, 0, len(a.wanted))
	for _, target := range a.wanted {
		effects = append(effects, domain.Effect{Kind: domain.EffectSearchDispatch, Target: target})
	}
	a.find(DownloadFailed)
	a.plan(SearchScoped, domain.ActionSearch, a.wanted, effects, []string{string(prior.Outcome.AttemptID)})
}

func (a *assessment) verifiedReject(prior PriorReject) bool {
	outcome, proof, job := prior.Outcome, prior.Readback, a.input.Case.Job
	if outcome.AttemptID == "" || outcome.Verification != domain.VerificationEffect || prior.SentAt.IsZero() ||
		outcome.ObservedAt.Before(prior.SentAt) || outcome.ObservedAt.After(a.input.Now) ||
		proof.AttemptID != outcome.AttemptID || proof.Job != job.Ref || proof.DownloadID != job.DownloadID ||
		proof.Release != job.Release || proof.Release.GrabHistoryID <= 0 || proof.Release.IndexerID <= 0 || proof.Release.GUID == "" ||
		proof.FailedHistoryID <= 0 || proof.FailedHistoryID == proof.Release.GrabHistoryID ||
		!slices.Equal(sortedQueueIDs(job), sortedIDs(proof.QueueIDs)) {
		return false
	}
	// A failed or completed positive observation is not proof of retirement.
	// Require the exact fresh, positive absence read in the job projection too.
	if phase := a.input.JobPhase.Observation; phase != prior.CurrentRetirement || phase.SourceRecord != proof.DownloadID ||
		phase.Kind != RejectQueueReadback || phase.State != domain.ObservationAbsent ||
		phase.ObservedAt.Before(outcome.ObservedAt) || !a.requireArr(phase) {
		return false
	}
	if !a.readback(prior, proof.Queue, RejectQueueReadback, domain.ObservationAbsent, a.fromArr(proof.Queue)) ||
		!a.readback(prior, proof.FailedHistory, RejectHistoryReadback, domain.ObservationKnown, a.fromArr(proof.FailedHistory)) ||
		!a.readback(prior, proof.Blocklist, RejectBlocklistReadback, domain.ObservationKnown, a.fromArr(proof.Blocklist)) {
		return false
	}
	if prior.Path == RejectRetain {
		if !a.readback(prior, proof.SourceRetained, RejectSourceReadback, domain.ObservationKnown, a.storageProof(proof.SourceRetained)) {
			return false
		}
		return a.matchEffects(prior, map[domain.EffectKind]domain.EvidenceID{
			domain.EffectQueueRemoval: proof.Queue.ID, domain.EffectBlocklist: proof.Blocklist.ID,
			domain.EffectSourceRetained: proof.SourceRetained.ID,
		})
	}
	if prior.Path != RejectRemove || !a.readback(prior, proof.ClientRemoval, RejectClientReadback, domain.ObservationAbsent,
		proof.ClientRemoval.Source == domain.Source(domain.ProductSAB) && proof.ClientRemoval.Scope == job.Client) ||
		!a.readback(prior, proof.DataRemoval, RejectDataReadback, domain.ObservationAbsent, a.storageProof(proof.DataRemoval)) {
		return false
	}
	return a.matchEffects(prior, map[domain.EffectKind]domain.EvidenceID{
		domain.EffectQueueRemoval: proof.Queue.ID, domain.EffectBlocklist: proof.Blocklist.ID,
		domain.EffectClientRemoval: proof.ClientRemoval.ID, domain.EffectDataRemoval: proof.DataRemoval.ID,
	})
}

func (a *assessment) readback(prior PriorReject, observation domain.Observation, kind domain.ObservationKind, state domain.ObservationState, trusted bool) bool {
	if !trusted || observation.Kind != kind || observation.State != state || observation.SourceRecord != prior.Readback.DownloadID ||
		observation.ObservedAt.Before(prior.SentAt) || observation.ObservedAt.After(prior.Outcome.ObservedAt) ||
		!observation.Fresh(prior.Outcome.ObservedAt) || observation.ID == "" {
		return false
	}
	a.remember(observation)
	return true
}

func (a *assessment) storageProof(observation domain.Observation) bool {
	return observation.Source == StorageProbeSource && a.affects(observation.Scope) &&
		(observation.Scope == a.input.Case.Job.Client || observation.Scope.Kind == domain.ScopeVolume)
}

func (a *assessment) matchEffects(prior PriorReject, required map[domain.EffectKind]domain.EvidenceID) bool {
	kinds := make([]domain.EffectKind, 0, len(required))
	used := map[domain.EvidenceID]bool{prior.Readback.FailedHistory.ID: true}
	for kind, id := range required {
		if used[id] {
			return false
		}
		used[id] = true
		kinds = append(kinds, kind)
	}
	if !a.certified(a.input.Capabilities[prior.Path], prior.Path, rejectEffects(kinds)) {
		return false
	}
	verified := make(map[domain.EffectKind]bool)
	for _, result := range prior.Outcome.Effects {
		kind := result.Effect.Kind
		id, expected := required[kind]
		if !expected {
			continue
		}
		if verified[kind] || result.Effect.Target != (domain.TargetRef{}) || result.Verification != domain.VerificationEffect ||
			!slices.Contains(result.Evidence, id) || slices.ContainsFunc(result.Evidence, func(ref domain.EvidenceID) bool {
			return ref != id && ref != prior.Readback.FailedHistory.ID && used[ref]
		}) {
			return false
		}
		verified[kind] = true
	}
	return len(verified) == len(required)
}

func sortedIDs(ids []int64) []int64 {
	sorted := slices.Clone(ids)
	slices.Sort(sorted)
	return sorted
}

func sortedQueueIDs(job domain.Job) []int64 {
	queue := make([]int64, 0, len(job.Members))
	for _, member := range job.Members {
		queue = append(queue, member.QueueID)
	}
	slices.Sort(queue)
	return queue
}

func rejectEffects(kinds []domain.EffectKind) []domain.Effect {
	effects := make([]domain.Effect, 0, len(kinds))
	for _, kind := range kinds {
		effects = append(effects, domain.Effect{Kind: kind})
	}
	return effects
}

func (a *assessment) availableForSearch(targets []domain.TargetRef) bool {
	allowed := true
	for _, ref := range targets {
		availability := a.targets[ref].SearchAvailable
		if !a.requireArr(availability.Observation) || !a.verified(availability) {
			a.hold(RequestChanged, availability.Observation)
			allowed = false
		}
	}
	return allowed
}

func (a *assessment) searchPathSupported(capability Capability) bool {
	switch capability.SearchAuthority {
	case SearchByArr:
		return slices.Contains(capability.Effects, domain.EffectSearchDispatch)
	case SearchByRescuarr:
		return a.certified(a.input.Capabilities[SearchScoped], SearchScoped, []domain.Effect{{Kind: domain.EffectSearchDispatch}})
	default:
		return false
	}
}

func (a *assessment) verified(fact domain.Fact[bool]) bool {
	return a.requireCurrent(fact.Observation, factAge) && fact.Observation.State == domain.ObservationKnown && fact.Value
}

func independent(first, second domain.Observation) bool {
	return first.Source != "" && second.Source != "" && first.Source != second.Source && first.ID != second.ID
}

func eligibleFailure(reason domain.ReasonCode) bool {
	switch reason {
	case QualityRejected, ArchiveUnresolved, PasswordRequired, SampleOnly, FileUnreadable, DownloadFailed:
		return true
	default:
		return false
	}
}

func (a *assessment) plan(path Path, action domain.Action, targets []domain.TargetRef, effects []domain.Effect, selectors []string) {
	i := a.input
	a.result.Path = path
	capability := i.Capabilities[path]
	if !a.certified(capability, path, effects) {
		a.hold(UnsupportedEffect)
	}
	// Reject may allocate indirectly when Arr owns search, so its relevant capacity is checked too.
	a.applyCapacity(path, targets)
	gate := i.Gates[path]
	switch {
	case !gate.BudgetKnown:
		a.hold(BudgetUnavailable)
	case gate.Remaining <= 0:
		a.hold(BudgetExhausted)
	case i.Now.Before(gate.NotBefore):
		a.hold(Cooldown)
		a.result.NextCheck = gate.NotBefore.UTC()
	}
	if slices.Contains(i.Policy.PausedActions, path) {
		a.hold(RecoveryPaused)
	}
	authority := effectiveAuthority(i.Policy, path)
	if action == domain.ActionReject && capability.SearchAuthority == SearchByRescuarr &&
		(effectiveAuthority(i.Policy, SearchScoped) == AuthorityNone || slices.Contains(i.Policy.PausedActions, SearchScoped)) {
		a.hold(ActionDenied)
	}
	if authority == AuthorityNone {
		a.hold(ActionDenied)
	}
	if len(a.result.Holds) != 0 || len(targets) == 0 {
		return
	}
	if action == domain.ActionReject && capability.SearchAuthority == SearchByArr {
		effects = append(effects, domain.Effect{Kind: domain.EffectSearchDispatch})
	}
	operation := domain.Operation{Job: i.Case.Job.Ref, Targets: slices.Clone(targets), Action: action, Effects: effects}
	operation.Key = domain.OperationKey(digest(struct {
		Job        domain.JobRef
		Path       Path
		DownloadID string
		Client     domain.Scope
		Release    domain.ReleaseIdentity
		Targets    []domain.TargetRef
		Selectors  []string
	}{operation.Job, path, i.Case.Job.DownloadID, i.Case.Job.Client, i.Case.Job.Release, targets, selectors}))
	queue := sortedQueueIDs(i.Case.Job)
	ids, materialEvidence := a.evidenceFingerprint()
	guards := []domain.Guard{{Reason: "preconditions_verified", Passed: true, Evidence: ids}}
	type targetIntent struct {
		Target  domain.TargetRef
		Profile domain.Revision
		FileID  int64
		Intent  Intent
	}
	intents := make([]targetIntent, 0, len(targets))
	for _, target := range targets {
		evidence := a.targets[target]
		intents = append(intents, targetIntent{target, evidence.Target.ProfileRevision, evidence.Target.FileID.Value, evidence.Intent.Value})
	}
	decisionHash := digest(struct {
		Operation                                 domain.Operation
		Case                                      domain.CaseID
		CaseRevision, Policy, Capability, Pattern domain.Revision
		Connection                                domain.Revision
		Evidence                                  []string
		Targets                                   []targetIntent
		Authority                                 Authority
		SearchOwner                               SearchAuthority
		QueueIDs                                  []int64
	}{operation, i.Case.ID, i.Case.Revision, i.Policy.Revision, capability.Revision, i.PatternRevision,
		i.Instance.ConnectionRevision, materialEvidence, intents, authority, capability.SearchAuthority, queue})
	a.result.Authority = authority
	a.result.State = domain.CaseProposed
	if authority == AuthorityAutonomous {
		a.result.State = domain.CaseReady
	}
	expires := i.Now.Add(proposalAge)
	a.result.Proposal = &domain.Proposal{
		ID: proposalID(decisionHash, expires), CaseID: i.Case.ID, CaseRevision: i.Case.Revision,
		PolicyRevision: i.Policy.Revision, CapabilityRevision: capability.Revision, PatternRevision: i.PatternRevision,
		DecisionHash: decisionHash, Operation: operation, Guards: guards, ExpiresAt: expires,
	}
}

func (a *assessment) evidenceFingerprint() ([]domain.EvidenceID, []string) {
	ids := make([]domain.EvidenceID, 0, len(a.seenFacts))
	material := make([]string, 0, len(a.seenFacts))
	for id, evidence := range a.seenFacts {
		ids = append(ids, id)
		hash := evidence.ContentHash
		if hash == "" {
			hash = string(id)
		}
		material = append(material, digest(struct {
			Scope                 domain.Scope
			Source                domain.Source
			Kind                  domain.ObservationKind
			State                 domain.ObservationState
			Adapter, Record, Hash string
			Schema                domain.Revision
		}{evidence.Scope, evidence.Source, evidence.Kind, evidence.State, evidence.AdapterRevision,
			evidence.SourceRecord, hash, evidence.SchemaRevision}))
	}
	slices.Sort(ids)
	slices.Sort(material)
	return ids, slices.Compact(material)
}

func proposalID(decisionHash string, expires time.Time) domain.ProposalID {
	return domain.ProposalID(digest(struct {
		Hash    string
		Expires time.Time
	}{decisionHash, expires.UTC()}))
}

func (a *assessment) certified(capability Capability, path Path, effects []domain.Effect) bool {
	if !capability.Certified || capability.Path != path || capability.Instance != a.input.Instance.Ref ||
		capability.Revision == 0 || capability.Protocol != a.input.Case.Job.Protocol ||
		(capability.Protocol != domain.ProtocolTorrent && capability.Protocol != domain.ProtocolUsenet) {
		return false
	}
	for _, kind := range capability.Effects {
		if kind == domain.EffectReplacementGrab ||
			(kind == domain.EffectSearchDispatch && (path == RejectRetain || path == RejectRemove) && capability.SearchAuthority == SearchByArr) {
			continue
		}
		if !slices.ContainsFunc(effects, func(effect domain.Effect) bool { return effect.Kind == kind }) {
			return false
		}
	}
	for _, effect := range effects {
		if !slices.Contains(capability.Effects, effect.Kind) {
			return false
		}
	}
	return true
}

func effectiveAuthority(policy Policy, path Path) Authority {
	if policy.Revision == 0 || !validMode(policy.Global) || !validMode(policy.Instance) {
		return AuthorityNone
	}
	global, instance := policy.GlobalActions[path], policy.InstanceActions[path]
	if (global != PermitReview && global != PermitAutomatic) || (instance != PermitReview && instance != PermitAutomatic) {
		return AuthorityNone
	}
	if policy.Global == domain.ModeObserve || policy.Instance == domain.ModeObserve {
		return AuthorityPreview
	}
	if policy.Global == domain.ModeAutonomous && policy.Instance == domain.ModeAutonomous && global == PermitAutomatic && instance == PermitAutomatic {
		return AuthorityAutonomous
	}
	return AuthorityAssist
}

func validMode(mode domain.Mode) bool {
	return mode == domain.ModeObserve || mode == domain.ModeAssist || mode == domain.ModeAutonomous
}

func (a *assessment) applyCapacity(path Path, targets []domain.TargetRef) {
	required := make(map[domain.Scope]struct{}, len(targets)+1)
	for _, ref := range targets {
		root := a.targets[ref].Target.Root
		if !validScope(root) || root.Kind != domain.ScopeRoot || root.Instance != a.input.Instance.Ref {
			a.hold(CapacityUnknown)
		}
		required[root] = struct{}{}
	}
	if path == RejectRetain || path == RejectRemove || path == SearchScoped {
		client := a.input.Case.Job.Client
		if !validScope(client) || client.Kind != domain.ScopeClient {
			a.hold(CapacityUnknown)
		}
		required[client] = struct{}{}
	}
	seen := make(map[domain.Scope]bool)
	for _, capacity := range a.input.Capacities[path] {
		observation := capacity.FreeBytes.Observation
		if !validScope(capacity.Scope) || !a.affects(capacity.Scope) || observation.Scope != capacity.Scope ||
			!capacity.AllocationKnown || seen[capacity.Scope] {
			a.hold(CapacityUnknown, observation)
			continue
		}
		seen[capacity.Scope] = true
		if !known(capacity.FreeBytes, a.input.Now, capacityAge) {
			if stale(observation, a.input.Now, capacityAge) {
				a.hold(CapacityStale, observation)
			} else {
				a.hold(CapacityUnknown, observation)
			}
			continue
		}
		a.remember(observation)
		free := capacity.FreeBytes.Value
		if free < capacity.FloorBytes || free-capacity.FloorBytes < capacity.ReservedBytes ||
			free-capacity.FloorBytes-capacity.ReservedBytes < capacity.RequiredBytes {
			a.hold(LowCapacity, observation)
		}
	}
	for scope := range required {
		if !seen[scope] {
			a.hold(CapacityUnknown)
		}
	}
}

// MatchApproval compares an approved plan against a fresh evaluation. Consuming
// the approval once is a transaction in the durable ledger, outside policy.
func MatchApproval(approved domain.Proposal, current Result, now time.Time) bool {
	if current.Proposal == nil || (current.Authority != AuthorityAssist && current.Authority != AuthorityAutonomous) ||
		now.IsZero() || !now.Before(approved.ExpiresAt) || !now.Before(current.Proposal.ExpiresAt) {
		return false
	}
	fresh := current.Proposal
	return approved.ID == proposalID(approved.DecisionHash, approved.ExpiresAt) &&
		approved.DecisionHash == fresh.DecisionHash && approved.CaseID == fresh.CaseID &&
		approved.CaseRevision == fresh.CaseRevision && approved.PolicyRevision == fresh.PolicyRevision &&
		approved.CapabilityRevision == fresh.CapabilityRevision && approved.PatternRevision == fresh.PatternRevision &&
		approved.Operation.Job == fresh.Operation.Job && approved.Operation.Key == fresh.Operation.Key &&
		approved.Operation.Action == fresh.Operation.Action && slices.Equal(approved.Operation.Targets, fresh.Operation.Targets) &&
		slices.Equal(approved.Operation.Effects, fresh.Operation.Effects)
}
