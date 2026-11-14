package recoveryservice

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/arradapter"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/observe"
	"github.com/rnodern/rescuarr/internal/observeservice"
	"github.com/rnodern/rescuarr/internal/store"
)

type emptyPackEffects interface {
	Preflight(context.Context, arradapter.EmptyPackScope) error
	Reject(context.Context, arradapter.EmptyPackScope) error
	VerifyRejected(context.Context, arradapter.EmptyPackScope, time.Time) (bool, error)
	Search(context.Context, arradapter.EmptyPackScope, []int64) (int64, error)
}

type emptyPackReader interface {
	SonarrEpisodeNeeds(context.Context, int64, []int64, time.Time) (map[int64]observe.EpisodeNeed, error)
	Queue(context.Context, domain.Revision, time.Time) (observe.QueueSnapshot, error)
}

// emptyPackWorker processes one completed no-files download at a time. The
// connection factory supplies both qualified effects and independent reads.
type emptyPackWorker struct {
	cases      *observeservice.Service
	journal    *EmptyPackJournal
	clock      app.Clock
	connection func(domain.Instance) (emptyPackEffects, emptyPackReader, error)
	after      domain.CaseID
	instance   domain.InstanceRef
}

// RunEmptyPackRecovery runs the background worker on an explicitly qualified
// connection provider. It does not start when the caller has no verified route.
func RunEmptyPackRecovery(ctx context.Context, cases *observeservice.Service, local *store.Store, clock app.Clock, instance domain.InstanceRef,
	transport http.RoundTripper, qualified func(domain.Instance) (*arradapter.EmptyPackAdapter, error), onError func(error)) error {
	if cases == nil || local == nil || clock == nil || !instance.Valid() || transport == nil || qualified == nil {
		return arradapter.ErrQualification
	}
	worker := &emptyPackWorker{
		cases: cases, journal: NewEmptyPackJournal(local), clock: clock, instance: instance,
		connection: func(instance domain.Instance) (emptyPackEffects, emptyPackReader, error) {
			effects, err := qualified(instance)
			if err != nil {
				return nil, nil, err
			}
			if effects == nil || !effects.MatchesInstance(instance) {
				return nil, nil, arradapter.ErrQualification
			}
			key, err := local.ReadSecret(instance.SecretRef)
			if err != nil {
				return nil, nil, observe.ErrAccess
			}
			reader, err := observe.NewArr(observe.Config{Instance: instance, APIKey: key}, transport)
			return effects, reader, err
		},
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			pass, cancel := context.WithTimeout(ctx, 50*time.Second)
			err := worker.step(pass)
			cancel()
			if err != nil && ctx.Err() == nil && onError != nil {
				onError(err)
			}
			timer.Reset(5 * time.Second)
		}
	}
}

