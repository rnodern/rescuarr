// Package arrwire encodes fixture-tested request shapes for pinned Arr versions.
// It has no transport or capability qualification.
package arrwire

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"

	"github.com/rnodern/rescuarr/internal/domain"
)

const (
	SonarrVersion             = "4.0.20.3014"
	SonarrIDOnlyImportVersion = "5.0.0.2941"
	RadarrVersion             = "6.4.4.10685"
)

var (
	ErrUnsupported = errors.New("unsupported Arr product or version")
	ErrBinding     = errors.New("prepared Arr action does not bind to the current operation")
	ErrNativeShape = errors.New("prepared Arr action does not fit the native request")
)

// Request contains API paths relative to the configured base and private wire
// values. Use the accessors at the transport boundary only.
type Request struct {
	method      string
	path        string
	rawQuery    string
	contentType string
	body        []byte
}

func (r Request) Method() string      { return r.method }
func (r Request) Path() string        { return r.path }
func (r Request) RawQuery() string    { return r.rawQuery }
func (r Request) ContentType() string { return r.contentType }
func (r Request) Body() []byte        { return bytes.Clone(r.body) }

func (Request) String() string     { return "arrwire.Request{redacted}" }
func (r Request) GoString() string { return r.String() }

// Encode checks prepared selectors against the operation and its revalidated
// instance and adapter revision. The caller supplies authoritative preflight,
// policy and qualification before handing a request to any future transport.
func Encode(instance domain.Instance, adapterRevision string, op domain.Operation, prepared domain.PreparedAction) (Request, error) {
	switch instance.Product {
	case domain.ProductSonarr:
		switch instance.UpstreamVersion {
		case SonarrVersion:
		case SonarrIDOnlyImportVersion:
			if op.Action != domain.ActionImport || len(op.Targets) != 1 {
				return Request{}, ErrUnsupported
			}
		default:
			return Request{}, ErrUnsupported
		}
	case domain.ProductRadarr:
		if instance.UpstreamVersion != RadarrVersion {
			return Request{}, ErrUnsupported
		}
	default:
		return Request{}, ErrUnsupported
	}

	if instance.Ref != op.Job.Instance || instance.Product != prepared.Product ||
		instance.UpstreamVersion != prepared.Version || instance.ConnectionRevision != prepared.ConnectionRevision ||
		adapterRevision != prepared.AdapterRevision || !prepared.Binds(op) {
		return Request{}, ErrBinding
	}
	for _, target := range op.Targets {
		if target.ID > math.MaxInt32 || target.SeriesID > math.MaxInt32 {
			return Request{}, ErrNativeShape
		}
	}

	switch op.Action {
	case domain.ActionImport:
		if instance.UpstreamVersion == SonarrIDOnlyImportVersion {
			file := prepared.Import.Files[0]
			if file.ReleaseType != "singleEpisode" && file.ReleaseType != "unknown" {
				return Request{}, ErrNativeShape
			}
		}
		return copyImport(instance.Product, instance.UpstreamVersion, prepared.Import)
	case domain.ActionReject:
		return queueReject(prepared.Reject)
	case domain.ActionSearch:
		if instance.Product == domain.ProductSonarr {
			return command(episodeSearchCommand{Name: "EpisodeSearch", EpisodeIDs: targetIDs(op.Targets)})
		}
		return command(movieSearchCommand{Name: "MoviesSearch", MovieIDs: targetIDs(op.Targets)})
	default:
		return Request{}, ErrBinding
	}
}

type episodeSearchCommand struct {
	Name       string  `json:"name"`
	EpisodeIDs []int32 `json:"episodeIds"`
}

type movieSearchCommand struct {
	Name     string  `json:"name"`
	MovieIDs []int32 `json:"movieIds"`
}

func targetIDs(targets []domain.TargetRef) []int32 {
	ids := make([]int32, len(targets))
	for i, target := range targets {
		ids[i] = int32(target.ID)
	}
	return ids
}

func queueReject(reject *domain.PreparedReject) (Request, error) {
	if reject.QueueID > math.MaxInt32 || reject.GrabHistoryID > math.MaxInt32 {
		return Request{}, ErrNativeShape
	}
	query := url.Values{
		"removeFromClient": {strconv.FormatBool(reject.RemoveFromClient)},
		"blocklist":        {"true"},
		"skipRedownload":   {"true"},
		"changeCategory":   {"false"},
	}
	return Request{method: http.MethodDelete, path: "/api/v3/queue/" + strconv.FormatInt(reject.QueueID, 10), rawQuery: query.Encode()}, nil
}

func command(body any) (Request, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return Request{}, ErrNativeShape
	}
	return Request{method: http.MethodPost, path: "/api/v3/command", contentType: "application/json", body: payload}, nil
}
