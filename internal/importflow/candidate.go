package importflow

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/policy"
)

type CandidateState string

const (
	Eligible CandidateState = "eligible"
	Rejected CandidateState = "rejected"
	Held     CandidateState = "held"
)

type Quality struct {
	ID         int64
	Name       string
	Source     string
	Resolution int64
	Modifier   *string
	Version    int
	Real       int
	IsRepack   *bool
}

type Language struct {
	ID   int64
	Name string
}

type Candidate struct {
	Key               string
	Targets           []domain.TargetRef
	Profiles          map[domain.TargetRef]domain.Revision `json:"-"`
	Size              int64
	Quality           *Quality
	Languages         []Language
	FormatIDs         []int64
	CustomFormatScore *int
	ReleaseType       *ReleaseType
	IndexerFlags      *int32
	ReleaseGroup      *string `json:"-"`
	Path              string  `json:"-"`
	FolderName        string  `json:"-"`
	Rejections        []Rejection
	State             CandidateState
	Reason            domain.ReasonCode
}

type ReleaseType string

const (
	ReleaseUnknown    ReleaseType = "unknown"
	ReleaseSingle     ReleaseType = "singleEpisode"
	ReleaseMulti      ReleaseType = "multiEpisode"
	ReleaseSeasonPack ReleaseType = "seasonPack"
)

type Rejection struct {
	Type   string
	Reason string `json:"-"`
}

type wireIdentity struct {
	ID        int64  `json:"id"`
	ProfileID *int64 `json:"qualityProfileId"`
	FileID    *int64 `json:"movieFileId"`
}

type wireEpisode struct {
	ID       int64  `json:"id"`
	SeriesID *int64 `json:"seriesId"`
	FileID   *int64 `json:"episodeFileId"`
}

type wireQuality struct {
	Quality struct {
		ID         int64   `json:"id"`
		Name       string  `json:"name"`
		Source     string  `json:"source"`
		Resolution *int64  `json:"resolution"`
		Modifier   *string `json:"modifier"`
	} `json:"quality"`
	Revision *struct {
		Version  *int  `json:"version"`
		Real     *int  `json:"real"`
		IsRepack *bool `json:"isRepack"`
	} `json:"revision"`
}

type wireCandidate struct {
	Path              string           `json:"path"`
	RelativePath      string           `json:"relativePath"`
	DownloadID        *string          `json:"downloadId"`
	FolderName        string           `json:"folderName"`
	Size              int64            `json:"size"`
	Series            *wireIdentity    `json:"series"`
	SeriesID          *int64           `json:"seriesId"`
	Episodes          []wireEpisode    `json:"episodes"`
	EpisodeIDs        []int64          `json:"episodeIds"`
	Movie             *wireIdentity    `json:"movie"`
	MovieID           *int64           `json:"movieId"`
	EpisodeFileID     *int64           `json:"episodeFileId"`
	MovieFileID       *int64           `json:"movieFileId"`
	Quality           wireQuality      `json:"quality"`
	Languages         []Language       `json:"languages"`
	CustomFormats     []wireIdentity   `json:"customFormats"`
	CustomFormatScore *int             `json:"customFormatScore"`
	IndexerFlags      json.RawMessage  `json:"indexerFlags"`
	ReleaseType       json.RawMessage  `json:"releaseType"`
	ReleaseGroup      json.RawMessage  `json:"releaseGroup"`
	Rejections        []*wireRejection `json:"rejections"`
}

type wireRejection struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
}

