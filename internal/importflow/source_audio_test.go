package importflow

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/rnodern/rescuarr/internal/domain"
)

func TestAudioSourceRelativePath(t *testing.T) {
	for _, tc := range []struct {
		style PathStyle
		root  string
		file  string
		want  string
	}{
		{WindowsPath, `C:\Downloads`, `c:\downloads\Show\Case Sensitive.mkv`, "Show/Case Sensitive.mkv"},
		{WindowsPath, `\\host\share\Complete`, `//Host/Share/complete/Show/File.mkv`, "Show/File.mkv"},
		{WindowsPath, `C:\Downloads`, `C:\Downloads-other\File.mkv`, ""},
		{WindowsPath, `C:\Downloads`, `C:\Downloads\..\File.mkv`, ""},
		{WindowsPath, `C:\Downloads`, `C:\Downloads\File.mkv:stream`, ""},
		{UnixPath, "/downloads", "/downloads/Show/File.mkv", "Show/File.mkv"},
		{UnixPath, "/downloads", "/Downloads/Show/File.mkv", ""},
		{UnixPath, "/downloads", "/downloads", ""},
	} {
		got, ok := audioSourceRelativePath(tc.style, tc.root, tc.file)
		if filepath.ToSlash(got) != tc.want || ok != (tc.want != "") {
			t.Errorf("%q under %q resolved to %q, %t", tc.file, tc.root, got, ok)
		}
	}
}

func TestAudioStreamLanguages(t *testing.T) {
	for _, tc := range []struct {
		response string
		tracks   []string
		complete bool
	}{
		{`{"streams":[{"index":1,"tags":{"language":"eng"}},{"index":2,"tags":{"language":"heb"}}]}`, []string{"en", "he"}, true},
		{`{"streams":[{"index":1,"tags":{"language":"eng"}},{"index":2}]}`, []string{"en"}, false},
		{`{"streams":[{"index":1,"tags":{"language":"und"}},{"index":2,"tags":{"language":"mul"}}]}`, nil, false},
		{`{"streams":[{"index":1,"tags":{"language":"eng"}},{"index":1}]}`, nil, false},
		{`{"streams":[]}`, nil, false},
	} {
		got, complete := audioStreamLanguages([]byte(tc.response))
		if !slices.Equal(got, tc.tracks) || complete != tc.complete {
			t.Errorf("%s: got %v, %t", tc.response, got, complete)
		}
	}
}

func TestSourceAudioProbeReadsTheBoundFile(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is unavailable")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe is unavailable")
	}

	root := t.TempDir()
	exact := filepath.Join(root, "Selected.mkv")
	fixture := exec.CommandContext(t.Context(), ffmpeg, "-v", "error", "-f", "lavfi", "-i",
		"anullsrc=r=8000:cl=mono", "-t", "0.10", "-c:a", "pcm_s16le",
		"-metadata:s:a:0", "language=eng", exact)
	if output, err := fixture.CombinedOutput(); err != nil {
		t.Fatalf("generate synthetic file: %v: %s", err, output)
	}
	info, err := os.Stat(exact)
	if err != nil {
		t.Fatal(err)
	}

	probe, err := NewSourceAudioProbe("fixture", `C:\Downloads`, root)
	if err != nil {
		t.Fatal(err)
	}
	source := SonarrV5Source{
		Job:        domain.JobRef{Instance: domain.InstanceRef{ID: "fixture", Epoch: 1}},
		OutputPath: `C:\Downloads`, Path: `C:\Downloads\Selected.mkv`, Size: info.Size(), SelectionDigest: [32]byte{1},
	}
	got, complete := probe.AudioTracks(t.Context(), WindowsPath, source)
	if !complete || !slices.Equal(got, []string{"en"}) {
		t.Fatalf("selected media tags: %v, %t", got, complete)
	}
	wrongSize := source
	wrongSize.Size++
	wrongConnection := source
	wrongConnection.Job.Instance.ID = "other"
	for _, mismatch := range []SonarrV5Source{wrongSize, wrongConnection} {
		if got, complete := probe.AudioTracks(t.Context(), WindowsPath, mismatch); len(got) != 0 || complete {
			t.Fatalf("unexpected evidence for mismatched source: %v, %t", got, complete)
		}
	}

	outside := filepath.Join(t.TempDir(), "Copied.mkv")
	data, err := os.ReadFile(exact)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "Outside.mkv")); err == nil {
		source.Path = `C:\Downloads\Outside.mkv`
		if got, complete := probe.AudioTracks(t.Context(), WindowsPath, source); len(got) != 0 || complete {
			t.Fatalf("escaped the permitted download directory: %v, %t", got, complete)
		}
	}
}
