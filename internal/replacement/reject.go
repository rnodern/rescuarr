// Package replacement verifies scoped readbacks for rejection and replacement.
package replacement

import (
	"slices"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/policy"
)

const (
	GrabHistory    domain.ObservationKind = "exact_grab_history"
	ArrTerminal    domain.ObservationKind = "arr_terminal_release_failure"
	ClientTerminal domain.ObservationKind = "client_terminal_release_failure"
)

const maxTargets = 128

// ReleaseSignature is the normalised join key for Arr history and blocklist
// readbacks. Fingerprints come from an adapter's exact title and quality records.
type ReleaseSignature struct {
	TitleFingerprint   string
	QualityFingerprint string
	IndexerID          int64
	Protocol           domain.Protocol
}

type Record struct {
	ID         int64
	DownloadID string
	Target     domain.TargetRef
	At         time.Time
	Signature  ReleaseSignature
}

type Original struct {
	Product domain.Product
	Job     domain.Job
	Targets []domain.TargetRef
	Grab    domain.Fact[[]Record]
	Arr     domain.Fact[bool] // exact terminal release failure, not an infrastructure fault
	Client  domain.Fact[bool] // independently observed terminal, unusable payload
}

// VerifiedSource requires the full single-owner job and an independent terminal
// read. It is evidence for policy, not permission to reject.
func (o Original) VerifiedSource(now time.Time) bool {
	job := o.Job
	record, ok := o.exactGrab()
	if !ok || job.Protocol != domain.ProtocolUsenet ||
		job.Client.Kind != domain.ScopeClient || job.Client.Key == "" || job.Client.Instance != job.Ref.Instance {
		return false
	}
	return o.Arr.Value && o.Client.Value &&
		arrObservation(o, o.Grab.Observation, GrabHistory, domain.ObservationKnown, now) &&
		arrObservation(o, o.Arr.Observation, ArrTerminal, domain.ObservationKnown, now) &&
		scopedObservation(o.Client.Observation, job.Client, domain.Source(domain.ProductSAB),
			ClientTerminal, domain.ObservationKnown, job.DownloadID, now) &&
		!o.Arr.Observation.ObservedAt.Before(record.At) && !o.Client.Observation.ObservedAt.Before(record.At) &&
		uniqueEvidence(o.Grab.Observation, o.Arr.Observation, o.Client.Observation)
}

func (o Original) exactGrab() (Record, bool) {
	job := o.Job
	if !exactScope(o) || len(o.Grab.Value) != len(o.Targets) || job.Release.GrabHistoryID <= 0 || job.Release.GUID == "" {
		return Record{}, false
	}
	signature, grabbedAt := o.Grab.Value[0].Signature, o.Grab.Value[0].At
	if !validSignature(signature, job) || grabbedAt.IsZero() || grabbedAt.After(o.Grab.Observation.ObservedAt) {
		return Record{}, false
	}
	ids := make(map[int64]bool, len(o.Grab.Value))
	targets := make([]domain.TargetRef, 0, len(o.Grab.Value))
	var primary Record
	for _, record := range o.Grab.Value {
		if record.ID <= 0 || ids[record.ID] || record.DownloadID != job.DownloadID ||
			record.Signature != signature || !record.At.Equal(grabbedAt) {
			return Record{}, false
		}
		ids[record.ID] = true
		targets = append(targets, record.Target)
		if record.ID == job.Release.GrabHistoryID {
			primary = record
		}
	}
	return primary, primary.ID != 0 && sameTargets(targets, o.Targets)
}

type BlocklistEntry struct {
	ID        int64
	Targets   []domain.TargetRef
	At        time.Time
	Signature ReleaseSignature
}

// RejectInput combines the durable attempt, its preflight snapshot and independent
// readbacks. Blocklist snapshots are complete for the relevant release and targets.
type RejectInput struct {
	Now          time.Time
	Origin       Original
	Attempt      domain.Attempt
	Queue        domain.Fact[[]int64] // exact original rows now absent
	Failed       domain.Fact[[]Record]
	Before       domain.Fact[[]BlocklistEntry]
	After        domain.Fact[[]BlocklistEntry]
	SourceBefore domain.Observation // positive observation of the original source and its scope
	Source       domain.Observation // retained source or positive proof of absent data
	Client       domain.Fact[bool]  // positive proof the original client job disappeared
}

