// DMA-scaling probe (opt-in). Answers Q0.4b: does the FMC DMA flash-side /
// DRAM-side address field take a byte address or a word (>>2) address, and in
// which flash-address space? It reproduces the exact mechanics the BootMCU
// uses (aspeed-rs ast2700_fmc_dma_read_sync: base 0x2000_0000, addresses >>2)
// as the first attempt, then a byte-address variant, and compares each DMA
// result against a user-mode read of the same offset.
//
// DRAM is mapped cacheable, so the destination cache lines are invalidated
// after each DMA before the CPU reads them back. Completion polling is bounded
// so a wrong encoding times out instead of hanging the board.

//go:build tamago && flashdiagdma

package diag

import (
	"unsafe"

	"github.com/usbarmory/tamago/soc/aspeed/ast2700"
)

// DMA register offsets (spi_ast2700_v1).
const (
	rDMADRAMHi   = 0x07C
	rDMACtrl     = 0x080
	rDMAFlashAdr = 0x084
	rDMADRAMAdr  = 0x088
	rDMALen      = 0x08C
	rDMACksum    = 0x090
)

const (
	dmaEnableRead = 0x1     // DMA_CTRL: ENABLE=1, DIR=0 (flash->DRAM)
	dmaStatus     = 1 << 11 // IRQ_CTRL DMA done
	dmaPollLimit  = 20_000_000
	rustFlashWin  = 0x20000000 // BootMCU DMA flash-address space base
)

func dmaProbe() {
	const off = 0x1000 // 4 KiB: byte (0x1000) and >>2 (0x400) encodings differ
	const n = 16
	p("[Q0.4b] DMA-scaling probe at flash offset %#x (%d bytes):", off, n)

	// Reference: user-mode reads of the same offset, both addressings, using
	// the MISC-clear+flush strategy (B).
	refStrat := userStrategy{name: "B", clearMisc: true}
	ref3 := userXfer(refStrat, opRead3B, off, 3, n, false)
	ref4 := userXfer(refStrat, opRead4B, off, 4, n, false)
	p("  user-mode ref 3B: %s", hex(ref3))
	p("  user-mode ref 4B: %s", hex(ref4))

	// Cache-line aligned destination buffer in DRAM (VA==PA under the flat map).
	buf := make([]byte, 128)
	base := uintptr(unsafe.Pointer(&buf[0]))
	dst := (base + 63) &^ 63 // 64-byte align
	dstOff := int(dst - base)

	attempts := []struct {
		name      string
		flashAddr uint32
		dramShift bool // true: write dram addr >>2 (Rust); false: byte
	}{
		{"Rust: base 0x2000_0000, flash>>2, dram>>2", uint32(rustFlashWin+off) >> 2, true},
		{"byte: flash offset, dram byte", uint32(off), false},
		{"byte: base 0x2000_0000+off, dram byte", uint32(rustFlashWin + off), false},
	}

	for _, a := range attempts {
		fill(buf, 0xA5) // sentinel so a no-op DMA is detectable
		ast2700.ARM.CleanDataCacheRange(dst, uintptr(n))

		var dramField uint32
		if a.dramShift {
			dramField = uint32(dst) >> 2
		} else {
			dramField = uint32(dst)
		}

		w32(rIRQCtrl, dmaStatus) // clear stale done
		w32(rDMADRAMHi, uint32(dst>>32)&0xff)
		w32(rDMAFlashAdr, a.flashAddr)
		w32(rDMADRAMAdr, dramField)
		w32(rDMALen, uint32(n-1))
		w32(rDMACtrl, dmaEnableRead)

		done := false
		for i := 0; i < dmaPollLimit; i++ {
			if r32(rIRQCtrl)&dmaStatus != 0 {
				done = true
				break
			}
		}
		cksum := r32(rDMACksum)
		w32(rDMACtrl, 0)
		w32(rIRQCtrl, dmaStatus)

		ast2700.ARM.InvalidateDataCacheRange(dst, uintptr(n))
		got := make([]byte, n)
		copy(got, buf[dstOff:dstOff+n])

		p("  attempt: %s", a.name)
		p("    flash_field=%#010x dram_field=%#010x done=%v cksum=%#010x", a.flashAddr, dramField, done, cksum)
		p("    data: %s", hex(got))
		switch {
		case !done:
			p("    => TIMED OUT (encoding likely wrong or engine stalled)")
		case eq(got, ref4):
			p("    => MATCHES 4B user-mode ref: this encoding is CORRECT (device in 4B)")
		case eq(got, ref3):
			p("    => MATCHES 3B user-mode ref: this encoding is CORRECT (device in 3B)")
		default:
			p("    => completed but data mismatches both refs (wrong address space?)")
		}
	}
}

func fill(b []byte, v byte) {
	for i := range b {
		b[i] = v
	}
}
