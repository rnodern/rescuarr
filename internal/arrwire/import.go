package arrwire

import (
	"math"

	"github.com/rnodern/rescuarr/internal/domain"
)

type copyCommand[File sonarrImportFile | radarrImportFile] struct {
	Name       string `json:"name"`
	ImportMode string `json:"importMode"`
	Files      []File `json:"files"`
}

type importFile struct {
	Path         string       `json:"path"`
	FolderName   string       `json:"folderName"`
	DownloadID   string       `json:"downloadId"`
	Quality      qualityModel `json:"quality"`
	Languages    []language   `json:"languages"`
	ReleaseGroup *string      `json:"releaseGroup,omitempty"`
	IndexerFlags int32        `json:"indexerFlags"`
}

type sonarrImportFile struct {
	importFile
	SeriesID    int32   `json:"seriesId"`
	EpisodeIDs  []int32 `json:"episodeIds"`
	ReleaseType string  `json:"releaseType"`
}

type radarrImportFile struct {
	importFile
	MovieID int32 `json:"movieId"`
}

type qualityModel struct {
	Quality  quality         `json:"quality"`
	Revision qualityRevision `json:"revision"`
}

type quality struct {
	ID         int32  `json:"id"`
	Name       string `json:"name"`
	Source     string `json:"source"`
	Resolution int32  `json:"resolution"`
	Modifier   string `json:"modifier,omitempty"`
}

type qualityRevision struct {
	Version  int32 `json:"version"`
	Real     int32 `json:"real"`
	IsRepack bool  `json:"isRepack"`
}

type language struct {
	ID   int32  `json:"id"`
	Name string `json:"name"`
}

func copyImport(product domain.Product, version string, prepared *domain.PreparedImport) (Request, error) {
	if product == domain.ProductSonarr {
		files := make([]sonarrImportFile, 0, len(prepared.Files))
		for _, selected := range prepared.Files {
			file, ok := nativeImportFile(product, version, prepared.DownloadID, selected)
			if !ok {
				return Request{}, ErrNativeShape
			}
			files = append(files, sonarrImportFile{
				importFile: file, SeriesID: int32(selected.Targets[0].SeriesID),
				EpisodeIDs: targetIDs(selected.Targets), ReleaseType: selected.ReleaseType,
			})
		}
		return command(copyCommand[sonarrImportFile]{Name: "ManualImport", ImportMode: "copy", Files: files})
	}

	// Binds requires exactly one movie and one eligible file for Radarr.
	selected := prepared.Files[0]
	file, ok := nativeImportFile(product, version, prepared.DownloadID, selected)
	if !ok {
		return Request{}, ErrNativeShape
	}
	return command(copyCommand[radarrImportFile]{
		Name: "ManualImport", ImportMode: "copy",
		Files: []radarrImportFile{{importFile: file, MovieID: int32(selected.Targets[0].ID)}},
	})
}

func nativeImportFile(product domain.Product, version, downloadID string, selected domain.PreparedImportFile) (importFile, bool) {
	if selected.QualityID > math.MaxInt32 || *selected.QualityResolution > math.MaxInt32 ||
		selected.Quality.Version > math.MaxInt32 || selected.Quality.Real > math.MaxInt32 ||
		!nativeImportEnums(product, version, selected) {
		return importFile{}, false
	}
	languages := make([]language, len(selected.Languages))
	for i, observed := range selected.Languages {
		if observed.ID > math.MaxInt32 {
			return importFile{}, false
		}
		languages[i] = language{ID: int32(observed.ID), Name: observed.Name}
	}

	// ManualImportResource and ManualImportFile use the same quality and
	// language objects and integer flags on every qualified contract.
	return importFile{
		Path: selected.Path, FolderName: selected.FolderName, DownloadID: downloadID,
		Quality: qualityModel{
			Quality: quality{
				ID: int32(selected.QualityID), Name: selected.QualityName, Source: selected.QualitySource,
				Resolution: int32(*selected.QualityResolution), Modifier: selected.QualityModifier,
			},
			Revision: qualityRevision{
				Version: int32(selected.Quality.Version), Real: int32(selected.Quality.Real), IsRepack: selected.Quality.IsRepack,
			},
		},
		Languages: languages, ReleaseGroup: selected.ReleaseGroup, IndexerFlags: int32(*selected.IndexerFlags),
	}, true
}

func nativeImportEnums(product domain.Product, version string, file domain.PreparedImportFile) bool {
	// Sonarr v5 adds the Subtitles flag. Older contracts retain their caps.
	if product == domain.ProductSonarr {
		limit := uint64(0xff)
		if ContractFor(product, version) == ContractSonarrV5 {
			limit = 0x1ff
		}
		if file.QualityModifier != "" || *file.IndexerFlags > limit {
			return false
		}
		switch file.QualitySource {
		case "television", "televisionRaw", "web", "webRip", "dvd", "bluray", "blurayRaw":
		default:
			return false
		}
		switch file.ReleaseType {
		case "unknown", "singleEpisode", "multiEpisode", "seasonPack":
			return true
		default:
			return false
		}
	}

	if file.ReleaseType != "" || *file.IndexerFlags > 0xfff {
		return false
	}
	switch file.QualitySource {
	case "cam", "telesync", "telecine", "workprint", "dvd", "tv", "webdl", "webrip", "bluray":
	default:
		return false
	}
	switch file.QualityModifier {
	case "none", "regional", "screener", "rawhd", "brdisk", "remux":
		return true
	default:
		return false
	}
}
