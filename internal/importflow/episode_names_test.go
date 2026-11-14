package importflow

import (
	"testing"

	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/observe"
)

func TestEpisodeFilenameReview(t *testing.T) {
	const release = "Captain.Wattle.Animated.S02E11.Captain.Wattle.Animated.720p.FIXTURE.WEB-DL.AAC2.0.x264-Lab"
	const titled = "Captain Wattle Animated_2x11_Signal Lost_720p"
	const wrongEpisode = "Captain.Wattle.Animated.S02E10.720p"
	row := observe.QueueRecord{
		DownloadID: "original", MediaTitle: "Captain Wattle: The Animated Series", Episode: "S02E11", EpisodeTitle: "Signal Lost",
		Target: domain.TargetRef{Kind: domain.TargetEpisode, ID: 41, SeriesID: 12},
	}
	review := domain.EpisodeNameReview{Series: row.MediaTitle, Episode: row.Episode, EpisodeTitle: row.EpisodeTitle}
	cases := []struct {
		name, folder, file string
		wantMatch, titled  bool
	}{
		{"shortened series name with repeated title", release, release, true, false},
		{"title and alternate numbering", titled, release, true, true},
		{"adjacent episode filename", release, wrongEpisode, false, false},
		{"adjacent episode folder", wrongEpisode, release, false, false},
		{"lookalike series", release, "Captain.Wattle.Live.Action.S02E11.720p", false, false},
		{"unnumbered episode", release, "Captain.Wattle.Animated.Signal.Lost.720p", false, false},
		{"another episode title", release, "Captain.Wattle.Animated.S02E11.Broken Antenna.720p", false, false},
		{"joined episodes", release, "Captain.Wattle.Animated.S02E11E12.720p", false, false},
		{"episode range", release, "Captain.Wattle.Animated.S02E11-12.720p", false, false},
		{"multiple episode markers", release, "Captain.Wattle.Animated.S02E11.S02E12.720p", false, false},
		{"sample", release, release + ".sample", false, false},
		{"resolution after dash", release, "Captain.Wattle.Animated.S02E11-720p", true, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			row.OutputPath = `\\NAS\downloads\` + test.folder
			filename := test.file + ".mkv"
			candidate := wireCandidate{Path: row.OutputPath + `\` + filename, Size: 4096, Rejections: []*wireRejection{{Type: "permanent"}}}
			got := checkEpisodeNames(review, row, WindowsPath, nil, []wireCandidate{candidate})
			if got.Matched != test.wantMatch || got.TitleInNames != test.titled ||
				got.Filename != filename || got.Folder != test.folder || !got.ArrRejection ||
				(!got.Matched && got.Reason == "") {
				t.Fatalf("unexpected evidence: %+v", got)
			}
		})
	}
}

func TestEpisodeFilenameReviewHoldsDifferentScopeOrIdentity(t *testing.T) {
	const release = "Series.S02E11.720p"
	row := observe.QueueRecord{OutputPath: "/downloads/" + release, DownloadID: "original", MediaTitle: "Series", Episode: "S02E11",
		Target: domain.TargetRef{Kind: domain.TargetEpisode, ID: 41, SeriesID: 12}}
	good := wireCandidate{Path: row.OutputPath + "/" + release + ".mkv", Size: 4096}
	other := "another"
	cases := []struct {
		name       string
		candidates []wireCandidate
		roots      []string
	}{
		{"no candidate", nil, nil},
		{"multiple videos", []wireCandidate{good, {Path: row.OutputPath + "/other.mkv", Size: 4096}}, nil},
		{"outside download", []wireCandidate{{Path: "/other/" + release + "/" + release + ".mkv", Size: 4096}}, nil},
		{"escaping path", []wireCandidate{{Path: row.OutputPath + "/../" + release + ".mkv", Size: 4096}}, nil},
		{"inside library root", []wireCandidate{good}, []string{"/downloads"}},
		{"another download", []wireCandidate{{Path: good.Path, Size: good.Size, DownloadID: &other}}, nil},
		{"conflicting Sonarr episode ID", []wireCandidate{{Path: good.Path, Size: good.Size, EpisodeIDs: []int64{42}}}, nil},
		{"empty file", []wireCandidate{{Path: good.Path}}, nil},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := checkEpisodeNames(domain.EpisodeNameReview{}, row, UnixPath, test.roots, test.candidates)
			if got.Matched || got.Reason == "" {
				t.Fatalf("unexpected match: %+v", got)
			}
		})
	}
}
