package observeservice

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/observe"
)

const closedRetention = 30 * 24 * time.Hour

// A job group is scoped to one installation/epoch and exact upstream download
// ID, protocol and client label. A label supports grouping only, never shared
// physical ownership. Missing IDs remain unique to the current observation.
type jobKey struct {
	Download string
	Protocol domain.Protocol
	Client   string
	Row      int64
	Scan     uint64
	Target   int64
}

type jobGroup struct {
	key  jobKey
	rows []observe.QueueRecord
}

type storedCase struct {
	ID          domain.CaseID
	Fingerprint string
	Payload     string
	Revision    domain.Revision
	Misses      int
}

func persistCases(ctx context.Context, tx *sql.Tx, instance domain.Instance, snapshot observe.QueueSnapshot, generation uint64,
	now time.Time, coverage domain.ObservationState) error {
	isComplete := coverage == domain.ObservationKnown
	if isComplete {
		if _, err := tx.ExecContext(ctx, `DELETE FROM observe_cases WHERE instance_id = ? AND state = ? AND updated_at < ?`,
			instance.Ref.ID, domain.CaseClosed, stamp(now.Add(-closedRetention))); err != nil {
			return err
		}
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM observe_cases WHERE instance_id = ?`, instance.Ref.ID).Scan(&count); err != nil {
		return err
	}
	groups := make(map[jobKey][]observe.QueueRecord)
	for _, row := range snapshot.Records {
		key := jobKey{Download: row.DownloadID, Protocol: row.Protocol, Client: row.ClientLabel}
		if row.DownloadID == "" || row.ClientLabel == "" || row.Protocol == domain.ProtocolUnknown {
			key.Row, key.Scan = row.ID, generation
		}
		if instance.Product == domain.ProductRadarr && row.Target.Valid() {
			key.Target = row.Target.ID
		}
		groups[key] = append(groups[key], row)
	}
	ordered := make([]jobGroup, 0, len(groups))
	for key, rows := range groups {
		ordered = append(ordered, jobGroup{key, rows})
	}
	slices.SortFunc(ordered, func(a, b jobGroup) int {
		return cmp.Or(cmp.Compare(a.key.Download, b.key.Download), cmp.Compare(a.key.Client, b.key.Client),
			cmp.Compare(a.key.Protocol, b.key.Protocol), cmp.Compare(a.key.Row, b.key.Row), cmp.Compare(a.key.Target, b.key.Target))
	})
	for _, group := range ordered {
		if err := persistGroup(ctx, tx, instance, group, snapshot.Evidence, generation, now, isComplete, &count); err != nil {
			return err
		}
	}
	if count >= maxCases {
		if _, err := tx.ExecContext(ctx, `UPDATE observe_instances SET health = 'projection_limit', coverage = 'partial' WHERE id = ?`, instance.Ref.ID); err != nil {
			return err
		}
	}
	if !isComplete {
		return nil
	}
	return markMissing(ctx, tx, instance.Ref, generation, now)
}

func persistGroup(ctx context.Context, tx *sql.Tx, instance domain.Instance, group jobGroup, evidence domain.Observation,
	generation uint64, now time.Time, isComplete bool, count *int) error {
	key, err := hashJSON(group.key)
	if err != nil {
		return err
	}
	previous, err := loadGroup(ctx, tx, instance.Ref, key)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) && *count >= maxCases {
		return nil
	}
	item := domain.Case{ID: previous.ID, Revision: previous.Revision, NextCheckAt: now.Add(45 * time.Second)}
	if len(previous.Payload) > 0 && !isComplete {
		// Partial positives refresh visibility, never erase a known member set or
		// provide absence. Keep the existing state and projection unchanged.
		_, err := tx.ExecContext(ctx, `UPDATE observe_cases SET last_seen_generation = ?, missing_complete_scans = 0,
			updated_at = ? WHERE id = ?`, generation, stamp(now), previous.ID)
		return err
	}
	if item.ID == "" {
		item.ID, item.Revision = domain.CaseID(newCaseID(instance.Ref, key)), 1
	}
	slices.SortFunc(group.rows, func(a, b observe.QueueRecord) int { return cmp.Compare(a.ID, b.ID) })
	caseGeneration := domain.Revision(1)
	if len(previous.Payload) > 0 {
		var old domain.Case
		if err := json.Unmarshal([]byte(previous.Payload), &old); err != nil {
			return err
		}
		caseGeneration = old.Job.Ref.Generation
		if contradictoryAliases(old.Job.Members, group.rows) {
			// An opaque ID can be recycled. Preserve the prior case and quarantine
			// the new local generation instead of inheriting that lineage.
			if err := retireConflictingGroup(ctx, tx, old, key, now); err != nil {
				return err
			}
			caseGeneration++
			item.ID = domain.CaseID(newCaseID(instance.Ref, key+"-"+strconv.FormatUint(uint64(caseGeneration), 10)))
			item.Revision = 1
			previous = storedCase{}
			if *count >= maxCases {
				return nil
			}
		}
	}
	item.State, item.Reason = summarise(group.rows)
	if caseGeneration > 1 {
		item.State, item.Reason = domain.CaseHeld, "job_id_reused"
	}
	smallGroup := len(group.rows) <= maxGroup
	if !smallGroup {
		group.rows = group.rows[:maxGroup]
	}
	// Exact upstream tokens remain server-only correlation inputs. The console
	// payload uses the locally derived job ID, never indexer or path metadata.
	item.Job = domain.Job{Ref: domain.JobRef{Instance: instance.Ref, ID: domain.JobID(item.ID), Generation: caseGeneration},
		Protocol: group.key.Protocol, CompleteSet: isComplete && smallGroup && group.key.Row == 0 && caseGeneration == 1}
	if evidence.ID != "" {
		item.Evidence, item.Job.Evidence = []domain.EvidenceID{evidence.ID}, []domain.EvidenceID{evidence.ID}
	}
	seen := make(map[domain.TargetRef]bool)
	for _, row := range group.rows {
		item.Job.Members = append(item.Job.Members, domain.QueueMember{QueueID: row.ID, SnapshotGeneration: domain.Revision(generation), Target: row.Target})
		if !row.Target.Valid() {
			item.Job.CompleteSet = false
			continue
		}
		if !seen[row.Target] {
			seen[row.Target] = true
			item.Targets = append(item.Targets, domain.Target{Ref: row.Target})
		}
	}
	slices.SortFunc(item.Targets, func(a, b domain.Target) int {
		return cmp.Or(cmp.Compare(a.Ref.Kind, b.Ref.Kind), cmp.Compare(a.Ref.SeriesID, b.Ref.SeriesID), cmp.Compare(a.Ref.ID, b.Ref.ID))
	})
	if !item.Job.CompleteSet && item.State != domain.CaseWaiting && caseGeneration == 1 {
		item.State, item.Reason = domain.CaseHeld, "target_ambiguous"
	}
	fingerprint, err := materialFingerprint(item)
	if err != nil {
		return err
	}
	if previous.ID != "" && fingerprint != previous.Fingerprint {
		item.Revision++
	}
	if previous.ID == "" {
		*count = *count + 1
	}
	payload, err := json.Marshal(item)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO observe_cases (id, instance_id, epoch, correlation_key, fingerprint, state, reason,
		revision, payload, last_seen_generation, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET fingerprint = excluded.fingerprint, state = excluded.state, reason = excluded.reason,
			revision = excluded.revision, payload = excluded.payload, last_seen_generation = excluded.last_seen_generation,
			missing_complete_scans = 0, updated_at = excluded.updated_at`, item.ID,
		instance.Ref.ID, instance.Ref.Epoch, key, fingerprint, item.State, item.Reason, item.Revision, string(payload), generation, stamp(now))
	return err
}

