package arradapter

import (
	"net"
	"net/http"
	"time"

	"github.com/rnodern/rescuarr/internal/arrwire"
	"github.com/rnodern/rescuarr/internal/domain"
)

// NewQualifiedSonarrV5Copy builds only the Sonarr v5 contract's singleton Copy
// route. Qualification itself and independent readback belong to the caller.
// RoundTrip does not follow redirects. Dispatch omits request replay metadata
// and the isolated transport does not reuse connections.
func NewQualifiedSonarrV5Copy(cfg Config, qualify Qualify, readback Readback, now func() time.Time) (*Adapter, error) {
	if cfg.Instance.Product != domain.ProductSonarr ||
		arrwire.ContractFor(cfg.Instance.Product, cfg.Instance.UpstreamVersion) != arrwire.ContractSonarrV5 {
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
