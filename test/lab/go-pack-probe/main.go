// The isolated labs invoke this probe against their disposable Sonarr.
// Production startup does not import this command or its fixture qualification.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/rnodern/rescuarr/internal/arradapter"
	"github.com/rnodern/rescuarr/internal/arrwire"
	"github.com/rnodern/rescuarr/internal/domain"
)

type input struct {
	Operation  string                    `json:"operation"`
	APIKey     string                    `json:"apiKey"`
	Scope      arradapter.EmptyPackScope `json:"scope"`
	SentAt     time.Time                 `json:"sentAt"`
	EpisodeIDs []int64                   `json:"episodeIds"`
}

type output struct {
	OK        bool       `json:"ok"`
	Reason    string     `json:"reason,omitempty"`
	SentAt    *time.Time `json:"sentAt,omitempty"`
	Verified  bool       `json:"verified,omitempty"`
	CommandID int64      `json:"commandId,omitempty"`
}

func main() {
	decoder := json.NewDecoder(io.LimitReader(os.Stdin, 1<<15))
	decoder.DisallowUnknownFields()
	var in input
	if len(os.Args) != 1 || decoder.Decode(&in) != nil || decoder.Decode(new(json.RawMessage)) != io.EOF {
		_ = json.NewEncoder(os.Stdout).Encode(output{Reason: "invalidInput"})
		os.Exit(1)
	}
	proof := run(in)
	_ = json.NewEncoder(os.Stdout).Encode(proof)
	if !proof.OK {
		os.Exit(1)
	}
}

func run(in input) output {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	address := "sonarr:8989"
	if runtime.GOOS == "windows" {
		address = "127.0.0.1:18989"
	}
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	transport := &http.Transport{
		DisableKeepAlives:  true,
		DisableCompression: true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr != address {
				return nil, arradapter.ErrConfiguration
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}
	defer transport.CloseIdleConnections()

	adapter, err := arradapter.NewEmptyPackAdapter(arradapter.Config{
		Instance: domain.Instance{
			Ref:                domain.InstanceRef{ID: "sealed-lab-sonarr", Epoch: 1},
			Product:            domain.ProductSonarr,
			BaseURL:            "http://" + address,
			UpstreamVersion:    arrwire.SonarrIDOnlyImportVersion,
			ConnectionRevision: 1,
		},
		AdapterRevision:     "sealed-lab-empty-pack",
		SettingsFingerprint: sha256.Sum256([]byte("disposable Sonarr and SABnzbd lab storage")),
		APIKey:              in.APIKey,
	}, transport.RoundTrip, labQualification, time.Now)
	if err != nil {
		return output{Reason: "invalidLabConnection"}
	}

	switch in.Operation {
	case "reject":
		if err := adapter.Preflight(ctx, in.Scope); err != nil {
			return output{Reason: "preflight:" + category(err)}
		}
		sentAt := time.Now().UTC()
		if err := adapter.Reject(ctx, in.Scope); err != nil {
			return output{Reason: "reject:" + category(err)}
		}
		return output{OK: true, SentAt: &sentAt}
	case "verify":
		verified, err := adapter.VerifyRejected(ctx, in.Scope, in.SentAt)
		if err != nil {
			return output{Reason: "readback:" + category(err)}
		}
		return output{OK: true, Verified: verified}
	case "search":
		id, err := adapter.Search(ctx, in.Scope, in.EpisodeIDs)
		if err != nil {
			return output{Reason: "search:" + category(err)}
		}
		return output{OK: true, CommandID: id}
	default:
		return output{Reason: "invalidOperation"}
	}
}

func labQualification(_ context.Context, binding arradapter.Binding) (arradapter.Qualification, error) {
	if binding.Route != arradapter.RouteEmptyPackReject && binding.Route != arradapter.RouteEmptyPackSearch {
		return arradapter.Qualification{}, arradapter.ErrQualification
	}
	now := time.Now().UTC()
	return arradapter.Qualification{
		Binding: binding, Verified: true, Evidence: "disposable-upstream-lab-test",
		ObservedAt: now.Add(-time.Second), ExpiresAt: now.Add(time.Minute),
	}, nil
}

func category(err error) string {
	switch {
	case errors.Is(err, arradapter.ErrQualification):
		return "qualificationUnavailable"
	case errors.Is(err, arradapter.ErrPrepared):
		return "invalidScope"
	case errors.Is(err, arradapter.ErrReadback):
		return "unverifiedReadback"
	case errors.Is(err, arradapter.ErrUncertain):
		return "uncertainDispatch"
	default:
		return "failed"
	}
}
