// No-op network diagnostic for the default build. Build with the `netdiag` tag
// to enable the raw-frame probe (see netdiag.go).

//go:build tamago && !netdiag

package net

import "src.kyanite.computer/aspeed-go/hal/ftgmac100"

func diagnose(*ftgmac100.Device) {}
