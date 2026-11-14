// Package arradapter exercises the fenced Arr write boundary with an injected fixture transport.
package arradapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rnodern/rescuarr/internal/arrwire"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/ledger"
)

const operationTimeout = 10 * time.Second

var (
	ErrConfiguration = errors.New("invalid Arr fixture adapter configuration")
	ErrPrepared      = errors.New("Arr dispatch requires a prepared action")
	ErrQualification = errors.New("Arr route qualification unavailable")
	ErrUncertain     = errors.New("Arr dispatch outcome uncertain")
	ErrReadback      = errors.New("scoped Arr readback unavailable")
)

type Route string

const (
	RouteCopyImport     Route = "copy_import"
	RouteRejectPreserve Route = "reject_preserve"
	RouteRejectRemove   Route = "reject_remove"
	RouteEpisodeSearch  Route = "episode_search"
	RouteMovieSearch    Route = "movie_search"
)

// Binding identifies one route and immutable configuration snapshot. The
// fingerprint covers relevant settings, including downloader, cleanup and path
// configuration, according to the caller's qualification contract.
type Binding struct {
	Instance            domain.InstanceRef
	Product             domain.Product
	Version             string
	ConnectionRevision  domain.Revision
	AdapterRevision     string
	SettingsFingerprint [sha256.Size]byte
	Route               Route
}

type Qualification struct {
	Binding    Binding
	Verified   bool
	Evidence   domain.EvidenceID
	ObservedAt time.Time
	ExpiresAt  time.Time
}

// Qualify looks up a current, positive fixture qualification for the exact
// binding. Results are values so the adapter can compare the entire snapshot.
type Qualify func(context.Context, Binding) (Qualification, error)

// FixtureTransport is one injected exchange. It must honour the request context.
// The adapter never falls back to an HTTP client, follows redirects or retries.
type FixtureTransport func(*http.Request) (*http.Response, error)

// Readback probes the job, targets and effects of the specified attempt using
// independent evidence. The ledger validates and aggregates its observations.
type Readback func(context.Context, Route, domain.Attempt) (domain.Outcome, error)

type Config struct {
	Instance            domain.Instance
	AdapterRevision     string
	SettingsFingerprint [sha256.Size]byte
	APIKey              string
}

type Adapter struct {
	instance domain.Instance
	binding  Binding
	base     url.URL
	key      string
	exchange FixtureTransport
	qualify  Qualify
	readback Readback
	now      func() time.Time
	route    Route
}

var _ ledger.PreparedAdapter = (*Adapter)(nil)

// NewFixture builds an adapter for synthetic exchanges and readback. There is
// no production transport or source of live route qualifications.
func NewFixture(cfg Config, exchange FixtureTransport, qualify Qualify, readback Readback, now func() time.Time) (*Adapter, error) {
	return newAdapter(cfg, exchange, qualify, readback, now, "")
}

func newAdapter(cfg Config, exchange FixtureTransport, qualify Qualify, readback Readback, now func() time.Time, route Route) (*Adapter, error) {
	instance := cfg.Instance
	base, ok := parseBase(instance.BaseURL)
	if !ok || !instance.Ref.Valid() || instance.ConnectionRevision == 0 ||
		cfg.AdapterRevision == "" || cfg.SettingsFingerprint == ([sha256.Size]byte{}) ||
		!validAPIKey(cfg.APIKey) || exchange == nil || qualify == nil || readback == nil || now == nil {
		return nil, ErrConfiguration
	}
	switch instance.Product {
	case domain.ProductSonarr:
		if instance.UpstreamVersion != arrwire.SonarrVersion &&
			(instance.UpstreamVersion != arrwire.SonarrIDOnlyImportVersion || route != RouteCopyImport) {
			return nil, ErrConfiguration
		}
	case domain.ProductRadarr:
		if instance.UpstreamVersion != arrwire.RadarrVersion {
			return nil, ErrConfiguration
		}
	default:
		return nil, ErrConfiguration
	}
	return &Adapter{
		instance: instance,
		binding: Binding{
			Instance: instance.Ref, Product: instance.Product, Version: instance.UpstreamVersion,
			ConnectionRevision: instance.ConnectionRevision, AdapterRevision: cfg.AdapterRevision,
			SettingsFingerprint: cfg.SettingsFingerprint,
		},
		base: base, key: cfg.APIKey, exchange: exchange, qualify: qualify, readback: readback, now: now, route: route,
	}, nil
}

func (*Adapter) Dispatch(context.Context, domain.Operation) (domain.Acknowledgement, error) {
	return domain.Acknowledgement{}, ErrPrepared
}

func (a *Adapter) DispatchPrepared(ctx context.Context, op domain.Operation, prepared domain.PreparedAction) (domain.Acknowledgement, error) {
	return a.dispatchPrepared(ctx, op, prepared, nil)
}