func (w *emptyPackWorker) enabled(ctx context.Context) (bool, error) {
	var enabled bool
	err := w.journal.local.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT mode = 'autonomous' AND NOT reconcile_required AND NOT paused
			FROM app_state JOIN action_pause USING (singleton) WHERE singleton = 1`).Scan(&enabled)
	})
	return enabled, err
}

func (w *emptyPackWorker) step(ctx context.Context) error {
	if allowed, err := w.enabled(ctx); !allowed || err != nil {
		return err
	}
	pending, err := w.journal.Pending(ctx, 128)
	if err != nil {
		return err
	}
	var pendingErrors error
	for _, checkpoint := range pending {
		if w.instance.Valid() && w.instance != checkpoint.Key.Job.Instance {
			continue
		}
		if ctx.Err() != nil {
			return errors.Join(pendingErrors, ctx.Err())
		}
		pendingErrors = errors.Join(pendingErrors, w.resume(ctx, checkpoint))
	}
	cases, err := w.cases.AutomaticEmptyPackCasesFor(ctx, w.instance.ID, w.after, 1)
	if err != nil {
		return errors.Join(pendingErrors, err)
	}
	if len(cases) == 0 {
		w.after = ""
		return pendingErrors
	}
	item := cases[0]
	w.after = item.ID
	return errors.Join(pendingErrors, w.start(ctx, item))
}

func (w *emptyPackWorker) start(ctx context.Context, item domain.Case) error {
	key := EmptyPackKey{CaseID: item.ID, Job: item.Job.Ref}
	if _, exists, err := w.journal.Load(ctx, key); exists || err != nil {
		return err
	}
	proof, err := w.cases.FreshRecoveryQueue(ctx, item.ID, item.Revision)
	if errors.Is(err, app.ErrHeld) || errors.Is(err, app.ErrConflict) || errors.Is(err, observeservice.ErrLimit) {
		return nil
	}
	if err != nil {
		return err
	}
	seriesID, ids, exact := missingFileTargets(proof)
	if !exact || len(ids) < 2 || proof.Case.Job.Protocol != domain.ProtocolUsenet || proof.Case.Job.Shared || proof.Case.LastSeenAt.IsZero() {
		return nil
	}
	release, err := w.cases.HoldRecoveryConnection(ctx, proof.Instance)
	if err != nil {
		return err
	}
	defer release()
	first := proof.Members[0]
	checkpoint := EmptyPackCheckpoint{
		Key: key, ConnectionRevision: proof.Instance.ConnectionRevision, SeriesID: seriesID,
		Release: first.Release, DownloadID: first.DownloadID, EpisodeIDs: ids,
	}
	for _, row := range proof.Members {
		checkpoint.QueueIDs = append(checkpoint.QueueIDs, row.ID)
	}
	slices.Sort(checkpoint.QueueIDs)
	slices.Sort(checkpoint.EpisodeIDs)
	effects, reader, err := w.connection(proof.Instance)
	if err != nil {
		return err
	}
	now := w.clock.Now()
	needs, err := reader.SonarrEpisodeNeeds(ctx, seriesID, ids, now)
	if err != nil {
		return err
	}
	if replacementPlan(proof, needs).UnclearCount != 0 {
		return nil
	}
	scope := emptyPackScope(checkpoint)
	if err := effects.Preflight(ctx, scope); err != nil {
		return err
	}
	ok, err := w.journal.BeginReject(ctx, key, checkpoint, w.clock.Now())
	if !ok || err != nil {
		return err
	}
	// Even an unknown response must be reconciled through independent reads.
	err = effects.Reject(ctx, scope)
	return w.cancelIfUnsent(ctx, key, EmptyPackRejectSending, err)
}

func (w *emptyPackWorker) resume(ctx context.Context, cp EmptyPackCheckpoint) error {
	instance, err := w.cases.RecoveryInstance(ctx, cp.Key.Job.Instance, cp.ConnectionRevision)
	if errors.Is(err, app.ErrHeld) {
		return nil
	}
	if err != nil {
		return err
	}
	release, err := w.cases.HoldRecoveryConnection(ctx, instance)
	if err != nil {
		return err
	}
	defer release()
	effects, reader, err := w.connection(instance)
	if err != nil {
		return err
	}
	scope := emptyPackScope(cp)
	verified, err := effects.VerifyRejected(ctx, scope, cp.StartedAt)
	if err != nil || !verified {
		return err
	}
	if cp.Stage == EmptyPackRejectSending {
		ok, err := w.journal.Advance(ctx, cp.Key, cp.Stage, EmptyPackCheckpoint{Stage: EmptyPackRejectVerified}, w.clock.Now())
		if err != nil || !ok {
			return err
		}
		cp.Stage = EmptyPackRejectVerified
	}
	return w.search(ctx, effects, reader, cp)
}

func (w *emptyPackWorker) search(ctx context.Context, effects emptyPackEffects, reader emptyPackReader, cp EmptyPackCheckpoint) error {
	needs, err := reader.SonarrEpisodeNeeds(ctx, cp.SeriesID, cp.EpisodeIDs, w.clock.Now())
	if err != nil {
		return err
	}
	now := w.clock.Now()
	queue, err := reader.Queue(ctx, 1, now)
	if err != nil || !queue.Evidence.Fresh(w.clock.Now()) {
		return err
	}
	for _, row := range queue.Records {
		if row.DownloadID == cp.DownloadID || slices.Contains(cp.QueueIDs, row.ID) {
			return nil
		}
	}
	item := domain.Case{}
	for _, id := range cp.EpisodeIDs {
		item.Targets = append(item.Targets, domain.Target{Ref: domain.TargetRef{
			Instance: cp.Key.Job.Instance, Kind: domain.TargetEpisode, ID: id, SeriesID: cp.SeriesID,
		}})
	}
	plan := planReplacement(item, queue, cp.DownloadID, cp.SeriesID, cp.Release, needs)
	if plan.UnclearCount != 0 {
		return nil
	}
	if plan.SearchCount == 0 {
		_, err := w.journal.Advance(ctx, cp.Key, cp.Stage, EmptyPackCheckpoint{Stage: EmptyPackDone}, w.clock.Now())
		return err
	}
	var wanted []int64
	for _, episode := range plan.Episodes {
		if episode.Status == app.ReplacementSearch {
			wanted = append(wanted, episode.Target.ID)
		}
	}
	ok, err := w.journal.Advance(ctx, cp.Key, cp.Stage, EmptyPackCheckpoint{Stage: EmptyPackSearchSending, SearchIDs: wanted}, w.clock.Now())
	if err != nil || !ok {
		return err
	}
	commandID, err := effects.Search(ctx, emptyPackScope(cp), wanted)
	if err != nil {
		return w.cancelIfUnsent(ctx, cp.Key, EmptyPackSearchSending, err)
	}
	_, err = w.journal.Advance(ctx, cp.Key, EmptyPackSearchSending, EmptyPackCheckpoint{Stage: EmptyPackDone, CommandID: commandID}, w.clock.Now())
	return err
}

// The adapter reserves ErrUncertain for anything that reached transport.
// Only a known-unsent preflight failure can release a checkpoint.
func (w *emptyPackWorker) cancelIfUnsent(ctx context.Context, key EmptyPackKey, stage EmptyPackStage, sendErr error) error {
	if !errors.Is(sendErr, arradapter.ErrReadback) && !errors.Is(sendErr, arradapter.ErrPrepared) &&
		!errors.Is(sendErr, arradapter.ErrQualification) && !errors.Is(sendErr, arradapter.ErrConfiguration) {
		return sendErr
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	_, err := w.journal.CancelUnsent(cleanup, key, stage, w.clock.Now())
	return errors.Join(sendErr, err)
}

func emptyPackScope(cp EmptyPackCheckpoint) arradapter.EmptyPackScope {
	return arradapter.EmptyPackScope{QueueIDs: cp.QueueIDs, DownloadID: cp.DownloadID, Release: cp.Release,
		SeriesID: cp.SeriesID, EpisodeIDs: cp.EpisodeIDs}
}