const (
	TargetProfileEvidence domain.ObservationKind = "import_target_profile"
	SourceAccessEvidence  domain.ObservationKind = "import_source_accessible"

	pathOutOfScope      domain.ReasonCode = "import_candidate_path_out_of_scope"
	downloadMismatch    domain.ReasonCode = "import_candidate_download_mismatch"
	identityAmbiguous   domain.ReasonCode = "import_candidate_identity_ambiguous"
	targetChanged       domain.ReasonCode = "import_candidate_target_changed"
	metadataUnknown     domain.ReasonCode = "import_candidate_metadata_unknown"
	nativeRejected      domain.ReasonCode = "import_candidate_rejected"
	targetNotNeeded     domain.ReasonCode = "import_candidate_target_not_needed"
	candidatesAmbiguous domain.ReasonCode = "import_candidates_ambiguous"
	noneUsable          domain.ReasonCode = "import_no_usable_candidate"
	sourceUnknown       domain.ReasonCode = "import_source_access_unknown"
	sourceInaccessible  domain.ReasonCode = "import_source_inaccessible"
)

func evaluate(instance domain.Instance, job Job, rows []wireCandidate, now time.Time) Preview {
	out := Preview{Job: job.Ref, NativeReview: true, Candidates: make([]Candidate, 0, len(rows))}
	expected := make(map[domain.TargetRef]Target, len(job.Targets))
	for _, target := range job.Targets {
		expected[target.Ref] = target
	}
	paths := make(map[string]int, len(rows))
	covered := make(map[domain.TargetRef]int, len(expected))
	ambiguous := false
	for _, row := range rows {
		position := len(out.Candidates)
		candidate := decodeCandidate(instance.Product, job, row, expected)
		out.Candidates = append(out.Candidates, candidate)
		if candidate.Key != "" {
			if other, duplicate := paths[candidate.Key]; duplicate {
				markAmbiguous(out.Candidates, other, position)
				ambiguous = true
			}
			paths[candidate.Key] = position
		}
		if candidate.State == Held && candidate.Reason != targetNotNeeded {
			ambiguous = true
		}
		if candidate.State != Eligible {
			continue
		}
		for _, target := range candidate.Targets {
			if other, overlap := covered[target]; overlap {
				markAmbiguous(out.Candidates, other, position)
				ambiguous = true
			}
			covered[target] = position
		}
	}
	sourceKnown := sourceAccessKnown(instance, job, now)
	switch {
	case ambiguous:
		out.Assessment, out.Reason = Ambiguous, candidatesAmbiguous
	case sourceKnown && !job.SourceAccessible.Value:
		out.Assessment, out.Reason = Uncertain, sourceInaccessible
	case len(rows) == 0 && !sourceKnown:
		out.Assessment, out.Reason = Uncertain, sourceUnknown
	case len(covered) == 0:
		out.Assessment, out.Reason = NoUsableCandidate, noneUsable
	case len(covered) == len(expected):
		out.Assessment, out.Reason = CompleteCandidates, "import_preview_complete"
	default:
		out.Assessment, out.Reason = PartialCandidates, "import_preview_partial"
	}
	out.Evidence = makeEvidence(instance, job, out, now)
	return out
}

func markAmbiguous(candidates []Candidate, positions ...int) {
	for _, position := range positions {
		candidates[position].State, candidates[position].Reason = Held, candidatesAmbiguous
	}
}

func sourceAccessKnown(instance domain.Instance, job Job, now time.Time) bool {
	evidence := job.SourceAccessible.Observation
	return evidence.Kind == SourceAccessEvidence && evidence.SourceRecord == job.DownloadID && fromOwner(instance, evidence, now)
}

