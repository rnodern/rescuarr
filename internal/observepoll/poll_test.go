package observepoll

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/observeservice"
)

type fixture struct {
	instances func(context.Context) ([]observeservice.InstanceStatus, error)
	collect   func(context.Context, domain.InstanceID) error
}

func (f fixture) Instances(ctx context.Context) ([]observeservice.InstanceStatus, error) {
	return f.instances(ctx)
}

func (f fixture) Collect(ctx context.Context, id domain.InstanceID) error {
	return f.collect(ctx, id)
}

func TestRunFillsAvailableSlotsAndGivesWaitingConnectionsPriority(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		release := make(chan struct{})
		started := make(chan domain.InstanceID, 12)
		finished := make(chan domain.InstanceID, 12)
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			Run(ctx, fixture{
				instances: func(context.Context) ([]observeservice.InstanceStatus, error) {
					return []observeservice.InstanceStatus{
						{ID: "slow-a"}, {ID: "slow-a"}, {ID: "unreachable"}, {ID: "slow-b"}, {ID: "healthy"}, {ID: "waiting"},
					}, nil
				},
				collect: func(ctx context.Context, id domain.InstanceID) error {
					started <- id
					defer func() { finished <- id }()
					switch id {
					case "unreachable":
						select {
						case <-release:
							return errors.New("fixture connection unavailable")
						case <-ctx.Done():
						}
					case "healthy":
						return nil
					default:
						<-ctx.Done()
					}
					return ctx.Err()
				},
			})
		}()
		synctest.Wait()
		checkIDs(t, started, "slow-a", "unreachable", "slow-b")
		checkIDs(t, finished)

		time.Sleep(discoveryInterval)
		synctest.Wait()
		checkIDs(t, started)

		// One failing read lets both later connections run without waiting for slow-a or slow-b.
		close(release)
		synctest.Wait()
		checkIDs(t, started, "healthy", "waiting")
		checkIDs(t, finished, "unreachable", "healthy")

		time.Sleep(pollTimeout - discoveryInterval)
		synctest.Wait()
		checkIDs(t, started)
		checkIDs(t, finished, "slow-a", "slow-b")

		cancel()
		<-stopped
		checkIDs(t, finished, "waiting")
	})
}

func TestRunRefreshesConfigurationAndHonoursCadence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		now := time.Now()
		instances := []observeservice.InstanceStatus{
			{ID: "manual", CheckedAt: now},
			{ID: "changed", Status: "contract_changed"},
		}
		started := make(chan domain.InstanceID, 12)
		stopped := make(chan struct{})
		updates := make(chan []observeservice.InstanceStatus, 1)
		go func() {
			defer close(stopped)
			var current []observeservice.InstanceStatus
			Run(ctx, fixture{
				instances: func(context.Context) ([]observeservice.InstanceStatus, error) {
					select {
					case current = <-updates:
					default:
					}
					if current == nil {
						return nil, errors.New("fixture store unavailable")
					}
					return current, nil
				},
				collect: func(_ context.Context, id domain.InstanceID) error {
					started <- id
					return nil
				},
			})
		}()
		synctest.Wait()
		checkIDs(t, started)

		updates <- append(instances, observeservice.InstanceStatus{ID: "new"})
		time.Sleep(discoveryInterval)
		synctest.Wait()
		checkIDs(t, started, "new")

		time.Sleep(pollInterval - discoveryInterval)
		synctest.Wait()
		checkIDs(t, started, "manual")

		time.Sleep(discoveryInterval)
		synctest.Wait()
		checkIDs(t, started, "new")

		updates <- instances
		time.Sleep(pollInterval)
		synctest.Wait()
		checkIDs(t, started, "manual")
		cancel()
		<-stopped
	})
}

func TestRunCancelsDiscovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		started := make(chan struct{})
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			Run(ctx, fixture{
				instances: func(ctx context.Context) ([]observeservice.InstanceStatus, error) {
					close(started)
					<-ctx.Done()
					return nil, ctx.Err()
				},
				collect: func(context.Context, domain.InstanceID) error {
					t.Error("collection attempted without discovering configured connections")
					return nil
				},
			})
		}()
		<-started
		cancel()
		<-stopped
	})
}

func checkIDs(t *testing.T, calls <-chan domain.InstanceID, want ...domain.InstanceID) {
	t.Helper()
	if len(calls) != len(want) {
		t.Fatalf("got %d calls, want %v", len(calls), want)
	}
	expected := make(map[domain.InstanceID]bool, len(want))
	for _, id := range want {
		expected[id] = true
	}
	for range want {
		id := <-calls
		if !expected[id] {
			t.Fatalf("unexpected call for %q, want %v", id, want)
		}
		delete(expected, id)
	}
}
