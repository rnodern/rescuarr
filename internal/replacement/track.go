package replacement

import (
	"fmt"
	"slices"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
)

const (
	CommandReadback    domain.ObservationKind = "scoped_search_command"
	ReplacementGrab    domain.ObservationKind = "replacement_grab"
	ReplacementFile    domain.ObservationKind = "replacement_library_file"
	ReplacementImport  domain.ObservationKind = "replacement_import_history"
	ReplacementProfile domain.ObservationKind = "replacement_current_profile"
)

type CommandState string

const (
	CommandPending   CommandState = "pending"
	CommandCompleted CommandState = "completed"
	CommandFailed    CommandState = "failed"
)

type SearchCommand struct {
	Reference string
	Name      string
	Targets   []domain.TargetRef
	State     CommandState
}

type Grab struct {
	Record
	Job      domain.JobRef
	Release  domain.ReleaseIdentity
	Profile  domain.Revision
	Eligible bool // authoritative current Arr profile eligibility
}

type Import struct {
	Record
	FileID int64
}

type SearchTarget struct {
	Target        domain.TargetRef
	Profile       domain.Fact[domain.Revision]
	PriorFileID   int64
	CurrentFileID domain.Fact[int64]
	Grabs         domain.Fact[[]Grab]
	Imports       domain.Fact[[]Import]
}

// SearchInput follows an exact target set after an explicit search or an Arr-owned rejection.
type SearchInput struct {
	Now     time.Time
	Origin  Original
	Attempt domain.Attempt
	Command domain.Fact[SearchCommand]
	Targets []SearchTarget
}

type SearchProgress struct {
	Command CommandState
	Targets []TargetProgress
}

type TargetProgress struct {
	Target domain.TargetRef
	Grab   domain.Fact[Grab]
	Import domain.EffectResult
}

// TrackSearch reports independently observed replacement imports. Available
// records cannot prove that this command, rather than Arr or another operator,
// caused the new grab. Any competing eligible grab leaves the target unresolved.
func TrackSearch(in SearchInput) SearchProgress {
	if in.Attempt.Operation.Action != domain.ActionSearch || !exactSearchEffects(in.Attempt.Operation) {
		return SearchProgress{}
	}
	return trackSearch(in, true)
}

// TrackArrReplacement follows Arr-owned work from the rejection timestamp. It
// reports replacement effects from exact target evidence without a command reference.
func TrackArrReplacement(in SearchInput) SearchProgress {
	op := in.Attempt.Operation
	if op.Action != domain.ActionReject || !exactRejectEffects(op.Effects) ||
		!slices.Contains(op.Effects, domain.Effect{Kind: domain.EffectSearchDispatch}) || !sameTargets(op.Targets, in.Origin.Targets) {
		return SearchProgress{}
	}
	return trackSearch(in, false)
}

func trackSearch(in SearchInput, explicit bool) SearchProgress {
	o, a := in.Origin, in.Attempt
	var out SearchProgress
	captured := o.Grab.Observation.ObservedAt
	if _, ok := o.exactGrab(); !ok || a.ID == "" || a.Operation.Key == "" || a.SentAt.IsZero() || !a.SentAt.Before(in.Now) ||
		captured.After(a.SentAt) || !arrObservation(o, o.Grab.Observation, GrabHistory, domain.ObservationKnown, captured) ||
		a.Operation.Job != o.Job.Ref || a.Acknowledgement.State == domain.DispatchRejectedBeforeEffect ||
		len(a.Operation.Targets) == 0 || len(a.Operation.Targets) > maxTargets || len(in.Targets) != len(a.Operation.Targets) {
		return out
	}
	certifiedTargets := make([]domain.TargetRef, 0, len(in.Targets))
	for _, target := range in.Targets {
		if !slices.Contains(o.Targets, target.Target) {
			return out
		}
		certifiedTargets = append(certifiedTargets, target.Target)
	}
	if !sameTargets(certifiedTargets, a.Operation.Targets) {
		return out
	}
	var completedCommand domain.Observation
	if explicit && qualifiedCommand(in) {
		out.Command = in.Command.Value.State
		if out.Command == CommandCompleted {
			completedCommand = in.Command.Observation
		}
	}
	for _, target := range in.Targets {
		out.Targets = append(out.Targets, trackTarget(o, a, target, in.Now, completedCommand))
	}
	return out
}

func exactSearchEffects(operation domain.Operation) bool {
	if len(operation.Targets) == 0 || len(operation.Targets) > maxTargets || len(operation.Effects) > 2*len(operation.Targets) {
		return false
	}
	effects := make(map[domain.Effect]bool, len(operation.Effects))
	for _, effect := range operation.Effects {
		if (effect.Kind != domain.EffectSearchDispatch && effect.Kind != domain.EffectReplacementGrab) ||
			!slices.Contains(operation.Targets, effect.Target) || effects[effect] {
			return false
		}
		effects[effect] = true
	}
	for _, target := range operation.Targets {
		if !effects[domain.Effect{Kind: domain.EffectSearchDispatch, Target: target}] {
			return false
		}
	}
	return true
}

