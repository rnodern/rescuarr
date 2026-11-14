package importflow

import (
	"context"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/observe"
)

const sonarrQueueTestVersion = "4.0.20.3012"

// SupportsEpisodeNameReview covers only the download-scoped GET. Import
// certification is independent and still uses the normal discovery contract.
func SupportsEpisodeNameReview(product domain.Product, version string) bool {
	return product == domain.ProductSonarr && (version == sonarrQueueTestVersion || version == SonarrVersion)
}

type EpisodeNameReview struct {
	Series       string
	Episode      string
	EpisodeTitle string
	Folder       string
	Filename     string
	TitleInNames bool
	Matched      bool
	Reason       string
	ArrRejection bool
}

// ReviewEpisodeNames compares the actual candidate path returned by Sonarr
// with the exact episode already identified in the fresh queue record.
func (c *Client) ReviewEpisodeNames(ctx context.Context, row observe.QueueRecord, style PathStyle, roots []string) (EpisodeNameReview, error) {
	if !SupportsEpisodeNameReview(c.instance.Product, c.instance.UpstreamVersion) ||
		row.Target.Instance != c.instance.Ref || row.Target.Kind != domain.TargetEpisode ||
		!row.Target.Valid() || row.DownloadID == "" ||
		!slices.Contains(row.Findings, observe.FindingSeriesIDOnly) {
		return EpisodeNameReview{}, ErrInvalid
	}
	review := EpisodeNameReview{Series: row.MediaTitle, Episode: row.Episode, EpisodeTitle: row.EpisodeTitle}
	if row.Status != "completed" || (row.TrackedState != "importBlocked" && row.TrackedState != "importPending") {
		review.Reason = "Sonarr is not reporting a completed download awaiting import or manual review. Check its current queue status."
		return review, nil
	}
	if len(seriesWords(row.MediaTitle)) == 0 || !singleEpisode(row.Episode).valid() {
		review.Reason = "Sonarr did not provide a series name and exact season and episode number."
		return review, nil
	}
	if _, ok := parsePath(style, row.OutputPath); !ok {
		review.Reason = "Sonarr did not provide a usable download location."
		return review, nil
	}
	candidates, err := c.get(ctx, row.DownloadID)
	if err != nil {
		return EpisodeNameReview{}, err
	}
	return checkEpisodeNames(review, row, style, roots, candidates), nil
}

func checkEpisodeNames(review EpisodeNameReview, row observe.QueueRecord, style PathStyle, roots []string, candidates []wireCandidate) EpisodeNameReview {
	var video *wireCandidate
	for i := range candidates {
		candidate := &candidates[i]
		if !videoFile(candidate.Path) {
			continue
		}
		if video != nil {
			review.Reason = "Sonarr returned more than one video for this download. Review the files individually in Sonarr."
			return review
		}
		video = candidate
	}
	if video == nil {
		review.Reason = "Sonarr returned no video file for this download. Check its location and permissions in Sonarr."
		return review
	}

	actualPath := video.Path
	if style == WindowsPath {
		actualPath = strings.ReplaceAll(actualPath, "\\", "/")
	}
	end := strings.LastIndexByte(actualPath, '/') + 1
	parent, file := actualPath[:end], actualPath[end:]
	if !pathWithin(style, row.OutputPath, video.Path, true) ||
		(!samePath(style, row.OutputPath, parent) && !samePath(style, row.OutputPath, video.Path)) ||
		slices.ContainsFunc(roots, func(root string) bool { return pathWithin(style, root, video.Path, true) }) ||
		(video.DownloadID != nil && *video.DownloadID != "" && *video.DownloadID != row.DownloadID) {
		review.Reason = "Sonarr did not report a video directly in the tracked download folder."
		return review
	}
	folder := path.Base(parent)
	if !cleanText(folder, 512) || !cleanText(file, 512) {
		review.Reason = "Sonarr returned a folder or filename that could not be checked."
		return review
	}
	review.Folder, review.Filename = folder, file
	review.ArrRejection = len(video.Rejections) > 0
	if video.Size <= 0 {
		review.Reason = "Sonarr did not confirm that the video file has content."
		return review
	}
	if conflictsWithEpisode(*video, row.Target) {
		review.Reason = "Sonarr associated the video with different series or episode IDs."
		return review
	}
	expected := singleEpisode(row.Episode)
	for _, name := range []struct {
		label, value string
	}{{"folder name", folder}, {"filename", strings.TrimSuffix(file, path.Ext(file))}} {
		found := singleEpisode(name.value)
		switch {
		case !found.valid() || found.season != expected.season || found.episode != expected.episode:
			review.Reason = "The " + name.label + " needs to identify exactly " + row.Episode + "."
		case !slices.Equal(seriesWords(name.value[:found.start]), seriesWords(row.MediaTitle)):
			review.Reason = "The series in the " + name.label + " does not match " + row.MediaTitle + "."
		default:
			namedTitle, extra := episodeSuffix(name.value[found.end:], row.MediaTitle)
			if extra {
				review.Reason = "The " + name.label + " looks like a sample or extra."
			} else if len(namedTitle) > 0 {
				if !slices.Equal(namedTitle, nameWords(row.EpisodeTitle)) {
					review.Reason = "The episode title in the " + name.label + " could not be confirmed against Sonarr."
				} else {
					review.TitleInNames = true
				}
			}
		}
		if review.Reason != "" {
			return review
		}
	}
	review.Matched = true
	return review
}

