package replacement

import (
	"slices"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
)

func scopedObservation(observation domain.Observation, scope domain.Scope, source domain.Source,
	kind domain.ObservationKind, state domain.ObservationState, record string, now time.Time) bool {
	return observation.ID != "" && observation.Fresh(now) && observation.Scope == scope &&
		observation.Source == source && observation.Kind == kind && observation.State == state &&
		observation.SourceRecord == record && observation.AdapterRevision != "" &&
		observation.SchemaRevision != 0 && observation.SnapshotGeneration != 0
}

func arrObservation(o Original, observation domain.Observation, kind domain.ObservationKind, state domain.ObservationState, now time.Time) bool {
	return scopedObservation(observation, domain.Scope{Kind: domain.ScopeInstance, Instance: o.Job.Ref.Instance},
		domain.Source(o.Product), kind, state, o.Job.DownloadID, now) && sameReadContract(observation, o.Grab.Observation)
}

func sameReadContract(a, b domain.Observation) bool {
	return a.AdapterRevision == b.AdapterRevision && a.SchemaRevision == b.SchemaRevision
}

func postSend(observation domain.Observation, a domain.Attempt) bool {
	return observation.ObservedAt.After(a.SentAt)
}

func evidenceCount(observations []domain.Observation, id domain.EvidenceID) int {
	count := 0
	for _, observation := range observations {
		if observation.ID == id {
			count++
		}
	}
	return count
}

func uniqueEvidence(observations ...domain.Observation) bool {
	for i, observation := range observations {
		if observation.ID == "" || evidenceCount(observations[:i], observation.ID) != 0 {
			return false
		}
	}
	return true
}

func queueIDs(job domain.Job) []int64 {
	ids := make([]int64, 0, len(job.Members))
	for _, member := range job.Members {
		ids = append(ids, member.QueueID)
	}
	slices.Sort(ids)
	return ids
}

func sameIDs(first, second []int64) bool {
	if len(first) != len(second) {
		return false
	}
	selected := slices.Clone(first)
	slices.Sort(selected)
	return slices.Equal(selected, second)
}

func sameTargets(a, b []domain.TargetRef) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[domain.TargetRef]bool, len(a))
	for _, target := range a {
		if seen[target] || !slices.Contains(b, target) {
			return false
		}
		seen[target] = true
	}
	return true
}