func makeEvidence(instance domain.Instance, job Job, preview Preview, now time.Time) domain.Observation {
	type targetBinding struct {
		Ref             domain.TargetRef
		ProfileID       int64
		ProfileRevision domain.Revision
		FileID          int64
		Intent          policy.Intent
		Evidence        domain.EvidenceID
	}
	targets := make([]targetBinding, 0, len(job.Targets))
	queue := job.Queue.Evidence
	expires := observationExpiry(queue)
	for _, target := range job.Targets {
		targets = append(targets, targetBinding{target.Ref, target.ProfileID, target.ProfileRevision, target.FileID, target.Intent, target.Evidence.ID})
		if expiry := observationExpiry(target.Evidence); expiry.Before(expires) {
			expires = expiry
		}
	}
	var source domain.EvidenceID
	if sourceAccessKnown(instance, job, now) {
		observed := job.SourceAccessible.Observation
		source = observed.ID
		if expiry := observationExpiry(observed); expiry.Before(expires) {
			expires = expiry
		}
	}
	canonical, _ := json.Marshal(struct {
		Job        domain.JobRef
		Queue      domain.EvidenceID
		Source     domain.EvidenceID
		Targets    []targetBinding
		Assessment Assessment
		Reason     domain.ReasonCode
		Candidates []Candidate
	}{job.Ref, queue.ID, source, targets, preview.Assessment, preview.Reason, preview.Candidates})
	state, complete := domain.ObservationKnown, true
	if preview.Assessment == Uncertain || !now.Before(expires) {
		state, complete = domain.ObservationUnknown, false
	}
	hash := sha256.Sum256(canonical)
	fingerprint := hex.EncodeToString(hash[:])
	return domain.Observation{
		ID: domain.EvidenceID(fingerprint), ContentHash: fingerprint, Scope: queue.Scope,
		Source: domain.Source(instance.Product), Kind: "manual_import_preview", AdapterRevision: adapter,
		SchemaRevision: 1, SnapshotGeneration: queue.SnapshotGeneration, ObservedAt: now,
		ExpiresAt: expires, State: state, Complete: complete,
	}
}

func observationExpiry(evidence domain.Observation) time.Time {
	if maximum := evidence.ObservedAt.Add(maxReadAge); maximum.Before(evidence.ExpiresAt) {
		return maximum
	}
	return evidence.ExpiresAt
}

func decodeCandidate(product domain.Product, job Job, row wireCandidate, expected map[domain.TargetRef]Target) Candidate {
	out := Candidate{State: Held}
	if row.DownloadID != nil && *row.DownloadID != job.DownloadID {
		out.Reason = downloadMismatch
		return out
	}
	keyPath, scoped := candidatePath(job, row.Path, row.RelativePath)
	if !scoped || row.EpisodeFileID != nil || row.MovieFileID != nil {
		out.Reason = pathOutOfScope
		return out
	}
	key := sha256.Sum256(fmt.Appendf(nil, "%q/%d/%q/%d/%q", job.Ref.Instance.ID, job.Ref.Instance.Epoch, job.Ref.ID, job.Ref.Generation, keyPath))
	out.Key, out.Path, out.Size = hex.EncodeToString(key[:]), row.Path, row.Size
	if cleanText(row.FolderName, 512) {
		out.FolderName = row.FolderName
	}
	metadataValid := candidateMetadata(product, &out, row)
	out.Targets = candidateTargets(product, job.Ref.Instance, row)
	if len(out.Targets) == 0 && hasTargetIdentity(row) {
		out.Reason = identityAmbiguous
		return out
	}
	out.Rejections = cleanRejections(row.Rejections)
	if row.Rejections == nil || out.Rejections == nil {
		out.Reason = metadataUnknown
		return out
	}
	out.Profiles = make(map[domain.TargetRef]domain.Revision, len(out.Targets))
	for _, ref := range out.Targets {
		target, exists := expected[ref]
		if !exists {
			out.Reason = identityAmbiguous
			return out
		}
		if !matchesCurrentTarget(row, target) {
			out.Reason = targetChanged
			return out
		}
		out.Profiles[ref] = target.ProfileRevision
	}
	if len(out.Rejections) > 0 {
		out.State, out.Reason = Rejected, nativeRejected
		return out
	}
	if len(out.Targets) == 0 {
		out.Reason = identityAmbiguous
		return out
	}
	for _, ref := range out.Targets {
		if intent := expected[ref].Intent; intent != policy.NeedFirstImport && intent != policy.NeedUpgrade {
			out.Reason = targetNotNeeded
			return out
		}
	}
	if !metadataValid {
		out.Reason = metadataUnknown
		return out
	}
	out.State = Eligible
	return out
}

