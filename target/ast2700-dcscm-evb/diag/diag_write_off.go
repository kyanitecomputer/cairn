// Write/erase validation disabled. The default diagnostic is strictly
// read-only. Build with an extra `flashdiagwrite` tag to run the destructive
// end-to-end write path (erase + program on a scratch sector).

//go:build tamago && !flashdiagwrite

package diag

func writeProbe() {
	p("[WRITE] destructive write/erase validation: DISABLED (read-only build)")
	p("        rebuild with -tags ...,flashdiag,flashdiagwrite to run it")
}
