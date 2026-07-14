// Package video brings up the AST2700 SoC display (GFX/CRT → DisplayPort) and
// mirrors the UART console onto an attached monitor. It is an optional feature,
// compiled only with the `ast2700video` build tag; the default build omits it
// entirely (see video_disabled.go).
//
// When enabled, the framebuffer console installs a runtime printk hook, so the
// board package's own printk must be disabled with the `linkprintk` build tag —
// i.e. build with `-tags ...,ast2700video,linkprintk`.
//
// Ported from the cmd/nats validation harness (video_ast2700.go + console.go).

//go:build ast2700video

package video

import (
	"fmt"
	"unsafe"

	"github.com/kyanitecomputer/aspeed-go/hal/aspeedgfx"
	"github.com/kyanitecomputer/aspeed-go/hal/edid"
	"github.com/kyanitecomputer/aspeed-go/hal/framebuffer"
	"github.com/usbarmory/tamago/soc/aspeed/ast2700"
)

const videoBytesPerPixel = 4

// videoMaxBytes sizes the framebuffer backing store for the largest supported
// mode (1920x1200 XRGB8888). Smaller modes use a prefix of this buffer.
const videoMaxBytes = 1920 * 1200 * videoBytesPerPixel

// videoMemPad backs the framebuffer. It is over-allocated by one page so the
// scanout buffer can be 4 KB aligned (see alignedFBAddr).
var videoMemPad [videoMaxBytes + 4096]byte

func alignedFBAddr() uintptr {
	start := uintptr(unsafe.Pointer(&videoMemPad[0]))
	return (start + 4095) &^ 4095
}

// Display state shared with the hotplug poller.
var (
	gfxCtl  *aspeedgfx.Controller
	curMode aspeedgfx.Mode
	lastHPD bool
)

// Init brings up the AST2700 SoC display (GFX/CRT → DisplayPort) and installs a
// text console that mirrors the UART console output (white text on a black
// background). The display resolution is auto-detected from the monitor's EDID
// over DP AUX, falling back to 800x600 when EDID is unavailable.
//
// Division of responsibility:
//   - The BootMCU (BMCU) loads and starts the DPMCU firmware, trains the DP
//     link, and grants the GFX scanout master DRAM read access via the DRAM
//     MPU (ca35::enable_dram_access). Without that grant the scanout reads are
//     blocked and the panel stays black.
//   - Here on the PSP (CA35) we read EDID, program the CRT clock/timing, build
//     the framebuffer, and route the GFX raster to the DP output.
//
// The GFX scanout is a direct DRAM master, so the framebuffer must be flushed
// from the CA35 data cache before the controller is enabled and after every
// console update (see fbConsole.flushRows).
func Init() {
	gfxCtl = &aspeedgfx.Controller{Base: aspeedgfx.AST2700GFXBase}
	bringUpMode(pickMode(), false)
	lastHPD = aspeedgfx.HPDAsserted()
}

// PollHotplug re-acquires the display mode when a monitor is newly connected. It
// is safe to call periodically from the main loop.
func PollHotplug() {
	hpd := aspeedgfx.HPDAsserted()
	defer func() { lastHPD = hpd }()

	if !hpd || lastHPD {
		return // no rising edge
	}
	mode := pickMode()
	if mode.Width == curMode.Width && mode.Height == curMode.Height {
		return
	}
	fmt.Printf("GFX: display hotplug -> %dx%d\n", mode.Width, mode.Height)
	bringUpMode(mode, true)
}

// pickMode reads and parses EDID to choose a display mode, falling back to
// 800x600 on any failure.
func pickMode() aspeedgfx.Mode {
	data, err := aspeedgfx.ReadEDID()
	if err != nil {
		fmt.Printf("GFX: EDID read failed (%v), using 800x600\n", err)
		return aspeedgfx.Mode800x600
	}
	e, err := edid.Parse(data)
	if err != nil {
		fmt.Printf("GFX: EDID parse failed (%v), using 800x600\n", err)
		return aspeedgfx.Mode800x600
	}
	mode, ok := aspeedgfx.SelectMode(e)
	if !ok {
		fmt.Printf("GFX: display %q advertises no supported mode, using 800x600\n", e.ManufacturerID)
		return aspeedgfx.Mode800x600
	}
	fmt.Printf("GFX: display %q preferred %dx%d -> using %dx%d @ %d kHz\n",
		e.ManufacturerID, e.Preferred.HActive, e.Preferred.VActive,
		mode.Width, mode.Height, mode.PixelClockKHz)
	return mode
}

// bringUpMode programs the clock, timing, framebuffer and console for mode. On
// reacquire it first disables the controller for a clean retrain. If the mode's
// pixel clock is not representable it falls back to 800x600.
func bringUpMode(mode aspeedgfx.Mode, reacquire bool) {
	if reacquire {
		gfxCtl.Disable()
	}

	if !aspeedgfx.ConfigureCRT(mode) {
		fmt.Printf("GFX: pixel clock for %dx%d unsupported, using 800x600\n", mode.Width, mode.Height)
		mode = aspeedgfx.Mode800x600
		aspeedgfx.ConfigureCRT(mode)
	}

	fb := framebuffer.New(framebuffer.Config{
		Width:    mode.Width,
		Height:   mode.Height,
		Format:   framebuffer.XRGB8888,
		PhysAddr: alignedFBAddr(),
	})
	fb.Fill(conBG)
	flushFramebuffer(fb)

	gfxCtl.SetMode(mode, fb)
	gfxCtl.Enable()
	gfxCtl.SetAddress(fb.Addr)

	// (Re)activate the framebuffer console so printk output is mirrored onto
	// the display at the new geometry.
	con = newFBConsole(fb, conScale)
	curMode = mode
}

// flushFramebuffer writes the framebuffer back to DRAM so the GFX scanout DMA
// (a non-coherent DRAM master) observes the pixels the CA35 wrote.
func flushFramebuffer(fb *framebuffer.Framebuffer) {
	ast2700.ARM.CleanDataCacheRange(fb.Addr, uintptr(len(fb.Buf)))
}
