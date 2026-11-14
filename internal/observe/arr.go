package observe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
)

const (
	AdapterRevision = "arr-v3-read-1"
	queuePageSize   = 100
	historyPageSize = 50
	maximumPages    = 10
	scanTimeout     = 25 * time.Second
)

var arrVersion = regexp.MustCompile(`^[0-9]{1,6}(\.[0-9]{1,6}){3}$`)

type Arr struct {
	reader             *reader
	upstreamVersion    string
	connectionRevision domain.Revision
}

func NewArr(cfg Config, transport http.RoundTripper) (*Arr, error) {
	if (cfg.Instance.Product != domain.ProductSonarr && cfg.Instance.Product != domain.ProductRadarr) || cfg.APIKey == "" {
		return nil, ErrConfiguration
	}
	r, err := newReader(cfg, transport)
	if err != nil {
		return nil, err
	}
	return &Arr{reader: r, upstreamVersion: cfg.Instance.UpstreamVersion, connectionRevision: cfg.Instance.ConnectionRevision}, nil
}

type SystemStatus struct {
	Product   domain.Product
	Version   string
	Branch    string
	Package   string
	OSFamily  string
	PathOS    string
	URLBase   string
	StartedAt time.Time
}

func (a *Arr) Status(ctx context.Context) (SystemStatus, error) {
	var wire struct {
		Name      string    `json:"appName"`
		Version   string    `json:"version"`
		Branch    string    `json:"branch"`
		Package   string    `json:"packageVersion"`
		OSName    string    `json:"osName"`
		IsLinux   bool      `json:"isLinux"`
		IsOSX     bool      `json:"isOsx"`
		IsWindows bool      `json:"isWindows"`
		URLBase   string    `json:"urlBase"`
		StartedAt time.Time `json:"startTime"`
	}
	if err := a.reader.get(ctx, "/api/v3/system/status", nil, true, &wire); err != nil {
		return SystemStatus{}, err
	}
	if !strings.EqualFold(wire.Name, string(a.reader.product)) || !arrVersion.MatchString(wire.Version) {
		return SystemStatus{}, ErrResponse
	}
	var pathOS string
	switch {
	case wire.IsWindows && !wire.IsLinux && !wire.IsOSX:
		pathOS = "windows"
	case !wire.IsWindows && wire.IsLinux != wire.IsOSX:
		pathOS = "unix"
	}
	return SystemStatus{
		Product: a.reader.product, Version: wire.Version, Branch: boundedText(wire.Branch, 64),
		Package: boundedText(wire.Package, 128), OSFamily: boundedText(wire.OSName, 64), PathOS: pathOS,
		URLBase: boundedText(wire.URLBase, 512), StartedAt: wire.StartedAt,
	}, nil
}

type QueueFinding string

const (
	FindingNoEligibleFiles QueueFinding = "no_eligible_files"
	FindingSeriesIDOnly    QueueFinding = "series_matched_by_id"
	FindingOtherMessage    QueueFinding = "unclassified_message"
)

type QueueRecord struct {
	// SeriesID is an in-memory Sonarr hint, even when no exact episode is mapped.
	SeriesID      int64 `json:"-"`
	ID            int64
	Target        domain.TargetRef
	DownloadID    string
	Protocol      domain.Protocol
	ClientLabel   string
	IndexerLabel  string
	Status        string
	TrackedState  string
	TrackedStatus string
	SizeBytes     *int64
	AddedAt       time.Time
	Findings      []QueueFinding
	Release       string `json:"-"`
	MediaTitle    string `json:"-"`
	EpisodeTitle  string `json:"-"`
	Episode       string `json:"-"`
	// OutputPath is Arr-visible server data. It must stay out of generic serialisation.
	OutputPath string `json:"-"`
}

type QueueSnapshot struct {
	Evidence domain.Observation
	Records  []QueueRecord
}

type queueWire struct {
	ID         int64      `json:"id"`
	SeriesID   int64      `json:"seriesId"`
	EpisodeID  int64      `json:"episodeId"`
	MovieID    int64      `json:"movieId"`
	DownloadID string     `json:"downloadId"`
	Protocol   string     `json:"protocol"`
	Client     string     `json:"downloadClient"`
	Indexer    string     `json:"indexer"`
	Status     string     `json:"status"`
	State      string     `json:"trackedDownloadState"`
	Tracked    string     `json:"trackedDownloadStatus"`
	Size       *int64     `json:"size"`
	AddedAt    time.Time  `json:"added"`
	OutputPath string     `json:"outputPath"`
	Title      string     `json:"title"`
	Series     queueMedia `json:"series"`
	Movie      queueMedia `json:"movie"`
	Episode    struct {
		ID     int64  `json:"id"`
		Series int64  `json:"seriesId"`
		Season *int   `json:"seasonNumber"`
		Number *int   `json:"episodeNumber"`
		Title  string `json:"title"`
	} `json:"episode"`
	Messages []struct {
		Messages []string `json:"messages"`
	} `json:"statusMessages"`
}

