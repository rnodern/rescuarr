// Package importflow reads and evaluates job-scoped native Arr import previews.
package importflow

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rnodern/rescuarr/internal/arrwire"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/observe"
	"github.com/rnodern/rescuarr/internal/policy"
)

const (
	// Baseline builds for the contracts that support the import preview.
	SonarrVersion = arrwire.SonarrVersion
	RadarrVersion = arrwire.RadarrVersion
	adapter       = "manual-import-preview-1"
	maxBody       = 2 << 20
	maxCandidates = 128
	maxReadAge    = time.Minute
	readTimeout   = 10 * time.Second
)

var (
	ErrInvalid     = errors.New("invalid import preview configuration or identity")
	ErrUnsupported = errors.New("import preview requires a supported Arr version")
	ErrStale       = errors.New("import preview requires current exact job evidence")
)

type Client struct {
	instance domain.Instance
	key      string
	base     url.URL
	http     *http.Client
	slots    chan struct{}
}

// New reuses Observe's input validation and accepts a caller-supplied transport.
func New(cfg observe.Config, transport http.RoundTripper) (*Client, error) {
	if _, err := observe.NewArr(cfg, transport); err != nil {
		return nil, ErrInvalid
	}
	base, err := url.Parse(cfg.Instance.BaseURL)
	if err != nil {
		return nil, ErrInvalid
	}
	base.Path = strings.TrimRight(base.Path, "/")
	if transport == nil {
		transport = &http.Transport{
			DialContext:       (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2: true, TLSHandshakeTimeout: 3 * time.Second,
			MaxConnsPerHost: 2, MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second,
			MaxResponseHeaderBytes: 32 << 10,
		}
	}
	return &Client{instance: cfg.Instance, key: cfg.APIKey, base: *base,
		http: &http.Client{Transport: transport, Timeout: readTimeout, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}, slots: make(chan struct{}, 2)}, nil
}

type PathStyle string

const (
	UnixPath    PathStyle = "unix"
	WindowsPath PathStyle = "windows"
)

type Target struct {
	Ref             domain.TargetRef
	ProfileID       int64
	ProfileRevision domain.Revision
	FileID          int64
	Monitored       bool
	Intent          policy.Intent
	Evidence        domain.Observation
}

// Job contains exactly the Arr-visible job identity and scope collected from a
// current coherent queue observation. OutputPath is private server-side input.
type Job struct {
	Ref              domain.JobRef
	DownloadID       string   `json:"-"`
	OutputPath       string   `json:"-"`
	LibraryRoots     []string `json:"-"`
	PathStyle        PathStyle
	Targets          []Target
	Queue            observe.QueueSnapshot `json:"-"`
	TrackingComplete bool
	SourceAccessible domain.Fact[bool] `json:"-"`
}

type Assessment string

const (
	CompleteCandidates Assessment = "complete_candidates"
	PartialCandidates  Assessment = "partial_candidates"
	NoUsableCandidate  Assessment = "no_usable_candidate"
	Ambiguous          Assessment = "ambiguous_candidates"
	Uncertain          Assessment = "preview_uncertain"
)

type Preview struct {
	Job          domain.JobRef
	Assessment   Assessment
	Reason       domain.ReasonCode
	Candidates   []Candidate
	Evidence     domain.Observation
	NativeReview bool
}

// Discover fetches only GET /manualimport using the current opaque download ID.
// A preview never authorises reprocessing, import or source cleanup.
func (c *Client) Discover(ctx context.Context, job Job, now time.Time) (Preview, error) {
	now = now.UTC()
	started := time.Now()
	result := Preview{Job: job.Ref, Assessment: Uncertain, Reason: "import_preview_unavailable", NativeReview: true}
	if !versionSupported(c.instance) {
		return result, ErrUnsupported
	}
	if err := validateJob(c.instance, job, now); err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-ctx.Done():
		return result, ctx.Err()
	}
	rows, err := c.get(ctx, job.DownloadID)
	if err != nil {
		return result, err
	}
	receivedAt := now.Add(time.Since(started))
	if err := validateJob(c.instance, job, receivedAt); err != nil {
		return result, err
	}
	result = evaluate(c.instance, job, rows, receivedAt)
	return result, nil
}

// PreviewContract reports whether the import preview covers an Arr build. The
// preview uses the Sonarr v4 and Radarr contracts. Sonarr v5 uses Manual Review.
func PreviewContract(product domain.Product, version string) bool {
	switch arrwire.ContractFor(product, version) {
	case arrwire.ContractSonarrV4:
		return product == domain.ProductSonarr
	case arrwire.ContractRadarr:
		return product == domain.ProductRadarr
	default:
		return false
	}
}

func versionSupported(i domain.Instance) bool {
	return PreviewContract(i.Product, i.UpstreamVersion)
}

