package ledger

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

const (
	schedulerInterval = 5 * time.Second
	initialReadback   = 5 * time.Second
	dispatchSlice     = 2*operationTimeout + 3*journalTimeout
	readbackSlice     = operationTimeout + 2*journalTimeout
)

// Scheduler runs the journal's dispatch and reconciliation separately. The
// executor's gate, adapter and clock must be safe for concurrent calls.
// An executor's dispatch halt remains in force across scheduler runs.
type Scheduler struct {
	executor *Executor
	journal  *Journal
	running  atomic.Bool
}

func NewScheduler(executor *Executor) (*Scheduler, error) {
	if executor == nil {
		return nil, ErrInvalid
	}
	journal, ok := executor.store.(*Journal)
	if !ok || journal == nil {
		return nil, ErrInvalid
	}
	return &Scheduler{executor: executor, journal: journal}, nil
}

// Run checks for work immediately and paces subsequent passes from the end
// of each pass. It waits for both workers' bounded journal cleanup on shutdown.
// onError, if supplied, may be called concurrently by the two workers.
func (s *Scheduler) Run(ctx context.Context, onError func(error)) error {
	if !s.running.CompareAndSwap(false, true) {
		return ErrConflict
	}
	defer s.running.Store(false)
	var cursor dispatchCursor
	var worker sync.WaitGroup
	worker.Go(func() { schedulerLoop(ctx, onError, s.readback) })
	schedulerLoop(ctx, onError, func(ctx context.Context) error { return s.dispatch(ctx, &cursor) })
	worker.Wait()
	return nil
}

func schedulerLoop(ctx context.Context, onError func(error), pass func(context.Context) error) {
	timer := time.NewTimer(schedulerInterval)
	timer.Stop()
	defer timer.Stop()
	for ctx.Err() == nil {
		if err := pass(ctx); err != nil && ctx.Err() == nil {
			if onError != nil {
				onError(err)
			}
			if errors.Is(err, ErrHalted) {
				return
			}
		}
		timer.Reset(schedulerInterval)
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

func (s *Scheduler) dispatch(ctx context.Context, cursor *dispatchCursor) error {
	if s.executor.halted.Load() {
		return ErrHalted
	}
	now := s.executor.clock.Now()
	lookup, cancel := context.WithTimeout(ctx, journalTimeout)
	candidates, reap, err := s.journal.dispatchCandidates(lookup, *cursor, now)
	cancel()
	if err != nil {
		return s.executor.checkStore(err)
	}
	if reap {
		reapCtx, release := context.WithTimeout(ctx, journalTimeout)
		defer release()
		err := s.journal.ReapReserved(reapCtx, now)
		return s.executor.checkStore(err)
	}
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		*cursor = candidate.cursor
		id := candidate.cursor.id
		if candidate.expired {
			expire, release := context.WithTimeout(ctx, journalTimeout)
			defer release()
			err := s.journal.expireAuthorization(expire, id, now)
			if errors.Is(err, ErrConflict) {
				return nil
			}
			return s.executor.checkStore(err)
		}
		if !candidate.available {
			continue
		}
		dispatchCtx, release := context.WithTimeout(ctx, dispatchSlice)
		defer release()
		_, err := s.executor.Dispatch(dispatchCtx, id)
		if errors.Is(err, ErrHeld) || errors.Is(err, ErrConflict) {
			return nil
		}
		return err
	}
	if len(candidates) < schedulerPageSize {
		*cursor = dispatchCursor{}
	}
	return nil
}

func (s *Scheduler) readback(ctx context.Context) error {
	ctx, stop := context.WithTimeout(ctx, readbackSlice)
	defer stop()
	lookup, cancel := context.WithTimeout(ctx, journalTimeout)
	id, err := s.journal.scheduledAttempt(lookup, s.executor.clock.Now())
	cancel()
	if err != nil {
		return s.executor.checkStore(err)
	}
	if id == "" || ctx.Err() != nil {
		return ctx.Err()
	}
	_, err = s.executor.reconcile(ctx, id, s.journal.beginScheduledReconcile)
	if errors.Is(err, ErrHeld) || errors.Is(err, ErrConflict) {
		return nil
	}
	return err
}