func candidatePath(job Job, path, relative string) (string, bool) {
	candidate, valid := parsePath(job.PathStyle, path)
	if !valid || len(candidate.parts) == 0 || strings.HasSuffix(path, "/") ||
		(job.PathStyle == WindowsPath && strings.HasSuffix(path, "\\")) || !pathWithin(job.PathStyle, job.OutputPath, path, true) {
		return "", false
	}
	// Arr also supports downloads whose tracked output is the file itself.
	// In that case the preview's relative path is its single base name.
	if samePath(job.PathStyle, job.OutputPath, path) {
		if job.PathStyle == WindowsPath {
			relative = strings.ToLower(relative)
		}
		if relative != candidate.parts[len(candidate.parts)-1] {
			return "", false
		}
	}
	for _, root := range job.LibraryRoots {
		if pathWithin(job.PathStyle, root, path, true) {
			return "", false
		}
	}
	return candidate.volume + "/" + strings.Join(candidate.parts, "/"), true
}

func hasTargetIdentity(row wireCandidate) bool {
	return row.Series != nil || row.SeriesID != nil || len(row.Episodes) != 0 || len(row.EpisodeIDs) != 0 ||
		row.Movie != nil || row.MovieID != nil
}

func candidateTargets(product domain.Product, instance domain.InstanceRef, row wireCandidate) []domain.TargetRef {
	if product == domain.ProductRadarr {
		if row.Movie == nil || row.Movie.ID <= 0 || !optionalIntMatches(row.MovieID, row.Movie.ID) ||
			row.Series != nil || row.SeriesID != nil || len(row.Episodes) > 0 || len(row.EpisodeIDs) > 0 {
			return nil
		}
		return []domain.TargetRef{{Instance: instance, Kind: domain.TargetMovie, ID: row.Movie.ID}}
	}
	if row.Series == nil || row.Series.ID <= 0 || !optionalIntMatches(row.SeriesID, row.Series.ID) ||
		row.Movie != nil || row.MovieID != nil || len(row.Episodes) == 0 || len(row.Episodes) > maxCandidates {
		return nil
	}
	targets := make([]domain.TargetRef, 0, len(row.Episodes))
	ids := make(map[int64]bool, len(row.Episodes))
	for _, episode := range row.Episodes {
		if episode.ID <= 0 || ids[episode.ID] || !optionalIntMatches(episode.SeriesID, row.Series.ID) {
			return nil
		}
		ids[episode.ID] = true
		targets = append(targets, domain.TargetRef{Instance: instance, Kind: domain.TargetEpisode, ID: episode.ID, SeriesID: row.Series.ID})
	}
	if row.EpisodeIDs != nil && !identicalIDs(row.EpisodeIDs, ids) {
		return nil
	}
	slices.SortFunc(targets, func(a, b domain.TargetRef) int { return cmp.Compare(a.ID, b.ID) })
	return targets
}