func qualifiedCommand(in SearchInput) bool {
	command, observation, a := in.Command.Value, in.Command.Observation, in.Attempt
	name := "EpisodeSearch"
	if in.Origin.Product == domain.ProductRadarr {
		name = "MoviesSearch"
	}
	if a.Acknowledgement.State != domain.DispatchAcknowledged || command.Name != name ||
		command.Reference == "" || command.Reference != a.Acknowledgement.UpstreamReference ||
		!sameTargets(command.Targets, a.Operation.Targets) || !postSend(observation, a) {
		return false
	}
	switch command.State {
	case CommandPending, CommandCompleted, CommandFailed:
		return scopedObservation(observation, domain.Scope{Kind: domain.ScopeInstance, Instance: in.Origin.Job.Ref.Instance},
			domain.Source(in.Origin.Product), CommandReadback, domain.ObservationKnown, command.Reference, in.Now) &&
			sameReadContract(observation, in.Origin.Grab.Observation) && uniqueEvidence(observation, in.Origin.Grab.Observation)
	}
	return false
}

func trackTarget(o Original, a domain.Attempt, t SearchTarget, now time.Time, completedCommand domain.Observation) TargetProgress {
	out := TargetProgress{Target: t.Target, Import: domain.EffectResult{
		Effect: domain.Effect{Kind: domain.EffectLibraryImport, Target: t.Target}, Verification: domain.VerificationUnresolved,
	}}
	gobs, pobs := t.Grabs.Observation, t.Profile.Observation
	arrScope, source := domain.Scope{Kind: domain.ScopeInstance, Instance: o.Job.Ref.Instance}, domain.Source(o.Product)
	targetRecord := fmt.Sprintf("%s:%d", t.Target.Kind, t.Target.ID)
	if !postSend(gobs, a) {
		return out
	}
	if len(t.Grabs.Value) == 0 && !gobs.ObservedAt.Before(completedCommand.ObservedAt) &&
		scopedObservation(gobs, arrScope, source, ReplacementGrab, domain.ObservationAbsent, targetRecord, now) &&
		sameReadContract(gobs, o.Grab.Observation) && uniqueEvidence(o.Grab.Observation, completedCommand, gobs) {
		out.Grab.Observation = gobs
		return out
	}
	if t.Profile.Value == 0 || t.PriorFileID < 0 || len(t.Grabs.Value) != 1 ||
		!scopedObservation(gobs, arrScope, source, ReplacementGrab, domain.ObservationKnown, targetRecord, now) ||
		!scopedObservation(pobs, arrScope, source, ReplacementProfile, domain.ObservationKnown, targetRecord, now) ||
		pobs.ObservedAt.Before(gobs.ObservedAt) || !sameReadContract(gobs, o.Grab.Observation) || !sameReadContract(pobs, gobs) ||
		!uniqueEvidence(o.Grab.Observation, gobs, pobs) {
		return out
	}
	grab := t.Grabs.Value[0]
	if !grab.Eligible || grab.Profile != t.Profile.Value || grab.Target != t.Target || grab.Job == o.Job.Ref ||
		grab.Job.Instance != o.Job.Ref.Instance || grab.Job.ID == "" || grab.Job.ID == o.Job.Ref.ID ||
		grab.Job.Generation == 0 || grab.ID <= 0 || originalHistoryID(o, grab.ID) ||
		grab.ID != grab.Release.GrabHistoryID || grab.Release.GUID == "" || grab.Release.GUID == o.Job.Release.GUID ||
		grab.DownloadID == "" || grab.DownloadID == o.Job.DownloadID || grab.Signature.IndexerID <= 0 ||
		grab.Signature.IndexerID != grab.Release.IndexerID || grab.Signature.Protocol != o.Job.Protocol ||
		grab.Signature.TitleFingerprint == "" || grab.Signature.QualityFingerprint == "" ||
		grab.Signature == o.Grab.Value[0].Signature ||
		!grab.At.After(a.SentAt) || grab.At.After(gobs.ObservedAt) {
		return out
	}
	out.Grab = domain.Fact[Grab]{Observation: gobs, Value: grab}
	if len(t.Imports.Value) != 1 || t.CurrentFileID.Value <= 0 || t.CurrentFileID.Value == t.PriorFileID {
		return out
	}
	imported, iobs, fobs := t.Imports.Value[0], t.Imports.Observation, t.CurrentFileID.Observation
	if imported.ID <= 0 || imported.ID == grab.ID || originalHistoryID(o, imported.ID) || imported.DownloadID != grab.DownloadID || imported.Target != grab.Target ||
		imported.Signature != grab.Signature || imported.FileID != t.CurrentFileID.Value ||
		!imported.At.After(grab.At) || imported.At.After(iobs.ObservedAt) || fobs.ObservedAt.Before(imported.At) ||
		!scopedObservation(iobs, arrScope, source, ReplacementImport, domain.ObservationKnown, grab.DownloadID, now) ||
		!scopedObservation(fobs, arrScope, source, ReplacementFile, domain.ObservationKnown, targetRecord, now) ||
		!sameReadContract(iobs, gobs) || !sameReadContract(fobs, gobs) || pobs.ObservedAt.Before(fobs.ObservedAt) ||
		!uniqueEvidence(o.Grab.Observation, gobs, pobs, iobs, fobs) {
		return out
	}
	out.Import.Verification = domain.VerificationExternal
	out.Import.Evidence = []domain.EvidenceID{gobs.ID, iobs.ID, fobs.ID, pobs.ID}
	return out
}

func originalHistoryID(o Original, id int64) bool {
	return slices.ContainsFunc(o.Grab.Value, func(record Record) bool { return record.ID == id })
}
