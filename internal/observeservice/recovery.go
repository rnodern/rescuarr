package observeservice

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/observe"
)

const recoveryCaseAge = 2 * time.Minute

// RecoveryQueue binds a current complete Arr queue to a saved local case.
// It is private input for policy evaluation and does not establish physical
// job ownership or permit a write.
type RecoveryQueue struct {
	Case     domain.Case           `json:"-"`
	Instance domain.Instance       `json:"-"`
	Queue    observe.QueueSnapshot `json:"-"`
	Members  []observe.QueueRecord `json:"-"`
}

type caseAtConnection struct {
	proof          RecoveryQueue
	correlationKey string
	generation     domain.Revision
}

func (s *Service) FreshRecoveryQueue(ctx context.Context, id domain.CaseID, expected domain.Revision) (RecoveryQueue, error) {
	if id == "" || len(id) > 128 || expected == 0 {
		return RecoveryQueue{}, app.ErrNotFound
	}
	baseline, err := s.recoveryCase(ctx, id)
	if err != nil {
		return RecoveryQueue{}, err
	}
	if baseline.proof.Case.Revision != expected {
		return RecoveryQueue{}, app.ErrConflict
	}
	instance := baseline.proof.Instance
	release, err := s.HoldRecoveryConnection(ctx, instance)
	if err != nil {
		return RecoveryQueue{}, err
	}
	defer release()
	// Share the service's global upstream read bound with background collection.
	select {
	case s.parallel <- struct{}{}:
	default:
		return RecoveryQueue{}, ErrLimit
	}
	defer func() { <-s.parallel }()
	ctx, cancel := context.WithTimeout(ctx, collectTimeout)
	defer cancel()
	at := s.clock.Now().UTC()
	generation := baseline.generation + 1
	queue, version, _, _, err := s.observe(ctx, instance, generation, at, false)
	if err != nil {
		return RecoveryQueue{}, err
	}
	if version != instance.UpstreamVersion || !queue.Evidence.Fresh(s.clock.Now().UTC()) {
		return RecoveryQueue{}, app.ErrHeld
	}
	members, err := bindRecoveryMembers(baseline, queue)
	if err != nil {
		return RecoveryQueue{}, err
	}
	current, err := s.recoveryCase(ctx, id)
	if err != nil {
		return RecoveryQueue{}, err
	}
	if current.generation != baseline.generation || current.proof.Case.Revision != expected ||
		current.correlationKey != baseline.correlationKey || current.proof.Instance != instance {
		return RecoveryQueue{}, app.ErrConflict
	}
	baseline.proof.Queue, baseline.proof.Members = queue, members
	for index := range baseline.proof.Case.Job.Members {
		baseline.proof.Case.Job.Members[index].SnapshotGeneration = generation
	}
	return baseline.proof, nil
}