func identicalIDs(got []int64, expected map[int64]bool) bool {
	if len(got) != len(expected) {
		return false
	}
	seen := make(map[int64]bool, len(got))
	for _, id := range got {
		if !expected[id] || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

func optionalIntMatches(value *int64, want int64) bool { return value == nil || *value == want }

func requiredIntMatches(value *int64, want int64) bool { return value != nil && *value == want }

func matchesCurrentTarget(row wireCandidate, target Target) bool {
	if target.Ref.Kind == domain.TargetMovie {
		return requiredIntMatches(row.Movie.ProfileID, target.ProfileID) && requiredIntMatches(row.Movie.FileID, target.FileID)
	}
	if !requiredIntMatches(row.Series.ProfileID, target.ProfileID) {
		return false
	}
	for _, episode := range row.Episodes {
		if episode.ID == target.Ref.ID {
			return requiredIntMatches(episode.FileID, target.FileID)
		}
	}
	return false
}

func candidateMetadata(product domain.Product, out *Candidate, row wireCandidate) bool {
	out.Languages = nativeLanguages(row.Languages)
	out.FormatIDs = nativeIDs(row.CustomFormats)
	out.CustomFormatScore = row.CustomFormatScore
	out.IndexerFlags = nativeFlags(row.IndexerFlags)
	out.ReleaseType = nativeReleaseType(row.ReleaseType)
	group, groupValid := nullableReleaseGroup(row.ReleaseGroup)
	out.ReleaseGroup = group
	quality, revision := row.Quality.Quality, row.Quality.Revision
	if quality.ID > 0 && cleanText(quality.Name, 128) && cleanText(quality.Source, 64) &&
		quality.Resolution != nil && *quality.Resolution >= 0 &&
		((quality.Modifier == nil && product == domain.ProductSonarr) || (quality.Modifier != nil && cleanText(*quality.Modifier, 64))) &&
		revision != nil && revision.Version != nil && *revision.Version > 0 && revision.Real != nil && *revision.Real >= 0 {
		out.Quality = &Quality{ID: quality.ID, Name: quality.Name, Source: quality.Source, Resolution: *quality.Resolution,
			Modifier: quality.Modifier, Version: *revision.Version, Real: *revision.Real, IsRepack: revision.IsRepack}
	}
	return out.Size > 0 && out.Quality != nil && len(out.Languages) > 0 && out.FormatIDs != nil && groupValid &&
		row.CustomFormatScore != nil && out.IndexerFlags != nil &&
		((product == domain.ProductSonarr && out.ReleaseType != nil) || (product == domain.ProductRadarr && row.ReleaseType == nil))
}

func nativeLanguages(languages []Language) []Language {
	if len(languages) == 0 || len(languages) > maxCandidates {
		return nil
	}
	seen := make(map[int64]bool, len(languages))
	for _, language := range languages {
		if language.ID <= 0 || !cleanText(language.Name, 128) || seen[language.ID] {
			return nil
		}
		seen[language.ID] = true
	}
	return languages
}

func nullableReleaseGroup(raw json.RawMessage) (*string, bool) {
	if raw == nil {
		return nil, true
	}
	var group *string
	if json.Unmarshal(raw, &group) != nil || (group != nil && *group != "" && !cleanText(*group, 256)) {
		return nil, false
	}
	return group, true
}

func nativeReleaseType(raw json.RawMessage) *ReleaseType {
	var release ReleaseType
	if json.Unmarshal(raw, &release) != nil {
		return nil
	}
	switch release {
	case ReleaseUnknown, ReleaseSingle, ReleaseMulti, ReleaseSeasonPack:
		return &release
	default:
		return nil
	}
}

func nativeIDs(records []wireIdentity) []int64 {
	if records == nil || len(records) > maxCandidates {
		return nil
	}
	ids := make([]int64, 0, len(records))
	seen := make(map[int64]bool, len(records))
	for _, record := range records {
		if record.ID <= 0 || seen[record.ID] {
			return nil
		}
		seen[record.ID] = true
		ids = append(ids, record.ID)
	}
	slices.Sort(ids)
	return ids
}

func nativeFlags(raw json.RawMessage) *int32 {
	var flags *int32
	if json.Unmarshal(raw, &flags) != nil {
		return nil
	}
	return flags
}

func cleanRejections(rows []*wireRejection) []Rejection {
	if rows == nil || len(rows) > maxCandidates {
		return nil
	}
	out := make([]Rejection, 0, len(rows))
	for _, row := range rows {
		if row == nil || !cleanText(row.Reason, 512) {
			return nil
		}
		kind := "unclassified"
		switch row.Type {
		case "permanent", "temporary":
			kind = row.Type
		}
		out = append(out, Rejection{Type: kind, Reason: row.Reason})
	}
	return out
}
