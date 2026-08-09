// Package diag is a read-only FMC/SPI-NOR hardware bring-up probe for the
// AST2700 DC-SCM board. It is compiled into the cairn payload only with the
// `flashdiag` build tag and, when present, short-circuits normal boot (see
// diaghook_on.go) so the board comes up, runs the probes, prints the findings
// over the UART console, and then idles.
//
// It answers the open Phase-0 questions from FMC_SPI_FLASH_PLAN.md directly on
// silicon, without touching the Rust BootMCU:
//
//   Q0.1  Does FMC CE0 RDID (0x9F) return a stable, plausible JEDEC ID?
//   Q0.2  Are byte-granular window reads reliable, or is the XIP window
//         32-bit-read-only? (session-handoff.md:40-47)
//   Q0.3  What address mode did the BootMCU leave the flash/controller in at
//         CA35 entry? (the 3B/4B handoff hazard)
//   Q0.4a Does the ASPEED user-mode read port need a dummy write per received
//         byte (the Rust `user_transfer` double-clock) or a plain read
//         (the Go driver's loop)?
//
// Everything here is READ-ONLY: no WREN, no program, no erase. It cannot alter
// flash contents, so it is safe to run on the same chip that holds the boot
// image. The controller CE0 control register is saved and restored around the
// user-mode transactions.
//
// This package is only meant to be used with GOOS=tamago GOARCH=arm64.

//go:build tamago

package diag

import (
	"fmt"
	"time"

	"github.com/kyanitecomputer/aspeed-go/hal/spi"
	"github.com/kyanitecomputer/aspeed-go/reg"
)

// AST2700 FMC controller register base and memory-mapped flash window, as seen
// by the CA35. These are the addresses cairn's store already assumes
// (spi.AST2700FMCBase / spi.AST2700FMCWindow); the probe verifies them. Both
// fall below DRAM (0x400000000) and are flat-mapped as Device-nGnRnE memory by
// the TamaGo arm64 MMU, so accesses reach the controller uncached and will not
// fault.
const (
	fmcBase = uintptr(0x14000000)
	fmcWin  = uintptr(0x100000000)
)

// FMC/SPI register offsets (spi_ast2700_v1 block).
const (
	rFlashConfig = 0x000
	rCECtrl      = 0x004
	rIRQCtrl     = 0x008
	rCmdCtrl     = 0x00C
	rCE0Ctrl     = 0x010 // CE_CTRL_N[0..3] at 0x010,0x014,0x018,0x01C
	rCE0Range    = 0x030 // CE_ADDR_RANGE[0..3] at 0x030,0x034,0x038,0x03C
	rEngineStat  = 0x1E0
	rLockSRST    = 0x1F0
	rLockKeep    = 0x1F4 // LOCK_SOC_RESET: keeps value across SOC reset
	rLockSOC     = 0x1F8
	rLockEff     = 0x1FC // read-only: LockSRST OR LockSOC
)

// CE_CTRL_N (per-CE control) bit fields.
const (
	cmdModeMask = 0x3
	cmdModeAuto = 0x0 // auto-read (memory-mapped XIP)
	cmdModeUser = 0x3 // user command mode
	ceStop      = 1 << 2
	ioModeMask  = 0xf << 28
)

// SPI NOR opcodes used read-only.
const (
	opRDID   = 0x9F
	opRead3B = 0x03
	opRead4B = 0x13
)

func p(format string, args ...any) { println(fmt.Sprintf(format, args...)) }

func r32(off uintptr) uint32    { return reg.Read32(fmcBase + off) }
func w32(off uintptr, v uint32) { reg.Write32(fmcBase+off, v) }

// Run executes the probe suite and never returns; it idles at the end so the
// console output stays readable.
func Run() {
	p("")
	p("================ cairn FMC/SPI-NOR hardware diagnostic ================")
	p("build: flashdiag  (read-only: no program, no erase)")
	p("FMC register base : %#x", uint64(fmcBase))
	p("FMC flash window  : %#x", uint64(fmcWin))
	p("-----------------------------------------------------------------------")

	dumpRegisters()
	entryMode := decodeAddrMode()
	decodeSegments()
	ref := windowProbe()          // Q0.2 + reference content for Q0.3
	rdidProbe()                   // Q0.1 + Q0.4a
	addrModeProbe(ref, entryMode) // Q0.3
	dmaProbe()                    // Q0.4b (opt-in; stub unless flashdiagdma)

	p("-----------------------------------------------------------------------")
	p("DIAG COMPLETE — see findings above. Idling.")
	p("=======================================================================")
	for {
		time.Sleep(60 * time.Second)
	}
}

