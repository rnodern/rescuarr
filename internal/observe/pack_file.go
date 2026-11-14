package observe

import (
	"context"
	"math"
	"strings"

	"github.com/rnodern/rescuarr/internal/domain"
)

type PackEpisodeFileState string

const (
	PackFileMissing           PackEpisodeFileState = "missing"
	PackFileLinked            PackEpisodeFileState = "linked_to_pack"
	PackFileDifferentDownload PackEpisodeFileState = "different_download"
	PackFileUnverified        PackEpisodeFileState = "unverified"
)

type PackEpisodeFile struct {
	State                  PackEpisodeFileState
	FileID                 int64 `json:"-"`
	ImportHistoryID        int64 `json:"-"`
	Filename               string
	Languages              []domain.ImportLanguage
	AudioLanguages         []string
	AudioLanguagesComplete bool
}

// SonarrPackEpisodeFile correlates the currently linked file with the native
// import event for this target and the exact download ID. The caller must use
// a fresh queue and grab history to establish which release the ID represents.
func (a *Arr) SonarrPackEpisodeFile(ctx context.Context, target domain.TargetRef, downloadID string) (PackEpisodeFile, error) {
	unknown := PackEpisodeFile{State: PackFileUnverified}
	if a.reader.product != domain.ProductSonarr || target.Kind != domain.TargetEpisode ||
		!target.Valid() || target.Instance != a.reader.instance || target.ID > math.MaxInt32 || target.SeriesID > math.MaxInt32 ||
		downloadID == "" || exactToken(downloadID, 256) != downloadID {
		return unknown, ErrConfiguration
	}
	if a.upstreamVersion != sonarrV5Needs {
		return unknown, ErrTargetVersion
	}
	ctx, cancel := context.WithTimeout(ctx, scanTimeout)
	defer cancel()
	fileID, err := a.EpisodeFileLink(ctx, target)
	if err != nil {
		return unknown, err
	}
	if fileID == 0 {
		return PackEpisodeFile{State: PackFileMissing}, nil
	}
	records, err := a.EpisodeImportHistory(ctx, target)
	if err != nil {
		return unknown, err
	}
	var imported HistoryRecord
	for _, record := range records {
		if record.FileID != fileID {
			continue
		}
		if imported.ID != 0 || record.DownloadID == "" {
			return unknown, nil
		}
		imported = record
	}
	if imported.ID == 0 {
		return unknown, nil
	}
	if imported.DownloadID != downloadID {
		return PackEpisodeFile{State: PackFileDifferentDownload}, nil
	}
	file, err := a.libraryFile(ctx, target, fileID, nil)
	if err != nil {
		return unknown, err
	}
	currentID, err := a.EpisodeFileLink(ctx, target)
	if err != nil {
		return unknown, err
	}
	if currentID != fileID {
		return unknown, ErrPartial
	}
	filename := packFileName(file.RelativePath)
	if filename == "" {
		filename = packFileName(file.Path)
	}
	return PackEpisodeFile{State: PackFileLinked, FileID: fileID, ImportHistoryID: imported.ID,
		Filename: filename, Languages: file.languages, AudioLanguages: file.audioLanguages,
		AudioLanguagesComplete: file.audioComplete}, nil
}

func packFileName(value string) string {
	// Sonarr's filesystem can use different separators than Rescuarr's host.
	filename := exactToken(value[strings.LastIndexAny(value, `/\`)+1:], 512)
	if filename == "." || filename == ".." || strings.HasSuffix(filename, ":") {
		return ""
	}
	return filename
}
