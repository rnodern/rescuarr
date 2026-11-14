package observe

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/rnodern/rescuarr/internal/domain"
)

var (
	sabVersion = regexp.MustCompile(`^[0-9]{1,6}(\.[0-9]{1,6}){2}$`)
	sabJobID   = regexp.MustCompile(`^SABnzbd_nzo_[A-Za-z0-9_]{1,64}$`)
)

type SAB struct{ reader *reader }

func NewSAB(cfg Config, transport http.RoundTripper) (*SAB, error) {
	if cfg.Instance.Product != domain.ProductSAB {
		return nil, ErrConfiguration
	}
	r, err := newReader(cfg, transport)
	if err != nil {
		return nil, err
	}
	return &SAB{reader: r}, nil
}

func (s *SAB) Version(ctx context.Context) (string, error) {
	var response struct {
		Version string `json:"version"`
	}
	if err := s.reader.get(ctx, "/api", url.Values{"mode": {"version"}, "output": {"json"}}, false, &response); err != nil {
		return "", err
	}
	if !sabVersion.MatchString(response.Version) {
		return "", ErrResponse
	}
	return response.Version, nil
}

type SABJobState string

const (
	SABActive    SABJobState = "active"
	SABFailed    SABJobState = "failed"
	SABCompleted SABJobState = "completed"
	SABUnknown   SABJobState = "unknown"
)

type SABJob struct {
	Instance domain.InstanceRef
	ID       string
	Category string
	State    SABJobState
	Bytes    *int64
}

type sabJobWire struct {
	ID       string `json:"nzo_id"`
	Category string `json:"category"`
	Status   string `json:"status"`
	Bytes    *int64 `json:"bytes"`
}

type sabPage struct {
	Jobs []sabJobWire `json:"slots"`
}

func (s *SAB) Queue(ctx context.Context, jobIDs ...string) ([]SABJob, error) {
	return s.jobs(ctx, "queue", jobIDs)
}

func (s *SAB) History(ctx context.Context, jobIDs ...string) ([]SABJob, error) {
	return s.jobs(ctx, "history", jobIDs)
}

// jobs returns only positively observed exact IDs. Unreturned IDs may be
// unavailable, moving between queue and history, or outside retained history.
func (s *SAB) jobs(ctx context.Context, mode string, ids []string) ([]SABJob, error) {
	if s.reader.key == "" || len(ids) < 1 || len(ids) > 10 {
		return nil, ErrConfiguration
	}
	requested := make(map[string]bool)
	for _, id := range ids {
		if !sabJobID.MatchString(id) || requested[id] {
			return nil, ErrConfiguration
		}
		requested[id] = true
	}
	var response struct {
		Status  *bool    `json:"status"`
		Queue   *sabPage `json:"queue"`
		History *sabPage `json:"history"`
	}
	query := url.Values{"mode": {mode}, "output": {"json"}, "nzo_ids": {strings.Join(ids, ",")}, "start": {"0"}, "limit": {strconv.Itoa(len(ids))}}
	if err := s.reader.get(ctx, "/api", query, true, &response); err != nil {
		return nil, err
	}
	if response.Status != nil && !*response.Status {
		return nil, ErrResponse
	}
	p := response.History
	if mode == "queue" {
		p = response.Queue
	}
	if p == nil || p.Jobs == nil || len(p.Jobs) > len(ids) {
		return nil, ErrResponse
	}
	out := make([]SABJob, 0, len(p.Jobs))
	for _, row := range p.Jobs {
		if !requested[row.ID] {
			return out, ErrPartial
		}
		delete(requested, row.ID)
		job := SABJob{Instance: s.reader.instance, ID: row.ID, Category: boundedText(row.Category, 128), State: SABUnknown, Bytes: nonnegative(row.Bytes)}
		switch strings.ToLower(row.Status) {
		case "downloading", "queued", "paused", "propagating", "fetching", "quickcheck", "verifying", "repairing", "extracting", "moving", "running", "loaded":
			job.State = SABActive
		case "failed":
			if mode == "history" {
				job.State = SABFailed
			}
		case "completed":
			if mode == "history" {
				job.State = SABCompleted
			}
		}
		out = append(out, job)
	}
	return out, nil
}
