package observe

import (
	"context"
	"slices"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
)

const maxFileItems = 128

// LibraryFile is the exact file reached through a fresh target link. The
// optional metadata can be unknown even when the association is known.
type LibraryFile struct {
	ID                  int64
	RelativePath        string `json:"-"`
	Path                string `json:"-"`
	SizeBytes           *int64
	DateAdded           *time.Time
	Quality             *FileQuality
	Edition             string `json:"-"`
	LanguageIDs         []int32
	FormatIDs           []int32
	CustomFormatScore   *int32
	QualityCutoffNotMet *bool
}

type FileQuality struct {
	ID         int32
	Source     string
	Resolution int32
	Modifier   string
	Revision   domain.ImportQualityRevision
}

type fileQualityWire struct {
	Quality *struct {
		ID         *int32 `json:"id"`
		Source     string `json:"source"`
		Resolution *int32 `json:"resolution"`
		Modifier   string `json:"modifier"`
	} `json:"quality"`
	Revision *struct {
		Version  *int32 `json:"version"`
		Real     *int32 `json:"real"`
		IsRepack *bool  `json:"isRepack"`
	} `json:"revision"`
}

func (a *Arr) libraryFile(ctx context.Context, ref domain.TargetRef, id int64, season *int32) (LibraryFile, error) {
	var wire struct {
		ID                  int64            `json:"id"`
		SeriesID            int64            `json:"seriesId"`
		MovieID             int64            `json:"movieId"`
		SeasonNumber        *int32           `json:"seasonNumber"`
		RelativePath        string           `json:"relativePath"`
		Path                string           `json:"path"`
		Size                *int64           `json:"size"`
		DateAdded           *time.Time       `json:"dateAdded"`
		Quality             fileQualityWire  `json:"quality"`
		Edition             string           `json:"edition"`
		Languages           []resourceIDWire `json:"languages"`
		Formats             []resourceIDWire `json:"customFormats"`
		CustomFormatScore   *int32           `json:"customFormatScore"`
		QualityCutoffNotMet *bool            `json:"qualityCutoffNotMet"`
	}
	resource := "episodefile"
	if ref.Kind == domain.TargetMovie {
		resource = "moviefile"
	}
	if err := a.getTargetResource(ctx, resource, id, &wire); err != nil {
		return LibraryFile{}, err
	}
	if wire.ID != id || (ref.Kind == domain.TargetEpisode && (wire.SeriesID != ref.SeriesID || wire.MovieID != 0 ||
		(season != nil && wire.SeasonNumber != nil && *season != *wire.SeasonNumber))) ||
		(ref.Kind == domain.TargetMovie && (wire.MovieID != ref.ID || wire.SeriesID != 0)) {
		return LibraryFile{}, ErrResponse
	}
	out := LibraryFile{
		ID: id, RelativePath: exactToken(wire.RelativePath, 4096), Path: exactToken(wire.Path, 4096), SizeBytes: nonnegative(wire.Size),
		Quality: wire.Quality.project(a.reader.product), LanguageIDs: fileMetadataIDs(wire.Languages, 0),
		FormatIDs: fileMetadataIDs(wire.Formats, 1), CustomFormatScore: wire.CustomFormatScore, QualityCutoffNotMet: wire.QualityCutoffNotMet,
	}
	if wire.DateAdded != nil && !wire.DateAdded.IsZero() {
		out.DateAdded = new(wire.DateAdded.UTC())
	}
	if ref.Kind == domain.TargetMovie {
		out.Edition = exactToken(wire.Edition, 256)
	}
	return out, nil
}

func (w fileQualityWire) project(product domain.Product) *FileQuality {
	q, rev := w.Quality, w.Revision
	if q == nil || q.ID == nil || *q.ID <= 0 || q.Resolution == nil || *q.Resolution < 0 ||
		rev == nil || rev.Version == nil || *rev.Version <= 0 || rev.Real == nil || *rev.Real < 0 || rev.IsRepack == nil {
		return nil
	}
	// These values are native to each pinned product. An unrecognised enum
	// leaves the quality unknown rather than giving it local meaning.
	if product == domain.ProductSonarr {
		switch q.Source {
		case "television", "televisionRaw", "web", "webRip", "dvd", "bluray", "blurayRaw":
		default:
			return nil
		}
		if q.Modifier != "" {
			return nil
		}
	} else {
		switch q.Source {
		case "cam", "telesync", "telecine", "workprint", "dvd", "tv", "webdl", "webrip", "bluray":
		default:
			return nil
		}
		switch q.Modifier {
		case "none", "regional", "screener", "rawhd", "brdisk", "remux":
		default:
			return nil
		}
	}
	return &FileQuality{ID: *q.ID, Source: q.Source, Resolution: *q.Resolution, Modifier: q.Modifier,
		Revision: domain.ImportQualityRevision{
			Version: int64(*rev.Version), Real: int64(*rev.Real), IsRepack: *rev.IsRepack, Observed: true,
		}}
}

func fileMetadataIDs(items []resourceIDWire, minimum int32) []int32 {
	if items == nil || len(items) > maxFileItems {
		return nil
	}
	result := make([]int32, 0, len(items))
	seen := make(map[int32]bool, len(items))
	for _, item := range items {
		if item.ID == nil || *item.ID < minimum || seen[*item.ID] {
			return nil
		}
		id := *item.ID
		seen[id] = true
		result = append(result, id)
	}
	slices.Sort(result)
	return result
}
