// Flash-diagnostic entry hook (disabled). Without the `flashdiag` build tag
// this is a no-op and the payload boots normally.

//go:build tamago && !flashdiag

package main

func maybeRunFlashDiag() {}