type Rejection struct {
	Readback policy.RejectReadback
	Outcome  domain.Outcome
}

// VerifyReject keeps queue retirement, blocklist, client disposition and source
// presence separate. Exact readbacks remain available after a lost acknowledgement,
// while the effect results keep the attempt open for reconciliation.
func VerifyReject(in RejectInput) Rejection {
	attempt, origin := in.Attempt, in.Origin
	out := Rejection{
		Outcome: domain.Outcome{AttemptID: attempt.ID, ObservedAt: in.Now, Verification: domain.VerificationUnresolved},
	}
	if len(attempt.Operation.Effects) > 4 {
		return out
	}
	for _, effect := range attempt.Operation.Effects {
		out.Outcome.Effects = append(out.Outcome.Effects, domain.EffectResult{
			Effect: effect, Verification: domain.VerificationUnresolved,
		})
	}
	if !validRejection(in) {
		return out
	}
	job := origin.Job
	ids := queueIDs(job)
	read := policy.RejectReadback{
		AttemptID: attempt.ID, Job: job.Ref, DownloadID: job.DownloadID, Release: job.Release, QueueIDs: ids,
	}
	if arrObservation(origin, in.Queue.Observation, policy.RejectQueueReadback, domain.ObservationAbsent, in.Now) &&
		postSend(in.Queue.Observation, attempt) && sameIDs(in.Queue.Value, ids) {
		read.Queue = in.Queue.Observation
	}
	grabIDs := originalGrabIDs(origin)
	if failed := failedHistory(in, grabIDs); failed != 0 {
		read.FailedHistory, read.FailedHistoryID = in.Failed.Observation, failed
		if insertedBlocklist(in) && uniqueEvidence(in.Failed.Observation, in.After.Observation, in.Before.Observation) {
			read.Blocklist = in.After.Observation
		}
	}
	source, priorSource := in.Source, in.SourceBefore
	if (source.Scope == job.Client || (source.Scope.Kind == domain.ScopeVolume && source.Scope.Instance == job.Ref.Instance && source.Scope.Key != "")) &&
		postSend(source, attempt) && sameReadContract(source, priorSource) && slices.Contains(attempt.Preflight, priorSource.ID) &&
		scopedObservation(priorSource, source.Scope, policy.StorageProbeSource,
			policy.RejectSourceReadback, domain.ObservationKnown, job.DownloadID, attempt.SentAt) {
		if scopedObservation(source, source.Scope, policy.StorageProbeSource,
			policy.RejectSourceReadback, domain.ObservationKnown, job.DownloadID, in.Now) {
			read.SourceRetained = source
		}
		if scopedObservation(source, source.Scope, policy.StorageProbeSource,
			policy.RejectDataReadback, domain.ObservationAbsent, job.DownloadID, in.Now) {
			read.DataRemoval = source
		}
	}
	if in.Client.Value && postSend(in.Client.Observation, attempt) &&
		sameReadContract(in.Client.Observation, origin.Client.Observation) &&
		scopedObservation(in.Client.Observation, job.Client, domain.Source(domain.ProductSAB),
			policy.RejectClientReadback, domain.ObservationAbsent, job.DownloadID, in.Now) {
		read.ClientRemoval = in.Client.Observation
	}
	out.Readback = read
	out.Outcome = rejectOutcome(in, read)
	return out
}