func validateJob(i domain.Instance, job Job, now time.Time) error {
	if !job.Ref.Instance.Valid() || job.Ref.Instance != i.Ref || job.Ref.ID == "" || job.Ref.Generation == 0 ||
		!cleanText(job.DownloadID, 256) || len(job.Targets) == 0 || len(job.Targets) > maxCandidates || len(job.LibraryRoots) == 0 ||
		len(job.LibraryRoots) > maxCandidates || (i.Product == domain.ProductRadarr && len(job.Targets) != 1) {
		return ErrInvalid
	}
	if output, ok := parsePath(job.PathStyle, job.OutputPath); !ok || len(output.parts) == 0 {
		return ErrInvalid
	}
	for _, root := range job.LibraryRoots {
		if _, ok := parsePath(job.PathStyle, root); !ok || pathWithin(job.PathStyle, root, job.OutputPath, true) || pathWithin(job.PathStyle, job.OutputPath, root, true) {
			return ErrInvalid
		}
	}
	queue := job.Queue.Evidence
	if !job.TrackingComplete || queue.Kind != "queue" || !fromOwner(i, queue, now) || queue.SnapshotGeneration == 0 {
		return ErrStale
	}
	seen := make(map[domain.TargetRef]bool, len(job.Targets))
	for _, target := range job.Targets {
		if !target.Ref.Valid() || target.Ref.Instance != i.Ref || seen[target.Ref] || target.ProfileID <= 0 || target.ProfileRevision == 0 || target.FileID < 0 ||
			(i.Product == domain.ProductSonarr && target.Ref.Kind != domain.TargetEpisode) ||
			(i.Product == domain.ProductRadarr && target.Ref.Kind != domain.TargetMovie) {
			return ErrInvalid
		}
		if target.Evidence.Kind != TargetProfileEvidence || target.Evidence.SourceRecord != strconv.FormatInt(target.Ref.ID, 10) ||
			!fromOwner(i, target.Evidence, now) || !validIntent(target) {
			return ErrStale
		}
		seen[target.Ref] = true
	}
	return validateQueueScope(job, seen)
}

func validIntent(target Target) bool {
	switch target.Intent {
	case policy.NeedFirstImport:
		return target.Monitored && target.FileID == 0
	case policy.NeedUpgrade, policy.NeedSatisfied:
		return target.Monitored && target.FileID > 0
	case policy.NeedWithdrawn:
		return !target.Monitored
	default:
		return false
	}
}

func validateQueueScope(job Job, expected map[domain.TargetRef]bool) error {
	ids := make(map[int64]bool)
	matched := make(map[domain.TargetRef]bool)
	for _, record := range job.Queue.Records {
		if record.DownloadID != job.DownloadID {
			if pathWithin(job.PathStyle, job.OutputPath, record.OutputPath, true) || pathWithin(job.PathStyle, record.OutputPath, job.OutputPath, true) {
				return ErrStale
			}
			continue
		}
		if record.ID <= 0 || ids[record.ID] || !expected[record.Target] || matched[record.Target] || record.Status != "completed" ||
			record.TrackedState != "importPending" || !samePath(job.PathStyle, record.OutputPath, job.OutputPath) {
			return ErrStale
		}
		ids[record.ID] = true
		matched[record.Target] = true
	}
	if len(matched) != len(expected) {
		return ErrStale
	}
	return nil
}

func fromOwner(i domain.Instance, observation domain.Observation, now time.Time) bool {
	return observation.ID != "" && observation.Source == domain.Source(i.Product) &&
		observation.Scope == (domain.Scope{Kind: domain.ScopeInstance, Instance: i.Ref}) && observation.State == domain.ObservationKnown &&
		observation.Fresh(now) && now.Sub(observation.ObservedAt) < maxReadAge
}

func (c *Client) get(ctx context.Context, download string) ([]wireCandidate, error) {
	endpoint := c.base
	endpoint.Path += "/api/v3/manualimport"
	endpoint.RawQuery = url.Values{"downloadId": {download}, "filterExistingFiles": {"false"}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, ErrInvalid
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Api-Key", c.key)
	res, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, observe.ErrUnavailable
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, observe.ErrAccess
	case http.StatusNotFound:
		return nil, observe.ErrNotFound
	case http.StatusTooManyRequests:
		return nil, observe.ErrRateLimited
	default:
		return nil, observe.ErrUnavailable
	}
	mimeType, _, err := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mimeType, "application/json") || res.ContentLength > maxBody {
		return nil, observe.ErrResponse
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxBody+1))
	if err != nil || len(body) > maxBody || !utf8.Valid(body) {
		return nil, observe.ErrResponse
	}
	var rows []wireCandidate
	if json.Unmarshal(body, &rows) != nil || rows == nil {
		return nil, observe.ErrResponse
	}
	if len(rows) > maxCandidates {
		return nil, observe.ErrPartial
	}
	return rows, nil
}