// dumpRegisters prints the controller state observed at CA35 entry. Each read
// is announced before it happens so a fault (unmapped address) localises which
// register was being read.
func dumpRegisters() {
	p("[regs] observed FMC controller state at CA35 entry:")
	p("  FLASH_CONFIG (0x000) = %#010x", r32(rFlashConfig))
	p("  CE_CTRL      (0x004) = %#010x", r32(rCECtrl))
	p("  IRQ_CTRL     (0x008) = %#010x", r32(rIRQCtrl))
	p("  CMD_CTRL     (0x00C) = %#010x", r32(rCmdCtrl))
	for cs := 0; cs < 4; cs++ {
		p("  CE%d_CTRL     (%#05x) = %#010x", cs, rCE0Ctrl+uintptr(cs*4), r32(rCE0Ctrl+uintptr(cs*4)))
	}
	for cs := 0; cs < 4; cs++ {
		p("  CE%d_RANGE    (%#05x) = %#010x", cs, rCE0Range+uintptr(cs*4), r32(rCE0Range+uintptr(cs*4)))
	}
	p("  ENGINE_STAT  (0x1E0) = %#010x", r32(rEngineStat))
	p("  LOCK_SRST    (0x1F0) = %#010x", r32(rLockSRST))
	p("  LOCK_KEEP    (0x1F4) = %#010x", r32(rLockKeep))
	p("  LOCK_SOC     (0x1F8) = %#010x", r32(rLockSOC))
	p("  LOCK_EFF     (0x1FC) = %#010x", r32(rLockEff))
}

// decodeAddrMode reports the controller's per-CE 4-byte address configuration
// (SPI004: ADDR_4B[3:0], AUTO_READ_4B[7:4]) — half of the Q0.3 answer. It
// returns CE0's controller CMD_MODE so later probes know whether the auto-read
// window was active at entry.
func decodeAddrMode() (ce0CmdMode uint32) {
	ce := r32(rCECtrl)
	addr4b := ce & 0xf
	auto4b := (ce >> 4) & 0xf
	p("[Q0.3a] controller CE_CTRL=%#010x: ADDR_4B=%#x AUTO_READ_4B=%#x", ce, addr4b, auto4b)
	for cs := 0; cs < 4; cs++ {
		mode := (addr4b >> uint(cs)) & 1
		auto := (auto4b >> uint(cs)) & 1
		p("        CE%d: controller address mode = %s, auto-read = %s",
			cs, mode4b(mode), mode4b(auto))
	}
	ce0 := r32(rCE0Ctrl)
	ce0CmdMode = ce0 & cmdModeMask
	p("        CE0 CMD_MODE = %d (%s)", ce0CmdMode, cmdModeName(ce0CmdMode))
	return ce0CmdMode
}

func mode4b(bit uint32) string {
	if bit != 0 {
		return "4-byte"
	}
	return "3-byte"
}

func cmdModeName(m uint32) string {
	switch m {
	case 0:
		return "auto-read/XIP"
	case 1:
		return "normal-read"
	case 2:
		return "normal-write"
	default:
		return "user"
	}
}

// decodeSegments decodes each CE address-range register with the SoC-aware
// helper under test (spi.SegmentDecode) and prints the window start/size. This
// validates hal/spi/segment.go against live silicon.
func decodeSegments() {
	p("[segments] CE address-decoding windows (via spi.SegmentDecode/AST2700):")
	for cs := 0; cs < 4; cs++ {
		regVal := r32(rCE0Range + uintptr(cs*4))
		start, size, ok := spi.SegmentDecode(spi.AST2700, regVal)
		if !ok {
			p("  CE%d: reg=%#010x decode failed", cs, regVal)
			continue
		}
		p("  CE%d: reg=%#010x -> start=%#x size=%#x (%d MiB)",
			cs, regVal, start, size, size>>20)
	}
}

// windowProbe answers Q0.2: is byte-granular access to the memory-mapped flash
// window reliable, or is it 32-bit-only? It only reads (never modifies flash).
// It returns the first 32 bytes as observed via 32-bit reads, for use as the
// reference boot-image content in addrModeProbe.
func windowProbe() []byte {
	const n = 32
	p("[Q0.2] memory-mapped window read (byte vs 32-bit), first %d bytes:", n)

	byteView := make([]byte, n)
	for i := 0; i < n; i++ {
		byteView[i] = reg.Read8(fmcWin + uintptr(i))
	}
	wordView := make([]byte, n)
	for i := 0; i < n; i += 4 {
		v := reg.Read32(fmcWin + uintptr(i))
		wordView[i+0] = byte(v)
		wordView[i+1] = byte(v >> 8)
		wordView[i+2] = byte(v >> 16)
		wordView[i+3] = byte(v >> 24)
	}
	p("  8-bit  reads: %s", hex(byteView))
	p("  32-bit reads: %s", hex(wordView))
	if eq(byteView, wordView) {
		p("  => VERDICT: byte and 32-bit reads AGREE — byte-granular window access works")
	} else {
		p("  => VERDICT: byte and 32-bit reads DIFFER — window is 32-bit-read-only;")
		p("             the driver must read the window in 32-bit words (or use DMA)")
	}
	return wordView
}