func videoFile(filename string) bool {
	switch strings.ToLower(path.Ext(strings.ReplaceAll(filename, "\\", "/"))) {
	case ".mkv", ".mp4", ".m4v", ".avi", ".mov", ".mpg", ".mpeg", ".ts", ".m2ts", ".wmv", ".webm":
		return true
	default:
		return false
	}
}

func conflictsWithEpisode(candidate wireCandidate, target domain.TargetRef) bool {
	return (candidate.Series != nil && candidate.Series.ID > 0 && candidate.Series.ID != target.SeriesID) ||
		(candidate.SeriesID != nil && *candidate.SeriesID > 0 && *candidate.SeriesID != target.SeriesID) ||
		slices.ContainsFunc(candidate.Episodes, func(e wireEpisode) bool {
			return (e.ID > 0 && e.ID != target.ID) || (e.SeriesID != nil && *e.SeriesID > 0 && *e.SeriesID != target.SeriesID)
		}) ||
		slices.ContainsFunc(candidate.EpisodeIDs, func(id int64) bool { return id > 0 && id != target.ID })
}

var (
	episodeNumber = regexp.MustCompile(`(?i)\b(?:s([0-9]{1,3})e([0-9]{1,4})|([0-9]{1,3})x([0-9]{1,4}))\b`)
	episodeRange  = regexp.MustCompile(`(?i)^(?:[ .-]*e[0-9]|\s*[-~&+]\s*(?:e\s*)?[0-9]+\b|[ .-]+(?:and|to)[ .-]+[0-9]+\b)`)
	releaseTag    = regexp.MustCompile(`(?i)\b(?:480p|576p|720p|1080[pi]|2160p|4320p|4k|hdtv|web[ .-]?(?:dl|rip)|blu[ .-]?ray|b[dr]rip|dvdrip|x26[45]|h[ .-]?26[45]|hevc|av1|aac|repack|proper)\b`)
)

type episodeMarker struct {
	season, episode int
	start, end      int
}

func (e episodeMarker) valid() bool { return e.episode > 0 }

func singleEpisode(name string) episodeMarker {
	// Treat underscores as separators without changing the byte offsets.
	value := strings.ReplaceAll(name, "_", " ")
	markers := episodeNumber.FindAllStringSubmatchIndex(value, 2)
	if len(markers) != 1 {
		return episodeMarker{}
	}
	m := markers[0]
	if episodeRange.MatchString(value[m[1]:]) {
		return episodeMarker{}
	}
	if m[2] < 0 {
		m = []int{m[0], m[1], m[6], m[7], m[8], m[9]}
	}
	season, _ := strconv.Atoi(value[m[2]:m[3]])
	episode, _ := strconv.Atoi(value[m[4]:m[5]])
	return episodeMarker{season: season, episode: episode, start: m[0], end: m[1]}
}

func nameWords(name string) []string {
	return strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}

func seriesWords(name string) []string {
	words := slices.DeleteFunc(nameWords(name), func(word string) bool {
		return word == "a" || word == "an" || word == "the" || word == "of"
	})
	if len(words) > 1 && words[len(words)-1] == "series" {
		words = words[:len(words)-1]
	}
	return words
}

func episodeSuffix(suffix, series string) ([]string, bool) {
	for _, word := range nameWords(suffix) {
		if word == "sample" || word == "trailer" || word == "featurette" {
			return nil, true
		}
	}
	if marker := releaseTag.FindStringIndex(strings.ReplaceAll(suffix, "_", " ")); marker != nil {
		suffix = suffix[:marker[0]]
	}
	if slices.Equal(seriesWords(suffix), seriesWords(series)) {
		return nil, false
	}
	return nameWords(suffix), false
}
