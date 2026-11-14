// Package prowloutbox persists privacy-preserving Prowl deliveries independently of recovery.
package prowloutbox

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/prowl"
	"github.com/rnodern/rescuarr/internal/store"
)

const (
	maxRoutes     = 8
	maxPending    = 256
	maxAttempts   = 4
	minSendGap    = 5 * time.Second
	claimLease    = 30 * time.Second
	globalRateCap = 120
)

var (
	ErrInvalid  = errors.New("invalid local Prowl configuration")
	ErrDisabled = errors.New("Prowl delivery has not been enabled")
	ErrLimit    = errors.New("local Prowl limit reached")
)

type Category uint8

const (
	Attention Category = 1 << iota
	Recovery
	Incident
	Resolution
	Pause
)

const defaultMask = Attention | Incident | Resolution

const allCategories = Attention | Recovery | Incident | Resolution | Pause

type Route struct {
	ID         domain.RouteID
	Revision   domain.Revision
	Enabled    bool
	Paused     bool
	Categories Category
	Timezone   string
	QuietStart int // local minutes after midnight. Equal to QuietEnd for no quiet hours.
	QuietEnd   int
	Digest     bool
}

// ConfigureRoute accepts only an owner-validated secret filename. Never return
// the reference or the secret content from a browser-facing projection.
type ConfigureRoute struct {
	Route  Route
	KeyRef string
	Actor  domain.ActorID
}

type Service struct {
	db      *store.Store
	clock   app.Clock
	client  *prowl.Client
	enabled bool
}

// New has a launch switch independent of route settings. Configuring a route or
// queuing a test does not grant the process permission to contact the provider.
func New(db *store.Store, clock app.Clock, client *prowl.Client, permitDelivery bool) *Service {
	if clock == nil {
		clock = app.SystemClock{}
	}
	return &Service{db: db, clock: clock, client: client, enabled: permitDelivery}
}

func (s *Service) Configure(ctx context.Context, input ConfigureRoute) (Route, error) {
	route := input.Route
	if input.Actor == "" || !routeValid(route) || input.KeyRef == "" || len(input.KeyRef) > 128 {
		return Route{}, ErrInvalid
	}
	key, err := s.db.ReadSecret(input.KeyRef)
	if err != nil || !prowl.ValidAPIKey(key) {
		return Route{}, ErrInvalid
	}
	if route.Categories == 0 {
		route.Categories = defaultMask
	}
	if route.Timezone == "" {
		route.Timezone = "UTC"
	}
	now := s.clock.Now()
	err = s.db.Write(ctx, func(tx *sql.Tx) error {
		if route.ID == "" {
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM prowl_routes`).Scan(&count); err != nil {
				return err
			}
			if count >= maxRoutes {
				return ErrLimit
			}
			route.ID, route.Revision = domain.RouteID(store.NewID()), 1
			if _, err := tx.ExecContext(ctx, `INSERT INTO prowl_routes (id, revision, key_ref, enabled, mask, timezone, quiet_start, quiet_end, digest)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, route.ID, route.Revision, input.KeyRef, route.Enabled,
				route.Categories, route.Timezone, route.QuietStart, route.QuietEnd, route.Digest); err != nil {
				return err
			}
		} else {
			result, err := tx.ExecContext(ctx, `UPDATE prowl_routes SET revision = revision + 1, key_ref = ?, enabled = ?, paused = 0,
				mask = ?, timezone = ?, quiet_start = ?, quiet_end = ?, digest = ? WHERE id = ? AND revision = ?`, input.KeyRef,
				route.Enabled, route.Categories, route.Timezone, route.QuietStart, route.QuietEnd, route.Digest, route.ID, route.Revision)
			if err != nil {
				return err
			}
			if err := oneChanged(result); err != nil {
				return err
			}
			route.Revision++
			if _, err := tx.ExecContext(ctx, `UPDATE notification_outbox SET state = 'suppressed', reason = 'notification_route_changed'
				WHERE route_id = ? AND route_revision < ? AND state IN ('pending','delayed')`, route.ID, route.Revision); err != nil {
				return err
			}
		}
		return store.AppendEvent(ctx, tx, domain.Event{ID: domain.EventID(store.NewID()), Kind: "notification_route_configured",
			Actor: input.Actor, Reason: "notification_route_configured", OccurredAt: now})
	})
	if err != nil {
		return Route{}, err
	}
	route.Paused = false
	return route, nil
}

func routeValid(r Route) bool {
	if len(r.ID) > 128 || (r.ID == "") != (r.Revision == 0) || r.Categories&^allCategories != 0 ||
		r.QuietStart < 0 || r.QuietStart >= 1440 || r.QuietEnd < 0 || r.QuietEnd >= 1440 ||
		len(r.Timezone) > 80 || strings.EqualFold(r.Timezone, "Local") {
		return false
	}
	_, err := time.LoadLocation(r.Timezone)
	return err == nil
}

func (s *Service) Routes(ctx context.Context) ([]Route, error) {
	var result []Route
	err := s.db.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id, revision, enabled, paused, mask, timezone, quiet_start, quiet_end, digest FROM prowl_routes ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r Route
			if err := rows.Scan(&r.ID, &r.Revision, &r.Enabled, &r.Paused, &r.Categories, &r.Timezone, &r.QuietStart, &r.QuietEnd, &r.Digest); err != nil {
				return err
			}
			result = append(result, r)
		}
		return rows.Err()
	})
	return result, err
}

func oneChanged(result sql.Result) error {
	count, err := result.RowsAffected()
	if err == nil && count != 1 {
		err = app.ErrConflict
	}
	return err
}

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }
