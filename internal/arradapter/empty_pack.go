package arradapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rnodern/rescuarr/internal/arrwire"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/observe"
)

const (
	RouteEmptyPackReject Route = "sonarr_empty_pack_reject"
	RouteEmptyPackSearch Route = "sonarr_empty_pack_search"
	maxPackEpisodes            = 128
)

type EmptyPackScope struct {
	QueueIDs            []int64
	DownloadID, Release string
	SeriesID            int64
	EpisodeIDs          []int64
}

// EmptyPackAdapter implements the two independently-qualified Sonarr 5
// effects. Its worker must persist each send before dispatch and must verify
// rejection before selecting the episodes to search.
type EmptyPackAdapter struct {
	arr      *observe.Arr
	binding  Binding
	base     url.URL
	key      string
	exchange FixtureTransport
	qualify  Qualify
	now      func() time.Time
}

type packTransport FixtureTransport

func (fn packTransport) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

func NewEmptyPackAdapter(config Config, exchange FixtureTransport, qualify Qualify, now func() time.Time) (*EmptyPackAdapter, error) {
	instance := config.Instance
	base, ok := parseBase(instance.BaseURL)
	if !ok || !instance.Ref.Valid() || instance.Product != domain.ProductSonarr ||
		instance.UpstreamVersion != arrwire.SonarrIDOnlyImportVersion || instance.ConnectionRevision == 0 ||
		config.AdapterRevision == "" || config.SettingsFingerprint == ([sha256.Size]byte{}) ||
		!validAPIKey(config.APIKey) || exchange == nil || qualify == nil || now == nil {
		return nil, ErrConfiguration
	}
	arr, err := observe.NewArr(observe.Config{Instance: instance, APIKey: config.APIKey}, packTransport(exchange))
	if err != nil {
		return nil, ErrConfiguration
	}
	binding := Binding{Instance: instance.Ref, Product: instance.Product, Version: instance.UpstreamVersion,
		ConnectionRevision: instance.ConnectionRevision, AdapterRevision: config.AdapterRevision, SettingsFingerprint: config.SettingsFingerprint}
	return &EmptyPackAdapter{arr: arr, binding: binding, base: base, key: config.APIKey, exchange: exchange, qualify: qualify, now: now}, nil
}

// MatchesInstance prevents a qualified exchange from being reused for a
// different URL, installation epoch or connection revision.
func (a *EmptyPackAdapter) MatchesInstance(instance domain.Instance) bool {
	base, ok := parseBase(instance.BaseURL)
	return ok && base == a.base && instance.Ref == a.binding.Instance &&
		instance.Product == a.binding.Product && instance.UpstreamVersion == a.binding.Version &&
		instance.ConnectionRevision == a.binding.ConnectionRevision
}

func (a *EmptyPackAdapter) Preflight(ctx context.Context, scope EmptyPackScope) error {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if _, err := a.certified(ctx, RouteEmptyPackSearch); err != nil {
		return err
	}
	_, err := a.rejectionPreflight(ctx, scope)
	return err
}

// Reject makes one native DELETE against a single alias. Sonarr applies it to
// the entire tracked download. HTTP acknowledgement alone proves no effect.
func (a *EmptyPackAdapter) Reject(ctx context.Context, scope EmptyPackScope) error {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	qualification, err := a.rejectionPreflight(ctx, scope)
	if err != nil {
		return err
	}
	query := url.Values{"removeFromClient": {"false"}, "blocklist": {"true"}, "skipRedownload": {"true"}, "changeCategory": {"false"}}
	path := "/api/v3/queue/" + strconv.FormatInt(slices.Min(scope.QueueIDs), 10)
	_, err = a.dispatch(ctx, qualification, RouteEmptyPackReject, RouteRejectPreserve, http.MethodDelete, path, query, nil, nil)
	return err
}

func (a *EmptyPackAdapter) rejectionPreflight(ctx context.Context, scope EmptyPackScope) (Qualification, error) {
	if !validPackScope(scope) {
		return Qualification{}, ErrPrepared
	}
	qualification, err := a.certified(ctx, RouteEmptyPackReject)
	if err != nil {
		return Qualification{}, err
	}
	history, err := a.packHistory(ctx, scope)
	if err != nil {
		return Qualification{}, err
	}
	indexer, grabbed := packGrabs(scope, history, a.now())
	if !grabbed || slices.ContainsFunc(history, func(row packHistoryRow) bool {
		return row.Event == "downloadFailed" || row.Event == "downloadIgnored"
	}) {
		return Qualification{}, ErrReadback
	}
	blocklist, err := a.packBlocklist(ctx, scope)
	if err != nil {
		return Qualification{}, err
	}
	if slices.ContainsFunc(blocklist, func(row packBlocklistRow) bool { return row.Title == scope.Release && row.Indexer == indexer }) {
		return Qualification{}, ErrReadback
	}
	query := url.Values{"downloadId": {scope.DownloadID}, "filterExistingFiles": {"false"}}
	var candidates []json.RawMessage
	if err := a.readPackJSON(ctx, "/api/v3/manualimport", query, &candidates); err != nil || candidates == nil || len(candidates) != 0 {
		return Qualification{}, ErrReadback
	}
	queue, err := a.packQueue(ctx)
	if err != nil || !exactPackAliases(scope, queue) {
		return Qualification{}, ErrReadback
	}
	return qualification, nil
}

func exactPackAliases(scope EmptyPackScope, rows []observe.QueueRecord) bool {
	aliases := make([]int64, 0, len(scope.QueueIDs))
	episodes := make([]int64, 0, len(scope.EpisodeIDs))
	client := ""
	path := ""
	for _, row := range rows {
		if row.DownloadID != scope.DownloadID {
			if slices.Contains(scope.QueueIDs, row.ID) {
				return false
			}
			continue
		}
		if row.Target.Kind != domain.TargetEpisode || row.Target.SeriesID != scope.SeriesID ||
			row.Protocol != domain.ProtocolUsenet || row.Release != scope.Release || row.ClientLabel == "" ||
			row.OutputPath == "" || !completedPackWithoutFiles(row) {
			return false
		}
		if client != "" && (row.ClientLabel != client || row.OutputPath != path) {
			return false
		}
		client, path = row.ClientLabel, row.OutputPath
		aliases = append(aliases, row.ID)
		episodes = append(episodes, row.Target.ID)
	}
	return samePackIDs(aliases, scope.QueueIDs) && samePackIDs(episodes, scope.EpisodeIDs)
}

func completedPackWithoutFiles(row observe.QueueRecord) bool {
	return row.Status == "completed" && (row.TrackedState == "importPending" || row.TrackedState == "importBlocked") &&
		slices.Contains(row.Findings, observe.FindingNoEligibleFiles)
}

func (a *EmptyPackAdapter) VerifyRejected(ctx context.Context, scope EmptyPackScope, sentAt time.Time) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if !validPackScope(scope) || sentAt.IsZero() || sentAt.After(a.now()) {
		return false, ErrPrepared
	}
	if err := a.currentVersion(ctx); err != nil {
		return false, err
	}
	history, err := a.packHistory(ctx, scope)
	if err != nil {
		return false, err
	}
	indexer, grabbed := packGrabs(scope, history, sentAt)
	now := a.now()
	// Sonarr's persisted dates may have second precision. Preflight proved
	// the exact failures and blocklist entry absent before the first write.
	earliest := sentAt.Truncate(time.Second)
	if !grabbed || !packFailed(scope, history, earliest, now) {
		return false, nil
	}
	blocklist, err := a.packBlocklist(ctx, scope)
	if err != nil {
		return false, err
	}
	matches := 0
	for _, row := range blocklist {
		if row.Title == scope.Release && row.Indexer == indexer && samePackIDs(row.EpisodeIDs, scope.EpisodeIDs) &&
			!row.Date.IsZero() && !row.Date.Before(earliest) && !row.Date.After(now) {
			matches++
		}
	}
	if matches != 1 {
		return false, nil
	}
	queue, err := a.packQueue(ctx)
	if err != nil {
		return false, err
	}
	return packGone(scope, queue), nil
}

// Search only admits an explicit subset of the original pack. The worker's
// post-reject decision is refreshed again just before dispatch.
func (a *EmptyPackAdapter) Search(ctx context.Context, scope EmptyPackScope, episodeIDs []int64) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if !validPackScope(scope) || !validPackIDs(episodeIDs) {
		return 0, ErrPrepared
	}
	for _, id := range episodeIDs {
		if !slices.Contains(scope.EpisodeIDs, id) {
			return 0, ErrPrepared
		}
	}
	qualification, err := a.certified(ctx, RouteEmptyPackSearch)
	if err != nil {
		return 0, err
	}
	needs, err := a.arr.SonarrEpisodeNeeds(ctx, scope.SeriesID, episodeIDs, a.now())
	if err != nil {
		return 0, ErrReadback
	}
	for _, id := range episodeIDs {
		if needs[id] != observe.EpisodeMissing {
			return 0, ErrReadback
		}
	}
	queue, err := a.packQueue(ctx)
	if err != nil || !packGone(scope, queue) || !searchClear(scope.SeriesID, episodeIDs, queue) {
		return 0, ErrReadback
	}
	body, err := json.Marshal(struct {
		Name       string  `json:"name"`
		EpisodeIDs []int64 `json:"episodeIds"`
	}{"EpisodeSearch", episodeIDs})
	if err != nil {
		return 0, ErrPrepared
	}
	return a.dispatch(ctx, qualification, RouteEmptyPackSearch, RouteEpisodeSearch, http.MethodPost, "/api/v3/command", nil, body, episodeIDs)
}

func searchClear(series int64, ids []int64, queue []observe.QueueRecord) bool {
	for _, row := range queue {
		if row.Status == "failed" || row.TrackedState == "downloadFailed" || completedPackWithoutFiles(row) {
			continue
		}
		if row.Target.Valid() {
			if row.Target.SeriesID == series && slices.Contains(ids, row.Target.ID) {
				return false
			}
		} else if row.SeriesID == 0 || row.SeriesID == series {
			return false
		}
	}
	return true
}

func packGone(scope EmptyPackScope, queue []observe.QueueRecord) bool {
	return !slices.ContainsFunc(queue, func(row observe.QueueRecord) bool {
		return row.DownloadID == scope.DownloadID || slices.Contains(scope.QueueIDs, row.ID)
	})
}

func (a *EmptyPackAdapter) certified(ctx context.Context, route Route) (Qualification, error) {
	binding := a.binding
	binding.Route = route
	qualification, err := a.qualify(ctx, binding)
	if err != nil || ctx.Err() != nil || !validQualification(binding, qualification, a.now()) {
		return Qualification{}, ErrQualification
	}
	if err := a.currentVersion(ctx); err != nil {
		return Qualification{}, err
	}
	return qualification, nil
}

func (a *EmptyPackAdapter) currentVersion(ctx context.Context) error {
	status, err := a.arr.Status(ctx)
	if err != nil || ctx.Err() != nil || status.Version != a.binding.Version {
		return ErrQualification
	}
	return nil
}

func (a *EmptyPackAdapter) packQueue(ctx context.Context) ([]observe.QueueRecord, error) {
	queue, err := a.arr.Queue(ctx, 1, a.now())
	if err != nil || ctx.Err() != nil || !queue.Evidence.Fresh(a.now()) {
		return nil, ErrReadback
	}
	return queue.Records, nil
}

func (a *EmptyPackAdapter) packRequest(ctx context.Context, method, path string, query url.Values, body []byte) (*http.Request, error) {
	target := a.base
	target.Path += path
	target.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, ErrConfiguration
	}
	req.GetBody = nil
	req.Close = true
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("X-Api-Key", a.key)
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (a *EmptyPackAdapter) dispatch(ctx context.Context, qualification Qualification, route, responseRoute Route, method, path string, query url.Values, body []byte, episodes []int64) (int64, error) {
	req, err := a.packRequest(ctx, method, path, query, body)
	if err != nil {
		return 0, err
	}
	binding := a.binding
	binding.Route = route
	if ctx.Err() != nil || !validQualification(binding, qualification, a.now()) {
		return 0, ErrQualification
	}
	res, err := a.exchange(req)
	if res != nil && res.Body != nil {
		defer res.Body.Close()
	}
	if err != nil || ctx.Err() != nil {
		return 0, ErrUncertain
	}
	if responseRoute == RouteRejectPreserve {
		if _, ok := acknowledge(responseRoute, res); ok {
			return 0, nil
		}
		return 0, ErrUncertain
	}
	return packSearchAcknowledgement(res, episodes)
}

func packSearchAcknowledgement(res *http.Response, episodes []int64) (int64, error) {
	if res == nil || res.Body == nil || res.StatusCode != http.StatusCreated || !jsonResponse(res) || !packIdentityEncoding(res) {
		return 0, ErrUncertain
	}
	body, ok := readAcknowledgement(res, maxAckBytes)
	if !ok {
		return 0, ErrUncertain
	}
	id, ok := commandID(body, RouteEpisodeSearch)
	if !ok {
		return 0, ErrUncertain
	}
	var command struct {
		Status string `json:"status"`
		Body   struct {
			EpisodeIDs []int64 `json:"episodeIds"`
		} `json:"body"`
	}
	if err := json.Unmarshal(body, &command); err != nil || !samePackIDs(command.Body.EpisodeIDs, episodes) ||
		!slices.Contains([]string{"queued", "started", "completed"}, command.Status) {
		return 0, ErrUncertain
	}
	return int64(id), nil
}

func (a *EmptyPackAdapter) readPackJSON(ctx context.Context, path string, query url.Values, output any) error {
	req, err := a.packRequest(ctx, http.MethodGet, path, query, nil)
	if err != nil {
		return err
	}
	res, err := a.exchange(req)
	if res != nil && res.Body != nil {
		defer res.Body.Close()
	}
	if err != nil || res == nil || res.Body == nil || res.StatusCode != http.StatusOK || !jsonResponse(res) || !packIdentityEncoding(res) {
		return ErrReadback
	}
	body, ok := readAcknowledgement(res, 2<<20)
	if !ok || bytes.Equal(bytes.TrimSpace(body), []byte("null")) || json.Unmarshal(body, output) != nil || ctx.Err() != nil {
		return ErrReadback
	}
	return nil
}

func packIdentityEncoding(response *http.Response) bool {
	encoding := response.Header.Values("Content-Encoding")
	return len(encoding) == 0 || (len(encoding) == 1 && strings.EqualFold(encoding[0], "identity"))
}

type packHistoryRow struct {
	ID         int64     `json:"id"`
	SeriesID   int64     `json:"seriesId"`
	EpisodeID  int64     `json:"episodeId"`
	DownloadID string    `json:"downloadId"`
	Title      string    `json:"sourceTitle"`
	Event      string    `json:"eventType"`
	Date       time.Time `json:"date"`
	Data       struct {
		Indexer string `json:"indexer"`
	} `json:"data"`
}

type packBlocklistRow struct {
	ID         int64     `json:"id"`
	SeriesID   int64     `json:"seriesId"`
	EpisodeIDs []int64   `json:"episodeIds"`
	Title      string    `json:"sourceTitle"`
	Indexer    string    `json:"indexer"`
	Protocol   string    `json:"protocol"`
	Date       time.Time `json:"date"`
}

func (a *EmptyPackAdapter) packHistory(ctx context.Context, scope EmptyPackScope) ([]packHistoryRow, error) {
	query := url.Values{"downloadId": {scope.DownloadID}, "sortKey": {"date"}, "sortDirection": {"descending"}}
	rows, err := readPackPage(ctx, a, "/api/v3/history", query, func(row packHistoryRow) int64 { return row.ID })
	for _, row := range rows {
		if row.DownloadID != scope.DownloadID || row.SeriesID != scope.SeriesID {
			return nil, ErrReadback
		}
	}
	return rows, err
}

func (a *EmptyPackAdapter) packBlocklist(ctx context.Context, scope EmptyPackScope) ([]packBlocklistRow, error) {
	query := url.Values{"seriesIds": {strconv.FormatInt(scope.SeriesID, 10)}, "protocols": {"usenet"}, "sortKey": {"date"}, "sortDirection": {"descending"}}
	rows, err := readPackPage(ctx, a, "/api/v3/blocklist", query, func(row packBlocklistRow) int64 { return row.ID })
	for _, row := range rows {
		if row.SeriesID != scope.SeriesID || row.Protocol != "usenet" {
			return nil, ErrReadback
		}
	}
	return rows, err
}

func readPackPage[T any](ctx context.Context, a *EmptyPackAdapter, path string, query url.Values, id func(T) int64) ([]T, error) {
	// Scope is at most 128 episodes. A bounded single page can contain the
	// original grab and failure for every target, with room for other history.
	const size = 400
	query.Set("page", "1")
	query.Set("pageSize", strconv.Itoa(size))
	var p struct {
		Page    int  `json:"page"`
		Size    int  `json:"pageSize"`
		Total   *int `json:"totalRecords"`
		Records []T  `json:"records"`
	}
	if err := a.readPackJSON(ctx, path, query, &p); err != nil || p.Page != 1 || p.Size != size ||
		p.Total == nil || p.Records == nil || *p.Total != len(p.Records) || len(p.Records) > size {
		return nil, ErrReadback
	}
	seen := make(map[int64]bool, len(p.Records))
	for _, row := range p.Records {
		key := id(row)
		if key <= 0 || seen[key] {
			return nil, ErrReadback
		}
		seen[key] = true
	}
	return p.Records, nil
}

func packGrabs(scope EmptyPackScope, history []packHistoryRow, latest time.Time) (string, bool) {
	indexer := ""
	var episodeIDs []int64
	for _, row := range history {
		if row.Event != "grabbed" {
			continue
		}
		if row.Title != scope.Release || row.Date.IsZero() || row.Date.After(latest) || row.Data.Indexer == "" ||
			(indexer != "" && indexer != row.Data.Indexer) {
			return "", false
		}
		indexer = row.Data.Indexer
		episodeIDs = append(episodeIDs, row.EpisodeID)
	}
	return indexer, samePackIDs(episodeIDs, scope.EpisodeIDs)
}

func packFailed(scope EmptyPackScope, history []packHistoryRow, sentAt, now time.Time) bool {
	var episodeIDs []int64
	for _, row := range history {
		if row.Event != "downloadFailed" {
			continue
		}
		if row.Title != scope.Release || row.Date.IsZero() || row.Date.Before(sentAt) || row.Date.After(now) {
			return false
		}
		episodeIDs = append(episodeIDs, row.EpisodeID)
	}
	return samePackIDs(episodeIDs, scope.EpisodeIDs)
}

func validPackScope(scope EmptyPackScope) bool {
	return validPackText(scope.DownloadID, 256) && validPackText(scope.Release, 300) &&
		!strings.ContainsAny(scope.Release, "/\\") && scope.SeriesID > 0 && scope.SeriesID <= math.MaxInt32 &&
		len(scope.EpisodeIDs) >= 2 && len(scope.QueueIDs) == len(scope.EpisodeIDs) &&
		validPackIDs(scope.QueueIDs) && validPackIDs(scope.EpisodeIDs)
}

func validPackText(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && strings.TrimSpace(value) == value &&
		strings.IndexFunc(value, func(r rune) bool { return r < ' ' || r == 127 }) < 0
}

func validPackIDs(ids []int64) bool {
	if len(ids) == 0 || len(ids) > maxPackEpisodes {
		return false
	}
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id <= 0 || id > math.MaxInt32 || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

func samePackIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}