// HoldRecoveryConnection prevents removal during a read of a current Arr connection.
// The caller must release it when the read finishes.
func (s *Service) HoldRecoveryConnection(ctx context.Context, instance domain.Instance) (func(), error) {
	id := instance.Ref.ID
	s.mu.Lock()
	defer s.mu.Unlock()
	var registered bool
	err := s.store.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM observe_instances
			WHERE id = ? AND epoch = ? AND connection_revision = ? AND health = 'available')`,
			id, instance.Ref.Epoch, instance.ConnectionRevision).Scan(&registered)
	})
	if err != nil {
		return nil, err
	}
	if !registered {
		return nil, app.ErrConflict
	}
	s.recoveryReads[id]++
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.recoveryReads[id]--
		if s.recoveryReads[id] == 0 {
			delete(s.recoveryReads, id)
		}
	}, nil
}

func (s *Service) recoveryCase(ctx context.Context, id domain.CaseID) (caseAtConnection, error) {
	var result caseAtConnection
	var payload string
	var generationAtCase domain.Revision
	var checked string
	instance := &result.proof.Instance
	err := s.store.Read(ctx, func(tx *sql.Tx) error {
		const query = `SELECT c.payload, c.correlation_key, c.last_seen_generation,
			i.id, i.epoch, i.product, i.display_name, i.base_url, i.secret_ref, i.connection_revision,
			i.version, i.generation, i.checked_at
			FROM observe_cases AS c JOIN observe_instances AS i ON i.id = c.instance_id AND i.epoch = c.epoch
			WHERE c.id = ? AND c.observation_only = 0 AND c.missing_complete_scans = 0
			AND i.coverage = 'known' AND i.health = 'available'`
		return tx.QueryRowContext(ctx, query, id).Scan(&payload, &result.correlationKey, &generationAtCase,
			&instance.Ref.ID, &instance.Ref.Epoch, &instance.Product, &instance.DisplayName, &instance.BaseURL, &instance.SecretRef,
			&instance.ConnectionRevision, &instance.UpstreamVersion, &result.generation, &checked)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return caseAtConnection{}, app.ErrHeld
	}
	if err != nil {
		return caseAtConnection{}, err
	}
	item := &result.proof.Case
	if err := json.Unmarshal([]byte(payload), item); err != nil {
		return caseAtConnection{}, err
	}
	observedAt, err := time.Parse(time.RFC3339Nano, checked)
	now := s.clock.Now().UTC()
	if err != nil || now.Before(observedAt) || !now.Before(observedAt.Add(recoveryCaseAge)) ||
		instance.UpstreamVersion == "" || result.generation != generationAtCase || item.ID != id ||
		item.Job.Ref.Instance != instance.Ref || item.Job.Ref.Generation != 1 || !item.Job.CompleteSet ||
		len(item.Job.Members) == 0 || len(item.Job.Members) > maxGroup || item.State == domain.CaseClosed {
		return caseAtConnection{}, app.ErrHeld
	}
	return result, nil
}

func bindRecoveryMembers(baseline caseAtConnection, queue observe.QueueSnapshot) ([]observe.QueueRecord, error) {
	item := baseline.proof.Case
	expected := make(map[int64]domain.TargetRef, len(item.Job.Members))
	for _, member := range item.Job.Members {
		if member.QueueID <= 0 || !member.Target.Valid() || member.SnapshotGeneration != baseline.generation ||
			expected[member.QueueID].Valid() {
			return nil, app.ErrHeld
		}
		expected[member.QueueID] = member.Target
	}
	members := make([]observe.QueueRecord, 0, len(expected))
	for _, row := range queue.Records {
		if target, exists := expected[row.ID]; exists {
			if row.Target != target || row.Protocol != item.Job.Protocol || row.DownloadID == "" || row.ClientLabel == "" {
				return nil, app.ErrConflict
			}
			members = append(members, row)
			delete(expected, row.ID)
		}
	}
	if len(expected) != 0 {
		return nil, app.ErrConflict
	}
	key := jobKey{Download: members[0].DownloadID, Protocol: members[0].Protocol, Client: members[0].ClientLabel}
	for _, row := range members {
		if row.DownloadID != key.Download || row.Protocol != key.Protocol || row.ClientLabel != key.Client {
			return nil, app.ErrConflict
		}
	}
	if baseline.proof.Instance.Product == domain.ProductRadarr {
		key.Target = members[0].Target.ID
	}
	correlation, err := hashJSON(key)
	if err != nil {
		return nil, err
	}
	state, reason := summarise(members)
	if correlation != baseline.correlationKey || state != item.State || reason != item.Reason {
		return nil, app.ErrConflict
	}
	// Check the entire snapshot for any extra membership of this exact job,
	// including a second Radarr movie or unexpected protocol or client label.
	count := 0
	for _, row := range queue.Records {
		if row.DownloadID == key.Download {
			if row.Protocol != key.Protocol || row.ClientLabel != key.Client {
				return nil, app.ErrConflict
			}
			count++
		}
	}
	if count != len(members) {
		return nil, app.ErrConflict
	}
	return members, nil
}