type queueMedia struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	Year  int    `json:"year"`
}

func (a *Arr) Queue(ctx context.Context, generation domain.Revision, observedAt time.Time) (QueueSnapshot, error) {
	if generation == 0 || observedAt.IsZero() {
		return QueueSnapshot{}, ErrConfiguration
	}
	query := url.Values{"sortKey": {"added"}, "sortDirection": {"descending"}}
	if a.reader.product == domain.ProductSonarr {
		query.Set("includeUnknownSeriesItems", "true")
		query.Set("includeSeries", "true")
		query.Set("includeEpisode", "true")
	} else {
		query.Set("includeUnknownMovieItems", "true")
		query.Set("includeMovie", "true")
	}
	rows, err := scanPages[queueWire](ctx, a.reader, "/api/v3/queue", query, queuePageSize, func(row queueWire) int64 { return row.ID })
	out := QueueSnapshot{Records: make([]QueueRecord, 0, len(rows))}
	for _, row := range rows {
		out.Records = append(out.Records, a.queueRecord(row))
	}
	out.Evidence = a.observation("queue", "", generation, observedAt, out.Records, err)
	return out, err
}

func (a *Arr) QueueDetails(ctx context.Context, target domain.TargetRef) ([]QueueRecord, error) {
	if !target.Valid() || target.Instance != a.reader.instance {
		return nil, ErrConfiguration
	}
	query := url.Values{}
	switch {
	case a.reader.product == domain.ProductSonarr && target.Kind == domain.TargetEpisode:
		query.Set("episodeIds", strconv.FormatInt(target.ID, 10))
	case a.reader.product == domain.ProductRadarr && target.Kind == domain.TargetMovie:
		query.Set("movieId", strconv.FormatInt(target.ID, 10))
	default:
		return nil, ErrConfiguration
	}
	var rows []queueWire
	if err := a.reader.get(ctx, "/api/v3/queue/details", query, true, &rows); err != nil {
		return nil, err
	}
	if len(rows) > queuePageSize {
		return nil, ErrPartial
	}
	out := make([]QueueRecord, 0, len(rows))
	ids := make(map[int64]bool)
	for _, row := range rows {
		record := a.queueRecord(row)
		if row.ID <= 0 || ids[row.ID] || record.Target != target {
			return out, ErrPartial
		}
		ids[row.ID] = true
		out = append(out, record)
	}
	return out, nil
}

func (a *Arr) queueRecord(row queueWire) QueueRecord {
	var findings []QueueFinding
	seen := make(map[QueueFinding]bool)
	for _, message := range row.Messages {
		for _, line := range message.Messages {
			finding := queueFinding(a.reader.product, line)
			if !seen[finding] {
				seen[finding] = true
				findings = append(findings, finding)
			}
		}
	}
	record := QueueRecord{
		ID: row.ID, Target: a.target(row.SeriesID, row.EpisodeID, row.MovieID),
		DownloadID: exactToken(row.DownloadID, 256), Protocol: protocol(row.Protocol),
		ClientLabel: boundedText(row.Client, 128), IndexerLabel: boundedText(row.Indexer, 128),
		Status:        state(row.Status, "queued", "paused", "downloading", "completed", "failed", "warning", "delay", "downloadClientUnavailable", "fallback"),
		TrackedState:  state(row.State, "downloading", "downloadFailed", "downloadFailedPending", "importBlocked", "importPending", "importing", "imported", "ignored"),
		TrackedStatus: state(row.Tracked, "ok", "warning", "error"),
		SizeBytes:     nonnegative(row.Size), AddedAt: row.AddedAt, Findings: findings,
		OutputPath: exactToken(row.OutputPath, 4096),
	}
	if a.reader.product == domain.ProductSonarr {
		record.SeriesID = queueSeriesHint(row)
		if record.SeriesID == 0 {
			record.Target = domain.TargetRef{}
		}
	}
	record.Release = boundedText(row.Title, 300)
	if strings.ContainsAny(record.Release, "/\\") {
		record.Release = ""
	}
	switch record.Target.Kind {
	case domain.TargetEpisode:
		if row.Series.ID == row.SeriesID {
			record.MediaTitle = boundedText(row.Series.Title, 240)
		}
		if e := row.Episode; e.ID == row.EpisodeID && e.Series == row.SeriesID {
			record.EpisodeTitle = boundedText(e.Title, 240)
			if e.Season != nil && e.Number != nil && *e.Season >= 0 && *e.Season <= 999 && *e.Number > 0 && *e.Number <= 9999 {
				record.Episode = fmt.Sprintf("S%02dE%02d", *e.Season, *e.Number)
			}
		}
	case domain.TargetMovie:
		if row.Movie.ID == row.MovieID {
			record.MediaTitle = boundedText(row.Movie.Title, 240)
			if record.MediaTitle != "" && row.Movie.Year >= 1800 && row.Movie.Year <= 2200 {
				record.MediaTitle += fmt.Sprintf(" (%d)", row.Movie.Year)
			}
		}
	}
	return record
}

