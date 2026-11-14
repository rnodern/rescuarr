// Package observe collects bounded read-only evidence from configured upstreams.
package observe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
)

const (
	readTimeout = 10 * time.Second
	maxBody     = 2 << 20
)

var (
	ErrConfiguration = errors.New("invalid observation configuration")
	ErrAccess        = errors.New("upstream denied observation")
	ErrUnavailable   = errors.New("upstream observation unavailable")
	ErrNotFound      = errors.New("upstream observation resource missing")
	ErrRateLimited   = errors.New("upstream observation rate limited")
	ErrResponse      = errors.New("upstream observation response invalid")
	ErrPartial       = errors.New("upstream observation is partial")
)

// Config is supplied by the owner-authenticated configuration boundary.
// The API key is resolved server-side and never returned in an observation or error.
type Config struct {
	Instance domain.Instance
	APIKey   string
}

type reader struct {
	instance domain.InstanceRef
	product  domain.Product
	base     url.URL
	key      string
	http     *http.Client
	slots    chan struct{}
}

func newReader(cfg Config, transport http.RoundTripper) (*reader, error) {
	base, err := url.Parse(cfg.Instance.BaseURL)
	if err != nil || !cfg.Instance.Ref.Valid() || strings.TrimSpace(cfg.APIKey) != cfg.APIKey || strings.ContainsAny(cfg.APIKey, "\r\n\x00") || len(cfg.APIKey) > 1024 {
		return nil, ErrConfiguration
	}
	if (base.Scheme != "http" && base.Scheme != "https") || base.Hostname() == "" || base.User != nil || base.Opaque != "" ||
		base.RawQuery != "" || base.ForceQuery || strings.Contains(cfg.Instance.BaseURL, "#") || base.RawPath != "" || strings.ContainsAny(base.Path, "\\\x00") || len(base.Path) > 512 {
		return nil, ErrConfiguration
	}
	base.Path = strings.TrimRight(base.Path, "/")
	for _, part := range strings.Split(strings.TrimPrefix(base.Path, "/"), "/") {
		if part == "." || part == ".." || (part == "" && base.Path != "") {
			return nil, ErrConfiguration
		}
	}
	if transport == nil {
		transport = &http.Transport{
			DialContext:       (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2: true, TLSHandshakeTimeout: 3 * time.Second,
			MaxConnsPerHost: 2, MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second,
			MaxResponseHeaderBytes: 32 << 10,
		}
	}
	return &reader{
		instance: cfg.Instance.Ref, product: cfg.Instance.Product, base: *base, key: cfg.APIKey,
		http: &http.Client{Transport: transport, Timeout: readTimeout, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		slots: make(chan struct{}, 2),
	}, nil
}

func (r *reader) get(ctx context.Context, path string, query url.Values, sendKey bool, dest any) error {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	target := r.base
	target.Path += path
	if sendKey && r.product == domain.ProductSAB {
		query.Set("apikey", r.key)
	}
	target.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return ErrConfiguration
	}
	req.Header.Set("Accept", "application/json")
	if sendKey && r.product != domain.ProductSAB {
		req.Header.Set("X-Api-Key", r.key)
	}
	res, err := r.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrUnavailable
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrAccess
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusTooManyRequests:
		return ErrRateLimited
	default:
		return ErrUnavailable
	}
	kind, _, err := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(kind, "application/json") {
		return ErrResponse
	}
	if res.ContentLength > maxBody {
		return ErrResponse
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxBody+1))
	if err != nil || len(body) > maxBody || bytes.Equal(bytes.TrimSpace(body), []byte("null")) || json.Unmarshal(body, dest) != nil {
		return ErrResponse
	}
	return nil
}