func exactScope(o Original) bool {
	job := o.Job
	if !job.Ref.Instance.Valid() || job.Ref.ID == "" || job.Ref.Generation == 0 || job.DownloadID == "" ||
		!job.CompleteSet || job.Shared || len(o.Targets) == 0 || len(o.Targets) > maxTargets ||
		len(job.Members) == 0 || len(job.Members) > maxTargets {
		return false
	}
	kind := domain.TargetEpisode
	if o.Product == domain.ProductRadarr {
		kind = domain.TargetMovie
		if len(o.Targets) != 1 {
			return false
		}
	} else if o.Product != domain.ProductSonarr {
		return false
	}
	targets := make(map[domain.TargetRef]bool, len(o.Targets))
	for _, target := range o.Targets {
		if !target.Valid() || target.Instance != job.Ref.Instance || target.Kind != kind || targets[target] ||
			target.SeriesID != o.Targets[0].SeriesID {
			return false
		}
		targets[target] = true
	}
	seenRows := make(map[int64]bool)
	covered := make(map[domain.TargetRef]bool)
	for _, member := range job.Members {
		if member.QueueID <= 0 || seenRows[member.QueueID] || !targets[member.Target] || member.SnapshotGeneration == 0 ||
			member.SnapshotGeneration != job.Members[0].SnapshotGeneration {
			return false
		}
		seenRows[member.QueueID] = true
		covered[member.Target] = true
	}
	return len(covered) == len(targets)
}

func validSignature(signature ReleaseSignature, job domain.Job) bool {
	return signature.TitleFingerprint != "" && signature.QualityFingerprint != "" &&
		signature.IndexerID > 0 && signature.IndexerID == job.Release.IndexerID && signature.Protocol == job.Protocol
}

func validRejection(in RejectInput) bool {
	a, o := in.Attempt, in.Origin
	if a.ID == "" || a.SentAt.IsZero() || a.SentAt.After(in.Now) || a.Operation.Action != domain.ActionReject ||
		a.Operation.Job != o.Job.Ref || a.Operation.Key == "" ||
		!o.VerifiedSource(a.SentAt) || !sameTargets(a.Operation.Targets, o.Targets) {
		return false
	}
	for _, observation := range []domain.Observation{o.Grab.Observation, o.Arr.Observation, o.Client.Observation, in.Before.Observation} {
		if observation.ID == "" || !slices.Contains(a.Preflight, observation.ID) {
			return false
		}
	}
	if !uniqueEvidence(o.Grab.Observation, o.Arr.Observation, o.Client.Observation, in.Before.Observation) {
		return false
	}
	switch a.Acknowledgement.State {
	case "", domain.DispatchUncertain, domain.DispatchAcknowledged:
	default:
		return false
	}
	kinds := make(map[domain.EffectKind]bool)
	for _, effect := range a.Operation.Effects {
		if effect.Target != (domain.TargetRef{}) || kinds[effect.Kind] {
			return false
		}
		kinds[effect.Kind] = true
	}
	return kinds[domain.EffectQueueRemoval] && kinds[domain.EffectBlocklist] &&
		((len(kinds) == 3 && kinds[domain.EffectSourceRetained]) ||
			(len(kinds) == 4 && kinds[domain.EffectClientRemoval] && kinds[domain.EffectDataRemoval]))
}

func originalGrabIDs(origin Original) map[int64]bool {
	ids := make(map[int64]bool, len(origin.Grab.Value))
	for _, record := range origin.Grab.Value {
		ids[record.ID] = true
	}
	return ids
}

func failedHistory(in RejectInput, grabIDs map[int64]bool) int64 {
	o, a, rows := in.Origin, in.Attempt, in.Failed.Value
	evidence := in.Failed.Observation
	if len(rows) != len(o.Targets) ||
		!arrObservation(o, evidence, policy.RejectHistoryReadback, domain.ObservationKnown, in.Now) || !postSend(evidence, a) {
		return 0
	}
	ids := make(map[int64]bool)
	targets := make([]domain.TargetRef, 0, len(rows))
	var firstID int64
	for _, record := range rows {
		if record.ID <= 0 || grabIDs[record.ID] || ids[record.ID] ||
			record.DownloadID != o.Job.DownloadID || record.Signature != o.Grab.Value[0].Signature ||
			!record.At.After(a.SentAt) || record.At.After(evidence.ObservedAt) {
			return 0
		}
		ids[record.ID] = true
		targets = append(targets, record.Target)
		if firstID == 0 || firstID > record.ID {
			firstID = record.ID
		}
	}
	if !sameTargets(targets, o.Targets) {
		return 0
	}
	return firstID
}

