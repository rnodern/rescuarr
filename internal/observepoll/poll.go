// Package observepoll schedules read-only collections for configured Observe connections.
package observepoll

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/observeservice"
)

const (
	discoveryInterval = 15 * time.Second
	discoveryTimeout  = 5 * time.Second
	pollInterval      = 45 * time.Second
	// Allow the service's 40-second upstream deadline to leave time to persist health.
	pollTimeout = 45 * time.Second
	// Leave one of the Observe service's four slots available for manual reads.
	parallel = 3
)

// Collector is the read-only collection surface of the Observe service.
type Collector interface {
	Instances(context.Context) ([]observeservice.InstanceStatus, error)
	Collect(context.Context, domain.InstanceID) error
}

type attempt struct {
	active     bool
	finishedAt time.Time
}

type result struct {
	id         domain.InstanceID
	finishedAt time.Time
}

// Run discovers configured connections immediately and at regular intervals.
// It returns after cancellation and waits for in-flight collections to exit.
func Run(ctx context.Context, service Collector) {
	ticker := time.NewTicker(discoveryInterval)
	defer ticker.Stop()

	var workers sync.WaitGroup
	defer workers.Wait()
	done := make(chan result, parallel)

	attempts := make(map[domain.InstanceID]attempt)
	var instances []observeservice.InstanceStatus
	active := 0
	refresh := true
	for ctx.Err() == nil {
		if refresh {
			instances = discover(ctx, service, attempts)
			refresh = false
		}
		if active < parallel {
			for _, id := range due(instances, attempts, time.Now()) {
				if active == parallel || ctx.Err() != nil {
					break
				}
				if attempts[id].active {
					continue
				}
				attempts[id] = attempt{active: true}
				active++
				workers.Go(func() {
					cycle, cancel := context.WithTimeout(ctx, pollTimeout)
					defer cancel()
					if cycle.Err() == nil {
						// The service persists health and coverage for failed upstream reads.
						_ = service.Collect(cycle, id)
					}
					done <- result{id: id, finishedAt: time.Now()}
				})
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh = true
		case completed := <-done:
			attempts[completed.id] = attempt{finishedAt: completed.finishedAt}
			active--
		}
	}
}

func discover(ctx context.Context, service Collector, attempts map[domain.InstanceID]attempt) []observeservice.InstanceStatus {
	lookup, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	instances, err := service.Instances(lookup)
	if err != nil {
		return nil
	}
	configured := make(map[domain.InstanceID]bool, len(instances))
	for _, instance := range instances {
		configured[instance.ID] = true
	}
	for id, state := range attempts {
		if !configured[id] && !state.active {
			delete(attempts, id)
		}
	}
	return instances
}

func due(instances []observeservice.InstanceStatus, attempts map[domain.InstanceID]attempt, now time.Time) []domain.InstanceID {
	type candidate struct {
		id            domain.InstanceID
		lastAttemptAt time.Time
	}
	var ready []candidate
	cutoff := now.Add(-pollInterval)
	for _, instance := range instances {
		state := attempts[instance.ID]
		// An upstream version change requires an explicit recheck.
		if state.active || instance.Status == "contract_changed" {
			continue
		}
		lastAttemptAt := state.finishedAt
		if lastAttemptAt.Before(instance.CheckedAt) {
			lastAttemptAt = instance.CheckedAt
		}
		if !lastAttemptAt.After(cutoff) {
			ready = append(ready, candidate{id: instance.ID, lastAttemptAt: lastAttemptAt})
		}
	}
	// Prefer waiting connections over connections that already had a turn.
	sort.SliceStable(ready, func(i, j int) bool {
		return ready[i].lastAttemptAt.Before(ready[j].lastAttemptAt)
	})
	ids := make([]domain.InstanceID, len(ready))
	for index, entry := range ready {
		ids[index] = entry.id
	}
	return ids
}