func (a *Adapter) dispatchPrepared(ctx context.Context, op domain.Operation, prepared domain.PreparedAction, beforeSend func(context.Context) error) (domain.Acknowledgement, error) {
	wire, err := arrwire.Encode(a.instance, a.binding.AdapterRevision, op, prepared)
	if err != nil {
		return domain.Acknowledgement{}, err
	}
	binding := a.binding
	binding.Route = actionRoute(op.Action, binding.Product, prepared)
	if a.route != "" && binding.Route != a.route {
		return domain.Acknowledgement{}, ErrQualification
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if ctx.Err() != nil {
		return domain.Acknowledgement{}, ErrQualification
	}
	qualification, err := a.qualify(ctx, binding)
	if err != nil || !validQualification(binding, qualification, a.now()) {
		return domain.Acknowledgement{}, ErrQualification
	}

	target := a.base
	target.Path += wire.Path()
	target.RawQuery = wire.RawQuery()
	req, err := http.NewRequestWithContext(ctx, wire.Method(), target.String(), bytes.NewReader(wire.Body()))
	if err != nil {
		return domain.Acknowledgement{}, ErrConfiguration
	}
	req.GetBody = nil
	req.Close = true
	req.Header.Set("X-Api-Key", a.key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	if wire.ContentType() != "" {
		req.Header.Set("Content-Type", wire.ContentType())
	}
	if ctx.Err() != nil || !validQualification(binding, qualification, a.now()) {
		return domain.Acknowledgement{}, ErrQualification
	}
	if beforeSend != nil {
		if err := beforeSend(ctx); err != nil || ctx.Err() != nil || !validQualification(binding, qualification, a.now()) {
			return domain.Acknowledgement{}, ErrQualification
		}
	}

	response, err := a.exchange(req)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err == nil {
		if ack, ok := acknowledge(binding.Route, response); ok && ctx.Err() == nil {
			return ack, nil
		}
	}
	return domain.Acknowledgement{State: domain.DispatchUncertain, Reason: "dispatch_uncertain"}, ErrUncertain
}

func (a *Adapter) Reconcile(ctx context.Context, attempt domain.Attempt) (domain.Outcome, error) {
	if attempt.ID == "" || attempt.SentAt.IsZero() || attempt.Prepared == nil {
		return domain.Outcome{}, ErrReadback
	}
	if _, err := arrwire.Encode(a.instance, a.binding.AdapterRevision, attempt.Operation, *attempt.Prepared); err != nil {
		return domain.Outcome{}, ErrReadback
	}
	route := actionRoute(attempt.Operation.Action, a.binding.Product, *attempt.Prepared)
	if a.route != "" && route != a.route {
		return domain.Outcome{}, ErrReadback
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if ctx.Err() != nil {
		return domain.Outcome{}, ErrReadback
	}
	observed, err := a.readback(ctx, route, copyAttempt(attempt))
	if err != nil || ctx.Err() != nil {
		return domain.Outcome{}, ErrReadback
	}
	return observed, nil
}

func validQualification(binding Binding, record Qualification, now time.Time) bool {
	return record.Verified && record.Binding == binding && record.Evidence != "" &&
		!record.ObservedAt.IsZero() && !record.ObservedAt.After(now) && record.ExpiresAt.After(now)
}

func actionRoute(action domain.Action, product domain.Product, prepared domain.PreparedAction) Route {
	switch action {
	case domain.ActionImport:
		return RouteCopyImport
	case domain.ActionReject:
		if prepared.Reject.RemoveFromClient {
			return RouteRejectRemove
		}
		return RouteRejectPreserve
	default: // The encoder has already required an exact supported action.
		if product == domain.ProductSonarr {
			return RouteEpisodeSearch
		}
		return RouteMovieSearch
	}
}

func parseBase(raw string) (url.URL, bool) {
	base, err := url.Parse(raw)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") ||
		base.Hostname() == "" || base.User != nil || base.Opaque != "" || base.RawPath != "" ||
		base.RawQuery != "" || base.ForceQuery || strings.Contains(raw, "#") || len(base.Path) > 512 {
		return url.URL{}, false
	}
	base.Path = strings.TrimSuffix(base.Path, "/")
	if base.Path != "" {
		for _, segment := range strings.Split(strings.TrimPrefix(base.Path, "/"), "/") {
			if segment == "" || segment == "." || segment == ".." {
				return url.URL{}, false
			}
			for _, r := range segment {
				if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~", r) {
					return url.URL{}, false
				}
			}
		}
	}
	return *base, true
}

func validAPIKey(key string) bool {
	if len(key) == 0 || len(key) > 1024 {
		return false
	}
	for _, r := range key {
		if r <= ' ' || r > '~' {
			return false
		}
	}
	return true
}