func insertedBlocklist(in RejectInput) bool {
	o, a := in.Origin, in.Attempt
	before, after := in.Before.Observation, in.After.Observation
	if len(in.Before.Value) > maxTargets || len(in.After.Value) > maxTargets ||
		(before.State != domain.ObservationKnown && before.State != domain.ObservationAbsent) ||
		!arrObservation(o, before, policy.RejectBlocklistReadback, before.State, a.SentAt) ||
		!arrObservation(o, after, policy.RejectBlocklistReadback, domain.ObservationKnown, in.Now) || !postSend(after, a) ||
		(before.State == domain.ObservationAbsent && len(in.Before.Value) != 0) {
		return false
	}
	ids := make(map[int64]bool, len(in.Before.Value))
	for _, earlier := range in.Before.Value {
		if !validBlocklistEntry(earlier, before) || ids[earlier.ID] || matchesOriginalBlocklist(o, earlier) {
			return false
		}
		ids[earlier.ID] = true
	}
	clear(ids)
	matched := false
	for _, entry := range in.After.Value {
		if !validBlocklistEntry(entry, after) || ids[entry.ID] {
			return false
		}
		ids[entry.ID] = true
		if !matchesOriginalBlocklist(o, entry) {
			continue
		}
		if matched || !entry.At.After(a.SentAt) || slices.ContainsFunc(in.Before.Value, func(earlier BlocklistEntry) bool {
			return entry.ID == earlier.ID
		}) {
			return false
		}
		matched = true
	}
	return matched
}

func validBlocklistEntry(entry BlocklistEntry, read domain.Observation) bool {
	return entry.ID > 0 && !entry.At.IsZero() && !entry.At.After(read.ObservedAt)
}

func matchesOriginalBlocklist(origin Original, entry BlocklistEntry) bool {
	return entry.Signature == origin.Grab.Value[0].Signature && sameTargets(entry.Targets, origin.Targets)
}

func rejectOutcome(in RejectInput, read policy.RejectReadback) domain.Outcome {
	a := in.Attempt
	out := domain.Outcome{AttemptID: a.ID, ObservedAt: in.Now, Verification: domain.VerificationUnresolved}
	proofs := map[domain.EffectKind]domain.Observation{
		domain.EffectQueueRemoval: read.Queue, domain.EffectBlocklist: read.Blocklist,
		domain.EffectSourceRetained: read.SourceRetained, domain.EffectClientRemoval: read.ClientRemoval,
		domain.EffectDataRemoval: read.DataRemoval,
	}
	observations := []domain.Observation{read.Queue, read.FailedHistory, read.Blocklist, read.SourceRetained,
		read.ClientRemoval, read.DataRemoval, in.Before.Observation, in.SourceBefore, in.Origin.Grab.Observation,
		in.Origin.Arr.Observation, in.Origin.Client.Observation}
	attributable := a.Acknowledgement.State == domain.DispatchAcknowledged
	failed := read.FailedHistory.ID != "" && evidenceCount(observations, read.FailedHistory.ID) == 1
	contradictRetain := read.ClientRemoval.ID != "" && slices.Contains(a.Operation.Effects, domain.Effect{Kind: domain.EffectSourceRetained})
	// Partial results allow the journal to keep its reconciliation fence while the
	// evidence is incomplete or attribution is pending.
	known, verified := 0, 0
	for _, effect := range a.Operation.Effects {
		proof := proofs[effect.Kind]
		result := domain.EffectResult{Effect: effect, Verification: domain.VerificationUnresolved}
		if proof.ID != "" && evidenceCount(observations, proof.ID) == 1 {
			result.Evidence = []domain.EvidenceID{proof.ID}
			result.Verification = domain.VerificationPartial
			if attributable && !contradictRetain && (effect.Kind == domain.EffectSourceRetained || failed) {
				result.Verification = domain.VerificationEffect
				verified++
			}
			known++
		}
		out.Effects = append(out.Effects, result)
	}
	switch {
	case verified == len(out.Effects):
		out.Verification = domain.VerificationEffect
	case known > 0:
		out.Verification = domain.VerificationPartial
	}
	return out
}