// rdidProbe answers Q0.1 (JEDEC ID) and Q0.4a (double-clock). It issues RDID in
// user mode two ways: the Go driver's read-only receive loop, and the Rust
// driver's write-0xFF-then-read loop. Whichever yields a plausible JEDEC ID is
// the correct receive discipline for this controller.
func rdidProbe() {
	p("[Q0.1/Q0.4a] RDID (0x9F) in user mode, two receive disciplines:")

	saved := r32(rCE0Ctrl)
	// User mode, single-bit I/O, preserve clock bits.
	cu := (saved &^ (ioModeMask | cmdModeMask)) | cmdModeUser

	readOnly := func() [3]byte {
		beginCE0(cu)
		reg.Write8(fmcWin, opRDID)
		var id [3]byte
		for i := range id {
			id[i] = reg.Read8(fmcWin)
		}
		endCE0(cu)
		return id
	}
	dummyWrite := func() [3]byte {
		beginCE0(cu)
		reg.Write8(fmcWin, opRDID)
		var id [3]byte
		for i := range id {
			reg.Write8(fmcWin, 0xFF)
			id[i] = reg.Read8(fmcWin)
		}
		endCE0(cu)
		return id
	}

	go1 := readOnly()
	go2 := readOnly() // repeat: stability check
	ru := dummyWrite()
	w32(rCE0Ctrl, saved) // restore entry state

	p("  read-only loop (Go style)   : %02x %02x %02x   (repeat: %02x %02x %02x)",
		go1[0], go1[1], go1[2], go2[0], go2[1], go2[2])
	p("  0xFF-write loop (Rust style): %02x %02x %02x", ru[0], ru[1], ru[2])
	p("  plausible JEDEC? read-only=%v  0xFF-write=%v", plausibleID(go1), plausibleID(ru))
	p("  (a plausible ID has a non-00/non-FF manufacturer byte and stable repeats)")
}

// addrModeProbe answers Q0.3 empirically: it reads offset 0 in user mode with a
// 3-byte and a 4-byte address and compares each against the auto-read window
// reference. The variant that matches reveals the flash device's effective
// addressing at CA35 entry.
func addrModeProbe(ref []byte, entryMode uint32) {
	const n = 16
	p("[Q0.3b] device addressing at entry — user-mode read of offset 0:")
	if entryMode != cmdModeAuto {
		p("  note: CE0 was NOT in auto-read at entry (CMD_MODE=%d); the window", entryMode)
		p("        reference may not reflect flash contents. Compare raw bytes below.")
	}

	saved := r32(rCE0Ctrl)
	cu := (saved &^ (ioModeMask | cmdModeMask)) | cmdModeUser

	read3 := userRead(cu, opRead3B, 0, n, false)
	read4 := userRead(cu, opRead4B, 0, n, true)
	w32(rCE0Ctrl, saved)

	refN := ref
	if len(refN) > n {
		refN = refN[:n]
	}
	p("  auto-read window ref : %s", hex(refN))
	p("  3-byte cmd 0x03      : %s  (match=%v)", hex(read3), eq(read3, refN))
	p("  4-byte cmd 0x13      : %s  (match=%v)", hex(read4), eq(read4, refN))
	switch {
	case eq(read3, refN) && !eq(read4, refN):
		p("  => VERDICT: device is in 3-BYTE mode at entry")
	case eq(read4, refN) && !eq(read3, refN):
		p("  => VERDICT: device is in 4-BYTE mode at entry (Policy A handoff needed)")
	default:
		p("  => VERDICT: inconclusive — inspect the raw bytes above")
	}
}

// beginCE0 starts a user-mode transaction on CE0: assert then deassert CE_STOP
// to pull CE# active. Mirrors hal/spi TxRx activate.
func beginCE0(ctrlUser uint32) {
	w32(rCE0Ctrl, ctrlUser|ceStop)
	w32(rCE0Ctrl, ctrlUser&^ceStop)
}

// endCE0 ends a user-mode transaction: reassert CE_STOP to release CE#.
func endCE0(ctrlUser uint32) {
	w32(rCE0Ctrl, ctrlUser|ceStop)
}

// userRead performs one user-mode read: opcode, address (3 or 4 bytes,
// MSB-first), then n data bytes via the read-only receive loop.
func userRead(ctrlUser uint32, op byte, addr uint32, n int, fourByte bool) []byte {
	beginCE0(ctrlUser)
	reg.Write8(fmcWin, op)
	if fourByte {
		reg.Write8(fmcWin, byte(addr>>24))
	}
	reg.Write8(fmcWin, byte(addr>>16))
	reg.Write8(fmcWin, byte(addr>>8))
	reg.Write8(fmcWin, byte(addr))
	out := make([]byte, n)
	for i := range out {
		out[i] = reg.Read8(fmcWin)
	}
	endCE0(ctrlUser)
	return out
}

func plausibleID(id [3]byte) bool {
	mfr := id[0]
	if mfr == 0x00 || mfr == 0xFF {
		return false
	}
	return true
}

func eq(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

const hexDigits = "0123456789abcdef"

func hex(b []byte) string {
	out := make([]byte, 0, len(b)*3)
	for i, v := range b {
		if i > 0 {
			out = append(out, ' ')
		}
		out = append(out, hexDigits[v>>4], hexDigits[v&0xf])
	}
	return string(out)
}
