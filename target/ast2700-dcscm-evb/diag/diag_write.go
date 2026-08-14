// Destructive write/erase validation (opt-in). Exercises the exact command
// sequences the real driver will use — WREN → WEL check → 4-byte sector erase →
// readback (expect 0xFF) → 4-byte page program → readback (expect pattern) —
// with bounded WIP polling. This proves the write path end-to-end on silicon.
//
// It operates ONLY on a scratch sector well above the ~24 MB boot image (63 MiB
// on the 64 MiB Macronix), using dedicated 4-byte opcodes so it needs no EN4B
// state and validates >16 MB addressing. It is destructive to that one sector
// only; the board is externally reflashable. Never compiled into the default
// read-only image.

//go:build tamago && flashdiagwrite

package diag

import "src.kyanite.computer/aspeed-go/reg"

const (
	scratchOff = 0x03F0_0000 // 63 MiB — above the boot image, within the 64 MiB chip

	opWREN   = 0x06
	opRDSR   = 0x05
	opPP4B   = 0x12 // page program, 4-byte address
	opSE4B   = 0x21 // sector erase (4 KiB), 4-byte address
	opREAD4B = 0x13

	srWIP = 1 << 0
	srWEL = 1 << 1

	erasePollMax   = 8_000_000
	programPollMax = 2_000_000
)

// userTxn runs one full user-mode transaction with the confirmed-correct
// mechanism: CE0 write-enable set, MISC cleared, read-back flush, read-only
// receive loop. Sends op + addrLen address bytes (MSB-first) + dataOut, then
// reads nIn bytes.
func userTxn(op byte, addr uint32, addrLen int, dataOut []byte, nIn int) []byte {
	savedCE := r32(rCE0Ctrl)
	savedMisc := r32(rMisc)
	savedCfg := r32(rFlashConfig)
	w32(rFlashConfig, savedCfg|ce0WriteEnable)
	w32(rMisc, 0)
	cu := (savedCE &^ (ioModeMask | cmdModeMask)) | cmdModeUser
	w32(rCE0Ctrl, cu|ceStop)
	w32(rCE0Ctrl, cu&^ceStop)
	_ = r32(rCE0Ctrl)

	reg.Write8(fmcWin, op)
	for i := 0; i < addrLen; i++ {
		reg.Write8(fmcWin, byte(addr>>uint(8*(addrLen-1-i))))
	}
	for _, b := range dataOut {
		reg.Write8(fmcWin, b)
	}
	out := make([]byte, nIn)
	for i := range out {
		out[i] = reg.Read8(fmcWin)
	}

	w32(rCE0Ctrl, cu|ceStop)
	w32(rCE0Ctrl, savedCE)
	w32(rMisc, savedMisc)
	w32(rFlashConfig, savedCfg)
	return out
}

func rdsr() byte { return userTxn(opRDSR, 0, 0, nil, 1)[0] }

// wren issues WREN and returns whether the WEL bit latched.
func wren() bool {
	userTxn(opWREN, 0, 0, nil, 0)
	return rdsr()&srWEL != 0
}

// waitWIP polls RDSR until WIP clears or the bound is hit.
func waitWIP(maxIter int) (done bool, last byte) {
	for i := 0; i < maxIter; i++ {
		last = rdsr()
		if last&srWIP == 0 {
			return true, last
		}
	}
	return false, last
}

// readWin reads n bytes from the auto-read window at off (uses the controller's
// configured auto-read, which is 4-byte on this board).
func readWin(off uint32, n int) []byte {
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		out[i] = reg.Read8(fmcWin + uintptr(off) + uintptr(i))
	}
	return out
}

func allEq(b []byte, v byte) bool {
	for _, x := range b {
		if x != v {
			return false
		}
	}
	return true
}

func writeProbe() {
	const n = 32
	p("[WRITE] destructive validation at scratch %#x (4-byte opcodes, 4 KiB sector):", scratchOff)

	// Baseline.
	winBefore := readWin(scratchOff, n)
	cmdBefore := userTxn(opREAD4B, scratchOff, 4, nil, n)
	p("  before  window: %s", hex(winBefore))
	p("  before  cmd0x13: %s (window==cmd: %v)", hex(cmdBefore), eq(winBefore, cmdBefore))

	// Erase.
	if !wren() {
		p("  WREN: WEL did NOT latch — aborting (status=%#02x)", rdsr())
		return
	}
	p("  WREN: WEL latched")
	userTxn(opSE4B, scratchOff, 4, nil, 0)
	ok, st := waitWIP(erasePollMax)
	p("  sector erase 0x21: WIP-clear=%v status=%#02x", ok, st)
	if !ok {
		p("  => ERASE TIMED OUT")
		return
	}
	erased := readWin(scratchOff, n)
	p("  after erase window: %s (all 0xFF: %v)", hex(erased), allEq(erased, 0xFF))

	// Program a recognizable pattern (256-byte page).
	pattern := make([]byte, 256)
	for i := range pattern {
		pattern[i] = byte(i)
	}
	if !wren() {
		p("  WREN(2): WEL did NOT latch — aborting")
		return
	}
	userTxn(opPP4B, scratchOff, 4, pattern, 0)
	ok, st = waitWIP(programPollMax)
	p("  page program 0x12: WIP-clear=%v status=%#02x", ok, st)
	if !ok {
		p("  => PROGRAM TIMED OUT")
		return
	}
	progWin := readWin(scratchOff, n)
	progCmd := userTxn(opREAD4B, scratchOff, 4, nil, n)
	want := pattern[:n]
	p("  after program window: %s (match: %v)", hex(progWin), eq(progWin, want))
	p("  after program cmd0x13: %s (match: %v)", hex(progCmd), eq(progCmd, want))

	switch {
	case eq(progWin, want) && eq(progCmd, want) && allEq(erased, 0xFF):
		p("  => WRITE PATH OK: erase + program + readback all verified")
	default:
		p("  => WRITE PATH FAILED — inspect the bytes above")
	}
}
