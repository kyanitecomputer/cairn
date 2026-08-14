// EHCI transfer-layer DMA backend for the `ehci enum` console command.
//
// The aspeed-go EHCI driver stays free of runtime/OS dependencies and asks the
// caller for physically-addressable, cache-maintained memory via ehci.DMA. This
// file supplies that backend on the AST2700 DC-SCM: a static DRAM arena (whose
// identity-mapped address is what the controller DMAs to) plus a LIFO bump
// allocator and the CA35 cache-maintenance ops. LIFO free matches the transfer
// layer's defer-ordered allocation of QH/qTD/buffers.

//go:build tamago

package console

import (
	"unsafe"

	"github.com/usbarmory/tamago/soc/aspeed/ast2700"
)

// ehciDMAArena is the static DRAM region backing EHCI schedule structures and
// transfer buffers. 32 KiB comfortably holds a QH, three qTDs and two
// page-aligned 4 KiB buffers plus alignment padding.
var ehciDMAArena [32768]byte

// ehciDMA is a LIFO bump allocator over ehciDMAArena implementing ehci.DMA.
type ehciDMA struct {
	base  uint64 // physical (== virtual, identity-mapped) base of the arena.
	off   int    // current bump offset.
	marks []int  // saved offsets for LIFO Free.
}

func newEHCIDMA() *ehciDMA {
	return &ehciDMA{base: uint64(uintptr(unsafe.Pointer(&ehciDMAArena[0])))}
}

// Alloc returns a zeroed sub-slice of the arena aligned to align (by physical
// address) and its physical address.
func (a *ehciDMA) Alloc(size, align int) ([]byte, uint64) {
	base := a.base + uint64(a.off)
	aligned := (base + uint64(align-1)) &^ uint64(align-1)
	start := a.off + int(aligned-base)
	end := start + size
	if end > len(ehciDMAArena) {
		return nil, 0 // out of arena; caller surfaces the resulting fault
	}
	a.marks = append(a.marks, a.off)
	a.off = end
	b := ehciDMAArena[start:end]
	for i := range b {
		b[i] = 0
	}
	return b, a.base + uint64(start)
}

// Free releases the most recent allocation (LIFO).
func (a *ehciDMA) Free(uint64) {
	if n := len(a.marks); n > 0 {
		a.off = a.marks[n-1]
		a.marks = a.marks[:n-1]
	}
}

// Clean writes CPU cache lines back to memory before the controller reads.
func (a *ehciDMA) Clean(phys uint64, size int) {
	ast2700.ARM.CleanDataCacheRange(uintptr(phys), uintptr(size))
}

// Invalidate refreshes CPU cache lines before the CPU reads controller writes.
func (a *ehciDMA) Invalidate(phys uint64, size int) {
	ast2700.ARM.InvalidateDataCacheRange(uintptr(phys), uintptr(size))
}
