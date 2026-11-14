// Package observeservice persists the read-only Arr projections used by the console.
package observeservice

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/observe"
	"github.com/rnodern/rescuarr/internal/store"
)

var (
	ErrInvalid  = errors.New("invalid Observe instance configuration")
	ErrConflict = errors.New("an Observe connection already exists for this destination")
	ErrLimit    = errors.New("Observe configuration limit reached")
)

const (
	maxInstances = 32
	maxCases     = 2_000
	maxGroup     = 128
	maxParallel  = 4
)

type Service struct {
	store     *store.Store
	clock     app.Clock
	transport http.RoundTripper
	parallel  chan struct{}
	mu        sync.Mutex
	active    map[domain.InstanceID]*pending
}

type pending struct {
	done chan struct{}
	err  error
}

var _ app.ConsoleReader = (*Service)(nil)

func New(db *store.Store, clock app.Clock, transport http.RoundTripper) *Service {
	if clock == nil {
		clock = app.SystemClock{}
	}
	return &Service{store: db, clock: clock, transport: transport,
		parallel: make(chan struct{}, maxParallel), active: make(map[domain.InstanceID]*pending)}
}

type InstanceInput struct {
	Product     domain.Product
	DisplayName string
	BaseURL     string
	SecretRef   string
	Actor       domain.ActorID
}

type InstanceStatus struct {
	ID          domain.InstanceID
	Product     domain.Product
	DisplayName string
	Version     string
	Status      domain.ReasonCode
	Coverage    domain.ObservationState
	CheckedAt   time.Time
}

// AddInstance validates a private secret reference and saves the local
// connection atomically with its audit event. It performs no network request.
func (s *Service) AddInstance(ctx context.Context, input InstanceInput) (domain.Instance, error) {
	name := strings.TrimSpace(input.DisplayName)
	rawURL := strings.TrimSpace(input.BaseURL)
	if input.Actor == "" || name == "" || !utf8.ValidString(name) || len(name) > 80 ||
		strings.IndexFunc(name, unicode.IsControl) != -1 || len(rawURL) > 1024 || len(input.SecretRef) > 128 {
		return domain.Instance{}, ErrInvalid
	}
	base, err := url.Parse(rawURL)
	if err != nil {
		return domain.Instance{}, ErrInvalid
	}
	base.Scheme, base.Host = strings.ToLower(base.Scheme), strings.ToLower(base.Host)
	if (base.Scheme == "http" && base.Port() == "80") || (base.Scheme == "https" && base.Port() == "443") {
		base.Host = base.Hostname()
		if net.ParseIP(base.Host) != nil && strings.Contains(base.Host, ":") {
			base.Host = "[" + base.Host + "]"
		}
	}
	base.Path = strings.TrimRight(base.Path, "/")
	// Reject a fragment delimiter even if URL serialization would drop it.
	if strings.Contains(rawURL, "#") {
		return domain.Instance{}, ErrInvalid
	}
	key, err := s.store.ReadSecret(input.SecretRef)
	if err != nil {
		return domain.Instance{}, ErrInvalid
	}
	instance := domain.Instance{Ref: domain.InstanceRef{ID: domain.InstanceID(store.NewID()), Epoch: 1},
		Product: input.Product, DisplayName: name, BaseURL: base.String(), SecretRef: input.SecretRef, ConnectionRevision: 1}
	if _, err := observe.NewArr(observe.Config{Instance: instance, APIKey: key}, s.transport); err != nil {
		return domain.Instance{}, ErrInvalid
	}
	now := s.clock.Now()
	err = s.store.Write(ctx, func(tx *sql.Tx) error {
		var count, found int
		if err := tx.QueryRowContext(ctx, `SELECT count(*), count(*) FILTER (WHERE product = ? AND base_url = ?) FROM observe_instances`,
			instance.Product, instance.BaseURL).Scan(&count, &found); err != nil {
			return err
		}
		if found > 0 {
			return ErrConflict
		}
		if count >= maxInstances {
			return ErrLimit
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO observe_instances (id, epoch, product, display_name, base_url, secret_ref, connection_revision)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, instance.Ref.ID, instance.Ref.Epoch, instance.Product, instance.DisplayName,
			instance.BaseURL, instance.SecretRef, instance.ConnectionRevision); err != nil {
			return err
		}
		if err := store.AppendEvent(ctx, tx, domain.Event{ID: domain.EventID(store.NewID()), Kind: "observe_instance_added",
			Actor: input.Actor, Scope: domain.Scope{Kind: domain.ScopeInstance, Instance: instance.Ref}, OccurredAt: now}); err != nil {
			return err
		}
		return bump(ctx, tx)
	})
	if err != nil {
		return domain.Instance{}, err
	}
	return instance, nil
}

func (s *Service) Instances(ctx context.Context) ([]InstanceStatus, error) {
	var instances []InstanceStatus
	err := s.store.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id, product, display_name, version, health, coverage, checked_at
			FROM observe_instances ORDER BY display_name, id LIMIT ?`, maxInstances)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var instance InstanceStatus
			var checked string
			if err := rows.Scan(&instance.ID, &instance.Product, &instance.DisplayName, &instance.Version, &instance.Status, &instance.Coverage, &checked); err != nil {
				return err
			}
			if checked != "" {
				instance.CheckedAt, err = time.Parse(time.RFC3339Nano, checked)
				if err != nil {
					return err
				}
			}
			if instance.Coverage == domain.ObservationKnown && instance.CheckedAt.Before(s.clock.Now().Add(-2*time.Minute)) {
				instance.Coverage = domain.ObservationStale
			}
			instances = append(instances, instance)
		}
		return rows.Err()
	})
	return instances, err
}

func bump(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `UPDATE observe_revision SET revision = revision + 1 WHERE singleton = 1`)
	return err
}
