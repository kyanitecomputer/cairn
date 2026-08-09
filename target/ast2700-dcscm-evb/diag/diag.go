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
	rCE1Ctrl     = 0x014
	rCE0Ctrl     = 0x010 // CE_CTRL_N[0..3] at 0x010,0x014,0x018,0x01C
	rCE0Range    = 0x030 // CE_ADDR_RANGE[0..3] at 0x030,0x034,0x038,0x03C
	rMisc        = 0x054 // MISC_CTRL / SAFS mode-select (SPI054; must be 0 for user PIO)
	rDataFIFO    = 0x200 // AST2700 user-mode data FIFO (ctrl_base + 0x200 + fifo_offset)
	rDmaFifoLen  = 0x1C0
	rEngineStat  = 0x1E0
	rDataInMon   = 0x1E4 // last data shifted in (RO) — detects whether the engine clocked
	rLockSRST    = 0x1F0
	rLockKeep    = 0x1F4 // LOCK_SOC_RESET: keeps value across SOC reset
	rLockSOC     = 0x1F8
	rLockEff     = 0x1FC // read-only: LockSRST OR LockSOC
)

// CE_CTRL_N (per-CE control) bit fields.
const (
	cmdModeMask       = 0x3
	cmdModeAuto       = 0x0 // auto-read (memory-mapped XIP)
	cmdModeNormalRead = 0x1 // controller-generated read using CE_CTRL.COMMAND
	cmdModeUser       = 0x3 // user command mode
	cmdShift          = 16  // CE_CTRL.COMMAND field [23:16]
	ceStop            = 1 << 2
	ioModeMask        = 0xf << 28
	clockBits         = 0x0f000f00 // CLOCK_RATE_LOW [11:8] | CLOCK_RATE_HIGH [27:24]
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
	commandPathProbe(ref)         // can the CA35 command the FMC at all?
	dmaProbe()                    // Q0.4b (controller DMA path; stub unless flashdiagdma)

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
	p("  MISC_CTRL    (0x054) = %#010x  (SAFS mode-select; must be 0 for user PIO)", r32(rMisc))
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

// userStrategy describes one candidate AST2700 user-mode command mechanism.
// The first hardware run returned zeros for every user-mode transaction, so we
// no longer assume a single mechanism: we try several and print each, so one
// boot reveals which the silicon actually honours. The variables under test
// are those the vendor (Zephyr) user-mode path touches that the first probe did
// not: clearing MISC_CTRL/SAFS (spi_aspeed.c:382), a CE_CTRL read-back flush
// (:388), routing data through the 0x200 FIFO (:376), and a real clock divider.
type userStrategy struct {
	name      string
	useFIFO   bool // route data through ctrl_base+0x200+fifo_offset instead of the window
	clearMisc bool // write 0 to MISC_CTRL (0x054) to disable SAFS
	setClock  bool // force a non-zero clock divider (mirror CE1's) instead of CE0's
}

func userStrategies() []userStrategy {
	return []userStrategy{
		{"A window, as-is (baseline)", false, false, false},
		{"B window, clear MISC + flush", false, true, false},
		{"C window, clear MISC + flush + clk", false, true, true},
		{"D FIFO@0x200, clear MISC + flush", true, true, false},
	}
}

// fifoPort returns the AST2700 user-mode data FIFO address for CE0:
// ctrl_base + 0x200 + (CE0 window start / 16 MiB).
func fifoPort() uintptr {
	start, _, _ := spi.SegmentDecode(spi.AST2700, r32(rCE0Range))
	return fmcBase + rDataFIFO + uintptr(start/0x0100_0000)
}

// userXfer runs one user-mode transaction under the given strategy: send opcode
// + addrLen address bytes (MSB-first), then read n bytes. dummyWrite selects
// the Rust "write 0xFF then read" receive discipline instead of a plain read.
// It saves and restores CE0_CTRL and MISC_CTRL.
func userXfer(s userStrategy, op byte, addr uint32, addrLen, n int, dummyWrite bool) []byte {
	savedCE := r32(rCE0Ctrl)
	savedMisc := r32(rMisc)
	if s.clearMisc {
		w32(rMisc, 0)
	}
	cu := (savedCE &^ (ioModeMask | cmdModeMask)) | cmdModeUser
	if s.setClock {
		cu = (cu &^ clockBits) | (r32(rCE1Ctrl) & clockBits)
	}
	port := fmcWin
	if s.useFIFO {
		port = fifoPort()
	}

	w32(rCE0Ctrl, cu|ceStop)
	w32(rCE0Ctrl, cu&^ceStop)
	_ = r32(rCE0Ctrl) // read-back flush (vendor does this)

	reg.Write8(port, op)
	for i := 0; i < addrLen; i++ {
		reg.Write8(port, byte(addr>>uint(8*(addrLen-1-i))))
	}
	out := make([]byte, n)
	for i := range out {
		if dummyWrite {
			reg.Write8(port, 0xFF)
		}
		out[i] = reg.Read8(port)
	}

	w32(rCE0Ctrl, cu|ceStop)
	w32(rCE0Ctrl, savedCE)
	w32(rMisc, savedMisc)
	return out
}

// rdidProbe answers Q0.1 (JEDEC ID) and Q0.4a (receive discipline) across all
// candidate user-mode strategies, since the first run showed the naive window
// approach returns zeros. For each strategy it prints RDID via both the
// read-only loop and the 0xFF-write loop.
func rdidProbe() {
	p("[Q0.1/Q0.4a] RDID (0x9F) across user-mode strategies (FIFO port=%#x):", uint64(fifoPort()))
	for _, s := range userStrategies() {
		ro := userXfer(s, opRDID, 0, 0, 3, false)
		dw := userXfer(s, opRDID, 0, 0, 3, true)
		p("  %-34s read-only=%s  0xFF-write=%s  plausible(ro=%v dw=%v)",
			s.name, hex(ro), hex(dw), plausible3(ro), plausible3(dw))
	}
	p("  (plausible = non-00/non-FF first byte; that strategy+discipline is the one to use)")
}

// addrModeProbe answers Q0.3 empirically: for the two most likely strategies it
// reads offset 0 with a 3-byte and a 4-byte address and compares against the
// auto-read window reference. The matching variant reveals the device's
// effective addressing at entry.
func addrModeProbe(ref []byte, entryMode uint32) {
	const n = 16
	p("[Q0.3b] device addressing at entry — user-mode read of offset 0:")
	if entryMode != cmdModeAuto {
		p("  note: CE0 was NOT in auto-read at entry (CMD_MODE=%d); window ref", entryMode)
		p("        may not reflect flash contents. Compare raw bytes below.")
	}
	refN := ref
	if len(refN) > n {
		refN = refN[:n]
	}
	p("  auto-read window ref : %s", hex(refN))
	for _, s := range userStrategies() {
		if !s.clearMisc {
			continue // baseline already known to fail; skip the noise
		}
		read3 := userXfer(s, opRead3B, 0, 3, n, false)
		read4 := userXfer(s, opRead4B, 0, 4, n, false)
		p("  [%s]", s.name)
		p("    3B 0x03: %s (match=%v)", hex(read3), eq(read3, refN))
		p("    4B 0x13: %s (match=%v)", hex(read4), eq(read4, refN))
	}
	p("  => device is in 3B if 0x03 matches the window ref, 4B if 0x13 matches")
}

// commandPathProbe determines whether the CA35 can make the FMC engine clock a
// transaction at all — the crux question after every user-mode PIO returned
// zeros. Two independent, non-destructive signals:
//
//  1. Normal-read command mode (CMD_MODE=1): the controller generates the read
//     using CE_CTRL.COMMAND + auto-generated address (per CE_CTRL/0x004 ADDR_4B,
//     currently 4B). The CA35 only *reads* the window (known to work). If this
//     matches the auto-read reference, the controller command engine is
//     drivable from the CA35 and only raw user-mode window PIO is the problem.
//  2. Engine-shift observation: issue a user-mode opcode write (0x9F) and read
//     ENGINE_STATUS / DATA_IN_MONITOR / DMA_FIFO_LEN before and after. If they
//     change, the CA35's window writes DO clock the bus (so the read-back is
//     the issue); if nothing moves, CA35 user-mode window writes are dropped
//     (flash writes must be delegated to the BootMCU/RoT).
func commandPathProbe(ref []byte) {
	const n = 16
	p("[cmd-path] can the CA35 drive the FMC engine?")

	refN := ref
	if len(refN) > n {
		refN = refN[:n]
	}

	// (1) normal-read mode with COMMAND=0x13 (matches the 4B auto-read the
	// controller is already configured for).
	saved := r32(rCE0Ctrl)
	savedMisc := r32(rMisc)
	w32(rMisc, 0)
	nr := (saved &^ (ioModeMask | cmdModeMask | (0xff << cmdShift))) |
		cmdModeNormalRead | (uint32(opRead4B) << cmdShift)
	w32(rCE0Ctrl, nr)
	_ = r32(rCE0Ctrl)
	got := make([]byte, n)
	for i := 0; i < n; i++ {
		got[i] = reg.Read8(fmcWin + uintptr(i))
	}
	w32(rCE0Ctrl, saved)
	w32(rMisc, savedMisc)
	p("  normal-read (CMD_MODE=1, COMMAND=0x13): %s (match=%v)", hex(got), eq(got, refN))

	// (2) engine-shift observation around a user-mode opcode write.
	es0, dim0, fl0 := r32(rEngineStat), r32(rDataInMon), r32(rDmaFifoLen)
	sMisc := r32(rMisc)
	w32(rMisc, 0)
	cu := (saved &^ (ioModeMask | cmdModeMask)) | cmdModeUser
	w32(rCE0Ctrl, cu|ceStop)
	w32(rCE0Ctrl, cu&^ceStop)
	_ = r32(rCE0Ctrl)
	reg.Write8(fmcWin, opRDID)
	reg.Write8(fmcWin, 0xFF)
	es1, dim1, fl1 := r32(rEngineStat), r32(rDataInMon), r32(rDmaFifoLen)
	w32(rCE0Ctrl, cu|ceStop)
	w32(rCE0Ctrl, saved)
	w32(rMisc, sMisc)
	p("  engine before: STAT=%#010x DATA_IN=%#010x FIFO_LEN=%#010x", es0, dim0, fl0)
	p("  engine after : STAT=%#010x DATA_IN=%#010x FIFO_LEN=%#010x", es1, dim1, fl1)
	if es0 != es1 || dim0 != dim1 || fl0 != fl1 {
		p("  => engine state CHANGED: CA35 user-mode writes DO clock the bus (read-back is the issue)")
	} else {
		p("  => engine state UNCHANGED: CA35 user-mode window writes appear dropped")
		p("     (flash writes likely must be delegated to the BootMCU/RoT over IPC)")
	}
}

func plausible3(id []byte) bool {
	if len(id) == 0 {
		return false
	}
	mfr := id[0]
	return mfr != 0x00 && mfr != 0xFF
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
