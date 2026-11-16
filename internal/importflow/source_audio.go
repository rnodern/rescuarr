package importflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
)

const (
	sourceAudioTimeout  = 5 * time.Second
	maxAudioProbeOutput = 16 << 10
)

// SourceAudioProbe reads track tags from the selected import file through an
// operator-configured mapping of the Sonarr download directory.
type SourceAudioProbe struct {
	instance   domain.InstanceID
	sonarrRoot string
	localRoot  string
	ffprobe    string
	slot       chan struct{}
}

func NewSourceAudioProbe(instance domain.InstanceID, sonarrRoot, localRoot string) (*SourceAudioProbe, error) {
	_, windows := parsePath(WindowsPath, sonarrRoot)
	_, unix := parsePath(UnixPath, sonarrRoot)
	if instance == "" || (!windows && !unix) || !filepath.IsAbs(localRoot) {
		return nil, errors.New("download audio requires a Sonarr connection and two absolute download paths")
	}
	probe, err := exec.LookPath("ffprobe")
	if err != nil {
		return nil, errors.New("download audio requires ffprobe on PATH")
	}
	return &SourceAudioProbe{instance: instance, sonarrRoot: sonarrRoot, localRoot: filepath.Clean(localRoot),
		ffprobe: probe, slot: make(chan struct{}, 1)}, nil
}

// AudioTracks returns known languages and whether every audio stream was
// tagged. An inaccessible, changed or unrecognised file supplies no evidence.
func (p *SourceAudioProbe) AudioTracks(ctx context.Context, style PathStyle, source SonarrV5Source) ([]string, bool) {
	if p == nil || source.Job.Instance.ID != p.instance || source.Size <= 0 ||
		source.SelectionDigest == ([32]byte{}) || !pathWithin(style, source.OutputPath, source.Path, true) {
		return nil, false
	}
	relative, ok := audioSourceRelativePath(style, p.sonarrRoot, source.Path)
	if !ok {
		return nil, false
	}
	select {
	case p.slot <- struct{}{}:
		defer func() { <-p.slot }()
	default:
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, sourceAudioTimeout)
	defer cancel()
	root, err := os.OpenRoot(p.localRoot)
	if err != nil {
		return nil, false
	}
	defer root.Close()
	before, err := root.Stat(relative)
	if err != nil || !before.Mode().IsRegular() || before.Size() != source.Size {
		return nil, false
	}
	file, err := root.Open(relative)
	if err != nil {
		return nil, false
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !sameAudioSource(before, opened) {
		return nil, false
	}

	// The child sees only an already opened, read-only input. The protocol and
	// format allowlists keep the probe on a single media stream.
	command := exec.CommandContext(ctx, p.ffprobe, "-v", "error", "-probesize", "4194304",
		"-analyzeduration", "2000000", "-protocol_whitelist", "pipe",
		"-format_whitelist", "matroska,webm,mov,mp4,m4a,3gp,3g2,mj2",
		"-select_streams", "a", "-show_entries", "stream=index:stream_tags=language", "-of", "json", "pipe:0")
	var output, diagnostics audioProbeOutput
	command.Stdin, command.Stdout, command.Stderr = file, &output, &diagnostics
	command.WaitDelay = time.Second
	if err := command.Run(); err != nil || diagnostics.content.Len() != 0 || ctx.Err() != nil {
		return nil, false
	}
	after, err := root.Stat(relative)
	if err != nil || !sameAudioSource(before, after) {
		return nil, false
	}
	return audioStreamLanguages(output.content.Bytes())
}

func sameAudioSource(before, after os.FileInfo) bool {
	return after.Mode().IsRegular() && os.SameFile(before, after) && before.Size() == after.Size() &&
		before.ModTime().Equal(after.ModTime())
}

func audioSourceRelativePath(style PathStyle, root, path string) (string, bool) {
	if !pathWithin(style, root, path, false) {
		return "", false
	}
	base, _ := parsePath(style, root)
	file, _ := parsePath(style, path)
	if style == WindowsPath {
		path = strings.ReplaceAll(path, "\\", "/")
	}
	// Windows matching is case-insensitive. Keep the reported casing when
	// opening the file through a case-sensitive mount.
	parts := strings.Split(path, "/")
	suffix := parts[len(parts)-(len(file.parts)-len(base.parts)):]
	local, err := filepath.Localize(strings.Join(suffix, "/"))
	return local, err == nil
}

type audioProbeOutput struct{ content bytes.Buffer }

func (p *audioProbeOutput) Write(data []byte) (int, error) {
	if len(data) > maxAudioProbeOutput-p.content.Len() {
		return 0, errors.New("audio probe output exceeded limit")
	}
	return p.content.Write(data)
}

func audioStreamLanguages(data []byte) ([]string, bool) {
	var result struct {
		Streams []struct {
			Index *int `json:"index"`
			Tags  struct {
				Language string `json:"language"`
			} `json:"tags"`
		} `json:"streams"`
	}
	if json.Unmarshal(data, &result) != nil || len(result.Streams) == 0 || len(result.Streams) > 32 {
		return nil, false
	}
	var languages []string
	seen := make(map[int]bool, len(result.Streams))
	complete := true
	for _, stream := range result.Streams {
		if stream.Index == nil || *stream.Index < 0 || seen[*stream.Index] {
			return nil, false
		}
		seen[*stream.Index] = true
		code := domain.AudioLanguageCode(stream.Tags.Language)
		if code == "" {
			complete = false
			continue
		}
		languages = append(languages, code)
	}
	return languages, complete
}
