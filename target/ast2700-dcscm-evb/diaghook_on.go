// Flash-diagnostic entry hook (enabled). With the `flashdiag` build tag the
// payload runs the read-only FMC/SPI-NOR probe suite instead of booting the
// normal management plane. diag.Run never returns.

//go:build tamago && flashdiag

package main

import "src.kyanite.computer/cairn/target/ast2700-dcscm-evb/diag"

func maybeRunFlashDiag() { diag.Run() }
