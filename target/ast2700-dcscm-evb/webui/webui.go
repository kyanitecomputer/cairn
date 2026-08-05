// Package webui wires the shared core/webui HTTPS server onto the AST2700
// DC-SCM target: it serves the facet single-page app over TLS on :443 (a
// self-signed certificate by default, persisted in the config store, or an
// operator-uploaded pair) and runs an HTTP→HTTPS redirect on :80.
//
// The facet assets are optional and injected at build time. Build with the
// `facetui` tag — the Dagger UI build populates the embedded `build/` directory
// from the facet repo — to bundle the SPA. Without the tag, assets() is nil and
// the :443 server still runs (for future API mounts) but has no UI resource to
// serve, so unknown paths 404. This mirrors vein's fs.FS injection: the SPA
// bytes are never committed into the firmware source.
//
// # Build gating
//
// The web server itself (core/webui, net/http + crypto/tls) is always compiled
// in — it will also host the Redfish/management API later, not only the SPA.
// Only the facet asset bundle is gated by the `facetui` tag: with it, the
// Dagger UI build populates the embedded build/ directory and assets() serves
// the SPA; without it assets() is nil and the UI routes 404 while the server
// still runs. The BootMCU now reads the CA35 entry offset from the image header
// (imgtools a35-header), so growing the payload no longer needs an FMC constant
// bump.
//
// This package is only compiled for GOOS=tamago.

//go:build tamago

package webui

import (
	"context"
	"crypto/tls"

	"src.kyanite.computer/core/cfgstore"
	corewebui "src.kyanite.computer/core/webui"
)

// httpsPort / redirectPort are the well-known management-UI ports.
const (
	httpsPort    uint16 = 443
	redirectPort uint16 = 80
)

// Options carries what the HTTPS UI server needs from the target.
type Options struct {
	// Store persists the TLS certificate/key (self-signed or uploaded).
	Store *cfgstore.Store
	// Hosts are the certificate SANs (hostname and/or management IP).
	Hosts []string
}

// Bundled reports whether the facet SPA was embedded in this build (facetui
// tag). The target uses it for status reporting; the server serves correctly
// either way.
func Bundled() bool { return assets() != nil }

// Server serves the facet SPA over HTTPS and redirects plain HTTP to it.
type Server struct {
	cert tls.Certificate
}

// New builds the UI server, materialising the TLS certificate once (generating
// and persisting a self-signed one on first boot when none is stored).
func New(opt Options) (*Server, error) {
	cert, err := corewebui.Certificate(opt.Store, opt.Hosts)
	if err != nil {
		return nil, err
	}
	return &Server{cert: cert}, nil
}

// HTTPS is a supervised service that serves the embedded facet SPA over TLS on
// :443 until ctx is cancelled.
func (s *Server) HTTPS(ctx context.Context) error {
	return corewebui.ListenAndServeTLS(ctx, httpsPort, corewebui.SPAHandler(assets()), s.cert)
}

// Redirect is a supervised service that permanently redirects plain HTTP on :80
// to the HTTPS origin until ctx is cancelled.
func (s *Server) Redirect(ctx context.Context) error {
	return corewebui.ListenAndServe(ctx, redirectPort, corewebui.RedirectHandler())
}
