// facetui build: embed the facet single-page app. The `build/` directory holds
// facet's adapter-static output and is populated by the Dagger UI build step
// (which clones/builds the facet repo) before compiling with `-tags facetui`.
// It is git-ignored and never committed into the firmware source.

//go:build tamago && facetui

package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:build
var buildFS embed.FS

func assets() fs.FS {
	sub, err := fs.Sub(buildFS, "build")
	if err != nil {
		return nil
	}
	return sub
}
