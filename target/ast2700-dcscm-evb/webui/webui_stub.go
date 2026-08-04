// Default build (no facetui tag): the facet web UI is not compiled in. This
// stub provides the same API as webui.go but imports neither core/webui nor
// net/http, so the firmware image stays byte-identical to a build without the
// web UI at all — keeping the CA35 entry offset stable so the existing FMC
// boots. Build with `-tags facetui` (which the Dagger UI build sets) to include
// and serve the SPA.
//
// This package is only compiled for GOOS=tamago.

//go:build tamago && !facetui

package webui

import (
	"context"

	"src.kyanite.computer/core/cfgstore"
)

// Options mirrors the facetui build's Options so target code compiles either way.
type Options struct {
	Store *cfgstore.Store
	Hosts []string
}

// Server is an inert stub in the default build.
type Server struct{}

// Bundled reports false: no SPA (and no web server) in this build.
func Bundled() bool { return false }

// New returns an inert server; the target guards on Bundled() before use.
func New(Options) (*Server, error) { return &Server{}, nil }

// HTTPS blocks until ctx is cancelled without serving (never scheduled by the
// target, which checks Bundled() first).
func (*Server) HTTPS(ctx context.Context) error { <-ctx.Done(); return nil }

// Redirect blocks until ctx is cancelled without serving.
func (*Server) Redirect(ctx context.Context) error { <-ctx.Done(); return nil }
