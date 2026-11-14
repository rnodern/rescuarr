package arradapter

import (
	"net"
	"net/http"
	"time"

	"github.com/rnodern/rescuarr/internal/arrwire"
	"github.com/rnodern/rescuarr/internal/domain"
)

// NewQualifiedSonarrV5Copy builds only the exact Sonarr v5 singleton Copy
// route. Qualification itself and independent readback belong to the caller.
// RoundTrip does not follow redirects. Dispatch omits request replay metadata
// and the isolated transport does not reuse connections.
func NewQualifiedSonarrV5Copy(cfg Config, qualify Qualify, readback Readback, now func() time.Time) (*Adapter, error) {
	if cfg.Instance.Product != domain.ProductSonarr || cfg.Instance.UpstreamVersion != arrwire.SonarrIDOnlyImportVersion {
		return nil, ErrConfiguration
	}
	transport := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		TLSHandshakeTimeout: 3 * time.Second,
		DisableKeepAlives:   true, DisableCompression: true,
		MaxResponseHeaderBytes: 32 << 10,
	}
	return newAdapter(cfg, transport.RoundTrip, qualify, readback, now, RouteCopyImport)
}