func loadGroup(ctx context.Context, tx *sql.Tx, instance domain.InstanceRef, key string) (storedCase, error) {
	var item storedCase
	err := tx.QueryRowContext(ctx, `SELECT id, fingerprint, payload, revision
		FROM observe_cases WHERE instance_id = ? AND epoch = ? AND correlation_key = ?`, instance.ID, instance.Epoch, key).
		Scan(&item.ID, &item.Fingerprint, &item.Payload, &item.Revision)
	return item, err
}

func contradictoryAliases(previous []domain.QueueMember, next []observe.QueueRecord) bool {
	known := make(map[int64]domain.TargetRef, len(previous))
	for _, member := range previous {
		if member.Target.Valid() {
			known[member.QueueID] = member.Target
		}
	}
	for _, row := range next {
		if target, found := known[row.ID]; found && row.Target.Valid() && target != row.Target {
			return true
		}
	}
	return false
}

func retireConflictingGroup(ctx context.Context, tx *sql.Tx, detail domain.Case, key string, now time.Time) error {
	detail.State, detail.Reason, detail.Revision = domain.CaseClosed, "job_identity_changed", detail.Revision+1
	detail.Job.CompleteSet = false
	payload, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE observe_cases SET correlation_key = ?, state = ?, reason = ?, revision = ?, payload = ?, updated_at = ? WHERE id = ?`,
		key+"-retired-"+strconv.FormatUint(uint64(detail.Job.Ref.Generation), 10), detail.State, detail.Reason, detail.Revision, string(payload), stamp(now), detail.ID)
	return err
}

func markMissing(ctx context.Context, tx *sql.Tx, instance domain.InstanceRef, generation uint64, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, payload, missing_complete_scans FROM observe_cases
		WHERE instance_id = ? AND epoch = ? AND state <> 'closed' AND last_seen_generation < ? LIMIT ?`, instance.ID, instance.Epoch, generation, maxCases)
	if err != nil {
		return err
	}
	var cases []storedCase
	for rows.Next() {
		var item storedCase
		if err := rows.Scan(&item.ID, &item.Payload, &item.Misses); err != nil {
			rows.Close()
			return err
		}
		cases = append(cases, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, item := range cases {
		var detail domain.Case
		if err := json.Unmarshal([]byte(item.Payload), &detail); err != nil {
			return err
		}
		state, reason := domain.CaseHeld, domain.ReasonCode("queue_presence_unknown")
		if item.Misses >= 1 {
			state, reason = domain.CaseClosed, "queue_missing_unverified"
		}
		detail.State, detail.Reason, detail.Revision, detail.Job.CompleteSet = state, reason, detail.Revision+1, false
		detail.NextCheckAt = now.Add(45 * time.Second)
		payload, err := json.Marshal(detail)
		if err != nil {
			return err
		}
		fingerprint, err := materialFingerprint(detail)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE observe_cases SET state = ?, reason = ?, revision = ?, payload = ?, fingerprint = ?,
			missing_complete_scans = ?, updated_at = ? WHERE id = ?`, state, reason, detail.Revision, string(payload), fingerprint, item.Misses+1, stamp(now), item.ID)
		if err != nil {
			return err
		}
	}
	return nil
}

func summarise(rows []observe.QueueRecord) (domain.CaseState, domain.ReasonCode) {
	active, failed, noEligible, warning, complete := false, false, false, false, false
	for _, row := range rows {
		active = active || row.Status == "downloading" || row.Status == "queued" || row.TrackedState == "importing"
		failed = failed || row.Status == "failed" || row.TrackedState == "downloadFailed"
		noEligible = noEligible || slices.Contains(row.Findings, observe.FindingNoEligibleFiles)
		warning = warning || len(row.Findings) > 0 || row.TrackedStatus == "warning" || row.TrackedStatus == "error"
		complete = complete || row.Status == "completed" || row.TrackedState == "importPending"
	}
	switch {
	case active:
		return domain.CaseWaiting, "upstream_processing"
	case failed:
		return domain.CaseHeld, "download_failed"
	case noEligible:
		return domain.CaseHeld, "no_eligible_files"
	case warning:
		return domain.CaseHeld, "upstream_intervention_required"
	case complete:
		return domain.CaseObserved, "awaiting_import_assessment"
	default:
		return domain.CaseObserved, "queue_state_unknown"
	}
}

func materialFingerprint(item domain.Case) (string, error) {
	members := slices.Clone(item.Job.Members)
	for index := range members {
		members[index].SnapshotGeneration = 0
	}
	return hashJSON(struct {
		State   domain.CaseState
		Reason  domain.ReasonCode
		Members []domain.QueueMember
		Targets []domain.Target
		Full    bool
	}{item.State, item.Reason, members, item.Targets, item.Job.CompleteSet})
}

func newCaseID(instance domain.InstanceRef, key string) string {
	hash, _ := hashJSON(struct {
		Instance domain.InstanceRef
		Key      string
	}{instance, key})
	return hash[:32]
}

func hashJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}