func queueSeriesHint(row queueWire) int64 {
	var hint int64
	for _, id := range [...]int64{row.SeriesID, row.Series.ID, row.Episode.Series} {
		if id <= 0 {
			continue
		}
		if hint != 0 && id != hint {
			return 0
		}
		hint = id
	}
	return hint
}

func queueFinding(product domain.Product, message string) QueueFinding {
	const noEligibleFiles = "no files found are eligible for import"
	line := strings.ToLower(strings.TrimSpace(message))
	path, hasPath := strings.CutPrefix(line, noEligibleFiles+" in ")
	switch {
	case strings.TrimSuffix(line, ".") == noEligibleFiles, hasPath && absoluteQueueMessagePath(path):
		return FindingNoEligibleFiles
	case product == domain.ProductSonarr &&
		strings.HasPrefix(line, "found matching series via grab history, but release was matched to series by id. automatic import is not possible"):
		return FindingSeriesIDOnly
	default:
		return FindingOtherMessage
	}
}

// Arr includes paths in its own filesystem format, which can differ from ours.
func absoluteQueueMessagePath(path string) bool {
	return strings.HasPrefix(path, "/") || strings.HasPrefix(path, `\\`) ||
		len(path) >= 3 && path[0] >= 'a' && path[0] <= 'z' && path[1] == ':' && (path[2] == '\\' || path[2] == '/')
}

func (a *Arr) target(seriesID, episodeID, movieID int64) domain.TargetRef {
	var ref domain.TargetRef
	switch {
	case a.reader.product == domain.ProductSonarr && movieID == 0:
		ref = domain.TargetRef{Instance: a.reader.instance, Kind: domain.TargetEpisode, ID: episodeID, SeriesID: seriesID}
	case a.reader.product == domain.ProductRadarr && seriesID == 0 && episodeID == 0:
		ref = domain.TargetRef{Instance: a.reader.instance, Kind: domain.TargetMovie, ID: movieID}
	}
	if !ref.Valid() {
		return domain.TargetRef{}
	}
	return ref
}

func (a *Arr) observation(kind domain.ObservationKind, selector string, generation domain.Revision, at time.Time, records any, err error) domain.Observation {
	value := domain.ObservationKnown
	if err != nil {
		value = domain.ObservationUnknown
	}
	return a.observationState(kind, selector, generation, at, records, value)
}

func (a *Arr) observationState(kind domain.ObservationKind, selector string, generation domain.Revision, at time.Time, records any, value domain.ObservationState) domain.Observation {
	complete := value == domain.ObservationKnown || value == domain.ObservationAbsent
	var ttl time.Duration
	if complete {
		ttl = 2 * time.Minute
	}
	fingerprint := digestObservationContent(records)
	contentHash := hex.EncodeToString(fingerprint[:])
	ref := a.reader.instance
	evidenceID := sha256.Sum256(fmt.Appendf(nil, "%q/%d/%s/%q/%d/%s/%s", ref.ID, ref.Epoch, kind, selector, generation, value, contentHash))
	return domain.Observation{
		ID: domain.EvidenceID(hex.EncodeToString(evidenceID[:])), ContentHash: contentHash,
		Scope:  domain.Scope{Kind: domain.ScopeInstance, Instance: a.reader.instance},
		Source: domain.Source(a.reader.product), Kind: kind, AdapterRevision: AdapterRevision,
		SchemaRevision: 1, SnapshotGeneration: generation, ObservedAt: at.UTC(), ExpiresAt: at.UTC().Add(ttl),
		State: value, Complete: complete,
	}
}

func digestObservationContent(value any) [sha256.Size]byte {
	body, _ := json.Marshal(value) // Only projected JSON-compatible primitives enter the digest.
	return sha256.Sum256(body)
}

func protocol(value string) domain.Protocol {
	switch strings.ToLower(value) {
	case "usenet":
		return domain.ProtocolUsenet
	case "torrent":
		return domain.ProtocolTorrent
	default:
		return domain.ProtocolUnknown
	}
}

func state(value string, known ...string) string {
	for _, candidate := range known {
		if strings.EqualFold(value, candidate) {
			return candidate
		}
	}
	return "unknown"
}

func exactToken(value string, maximum int) string {
	if len(value) > maximum || strings.IndexFunc(value, func(c rune) bool { return c < ' ' || c == 127 }) >= 0 {
		return ""
	}
	return value
}

func boundedText(value string, maximum int) string {
	return strings.TrimSpace(exactToken(value, maximum))
}

func nonnegative(value *int64) *int64 {
	if value != nil && *value < 0 {
		return nil
	}
	return value
}
