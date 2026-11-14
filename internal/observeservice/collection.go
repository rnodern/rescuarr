package observeservice

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/observe"
)

const collectTimeout = 40 * time.Second

// Collect coalesces concurrent reads of one instance. External requests always
// occur between short SQLite transactions and never use effectful methods.
func (s *Service) Collect(ctx context.Context, id domain.InstanceID) error {
	if id == "" || len(id) > 128 {
		return app.ErrNotFound
	}
	s.mu.Lock()
	if existing := s.active[id]; existing != nil {
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-existing.done:
			return existing.err
		}
	}
	// Reject rather than accumulate unbounded waiting requests for invalid IDs.
	select {
	case s.parallel <- struct{}{}:
	default:
		s.mu.Unlock()
		return ErrLimit
	}
	current := &pending{done: make(chan struct{})}
	s.active[id] = current
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.active, id)
		<-s.parallel
		close(current.done)
		s.mu.Unlock()
	}()
	current.err = s.collect(ctx, id)
	return current.err
}

func (s *Service) collect(ctx context.Context, id domain.InstanceID) error {
	instance := domain.Instance{Ref: domain.InstanceRef{ID: id}}
	var generation, complete uint64
	localErr := s.store.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT epoch, product, display_name, base_url, secret_ref, connection_revision, version, generation, complete_scans
			FROM observe_instances WHERE id = ?`, id).Scan(&instance.Ref.Epoch, &instance.Product, &instance.DisplayName, &instance.BaseURL,
			&instance.SecretRef, &instance.ConnectionRevision, &instance.UpstreamVersion, &generation, &complete)
	})
	if errors.Is(localErr, sql.ErrNoRows) {
		return app.ErrNotFound
	}
	if localErr != nil {
		return localErr
	}
	generation++
	cycle, cancel := context.WithTimeout(ctx, collectTimeout)
	defer cancel()
	observedAt := s.clock.Now().UTC()
	snapshot, version, checks, upstreamErr := s.observe(cycle, instance, domain.Revision(generation), observedAt, true)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	coverage := domain.ObservationUnknown
	if upstreamErr == nil {
		coverage = domain.ObservationKnown
		complete++
	} else if len(snapshot.Records) > 0 || errors.Is(upstreamErr, observe.ErrPartial) {
		coverage = "partial"
	}
	expectedRevision := instance.ConnectionRevision
	upgrade := version != "" && instance.UpstreamVersion != "" && version != instance.UpstreamVersion
	err := s.store.Write(ctx, func(tx *sql.Tx) error {
		status := healthReason(upstreamErr)
		if upgrade {
			// Keep old cases, discard this read's rows and require an explicit
			// later scan before accepting observations from the changed adapter.
			status = "contract_changed"
			coverage = domain.ObservationUnknown
			if upstreamErr == nil {
				complete--
			}
			instance.ConnectionRevision++
		}
		result, err := tx.ExecContext(ctx, `UPDATE observe_instances SET generation = ?, complete_scans = ?, checked_at = ?,
			health = ?, coverage = ?, version = CASE WHEN ? <> '' THEN ? ELSE version END, connection_revision = ?
			WHERE id = ? AND epoch = ? AND connection_revision = ? AND generation = ?`, generation, complete,
			stamp(observedAt), status, coverage, version, version, instance.ConnectionRevision, id, instance.Ref.Epoch, expectedRevision, generation-1)
		if err != nil {
			return err
		}
		if changed, err := result.RowsAffected(); err != nil || changed != 1 {
			if err == nil {
				err = app.ErrConflict
			}
			return err
		}
		if !upgrade {
			if err := persistCases(ctx, tx, instance, snapshot, generation, observedAt, coverage, checks); err != nil {
				return err
			}
		}
		return bump(ctx, tx)
	})
	if err != nil {
		return err
	}
	if upgrade {
		return observe.ErrResponse
	}
	return upstreamErr
}

func (s *Service) observe(ctx context.Context, instance domain.Instance, generation domain.Revision, now time.Time, checkNames bool) (observe.QueueSnapshot, string, map[int64]domain.EpisodeNameReview, error) {
	key, err := s.store.ReadSecret(instance.SecretRef)
	if err != nil {
		return observe.QueueSnapshot{}, "", nil, observe.ErrAccess
	}
	arr, err := observe.NewArr(observe.Config{Instance: instance, APIKey: key}, s.transport)
	if err != nil {
		return observe.QueueSnapshot{}, "", nil, err
	}
	status, err := arr.Status(ctx)
	if err != nil {
		return observe.QueueSnapshot{}, "", nil, err
	}
	snapshot, err := arr.Queue(ctx, generation, now)
	var checks map[int64]domain.EpisodeNameReview
	if err == nil && checkNames && (instance.UpstreamVersion == "" || instance.UpstreamVersion == status.Version) {
		instance.UpstreamVersion = status.Version
		checks = s.reviewEpisodeNames(ctx, observe.Config{Instance: instance, APIKey: key}, arr, snapshot, status, generation)
	}
	return snapshot, status.Version, checks, err
}

func healthReason(err error) domain.ReasonCode {
	switch {
	case err == nil:
		return "available"
	case errors.Is(err, observe.ErrPartial):
		return "partial_queue"
	case errors.Is(err, observe.ErrAccess):
		return "authentication_failed"
	case errors.Is(err, observe.ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, observe.ErrResponse), errors.Is(err, observe.ErrConfiguration):
		return "unsupported_payload"
	default:
		return "upstream_unavailable"
	}
}

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }
