// Default build: the facet SPA is not bundled. assets() is nil, so the :443
// server runs (for the management/Redfish API) but has no UI resource and
// unknown paths 404. Build with the `facetui` tag (see assets_facetui.go) to
// embed the UI bundle.

//go:build tamago && !facetui

package webui

import "io/fs"

func assets() fs.FS { return nil }
