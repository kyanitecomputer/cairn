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
	"strings"
	"time"
	"unsafe"

	"github.com/kyanitecomputer/aspeed-go/hal/aspeedgfx"
	"github.com/kyanitecomputer/aspeed-go/hal/edid"
	"github.com/kyanitecomputer/aspeed-go/hal/framebuffer"
	"github.com/usbarmory/tamago/soc/aspeed/ast2700"

	"src.kyanite.computer/core/console"
)

// dpReadyTimeout bounds how long we wait for the DPMCU to report the DP link
// ready after programming an EDID-derived mode before falling back to the
// known-good 800x600. A real monitor's DPMCU link training completes well
// within this window; exceeding it means the mode cannot be carried.
const dpReadyTimeout = 2 * time.Second

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

	mode = programMode(mode)

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

// programMode configures the CRT clock/timing and DPMCU DISPLAY_FORMAT for mode
// and confirms the DP link comes ready. For an EDID-derived mode the DPMCU link
// cannot carry (link training never completes), or a mode whose pixel clock is
// unrepresentable, it falls back to the known-good 800x600 rather than leaving
// the panel blank. It returns the mode actually programmed.
//
// The 800x600 mode is hardware-validated, so it is programmed unconditionally
// without probing DP readiness. This keeps the no-cable / no-EDID path (which
// already resolves to 800x600 in pickMode) fast and unconditional.
func programMode(mode aspeedgfx.Mode) aspeedgfx.Mode {
	if !aspeedgfx.ConfigureCRT(mode) {
		fmt.Printf("GFX: pixel clock for %dx%d unsupported, using 800x600\n", mode.Width, mode.Height)
		return fallbackMode()
	}
	if mode.ModeIndex == aspeedgfx.ASTDP_800x600_60 {
		return mode
	}
	if aspeedgfx.WaitDPReady(dpReadyTimeout) {
		return mode
	}
	fmt.Printf("GFX: DP link not ready for %dx%d, falling back to 800x600\n", mode.Width, mode.Height)
	return fallbackMode()
}

// fallbackMode programs the known-good 800x600 mode and returns it.
func fallbackMode() aspeedgfx.Mode {
	mode := aspeedgfx.Mode800x600
	aspeedgfx.ConfigureCRT(mode)
	return mode
}

// flushFramebuffer writes the framebuffer back to DRAM so the GFX scanout DMA
// (a non-coherent DRAM master) observes the pixels the CA35 wrote.
func flushFramebuffer(fb *framebuffer.Framebuffer) {
	ast2700.ARM.CleanDataCacheRange(fb.Addr, uintptr(len(fb.Buf)))
}

// Commands returns the display bring-up console commands. It is only present in
// the video-enabled build; the default build's stub returns nil.
func Commands() []console.Command {
	return []console.Command{displayCmd()}
}

// displayCmd dumps GFX/DP/DPMCU register state for display bring-up. It reports
// the hot-plug-detect level, whether the BootMCU has released the DPMCU core,
// the DP link-ready scratch codes, and the CRT scanout timing/address so a
// black panel can be diagnosed from the serial console.
func displayCmd() console.Command {
	return console.Command{
		Name: "display",
		Help: "display — dump GFX/DP/DPMCU state (video bring-up)",
		Run: func([]string) (string, error) {
			if gfxCtl == nil {
				return "display not initialised", nil
			}
			st := gfxCtl.Status()
			var b strings.Builder
			fmt.Fprintf(&b, "mode:      %dx%d  HPD=%v  DPMCU_running=%v  DP_ready=%v\n",
				curMode.Width, curMode.Height, aspeedgfx.HPDAsserted(), aspeedgfx.DPMCURunning(), st.DPReady)
			fmt.Fprintf(&b, "GFX:       ctrl1=%08x ctrl2=%08x status=%08x addr=%08x offset=%08x\n",
				st.Ctrl1, st.Ctrl2, st.CRTCStatus, st.Addr, st.Offset)
			fmt.Fprintf(&b, "GFX time:  horiz0=%08x horiz1=%08x vert0=%08x vert1=%08x\n",
				st.Horiz0, st.Horiz1, st.Vert0, st.Vert1)
			fmt.Fprintf(&b, "DP:        version=%08x source=%08x dpmcu_de0=%08x redrv=%08x\n",
				st.DPVersion, st.DPSource, st.DPMCU, st.ReDriver)
			fmt.Fprintf(&b, "DPMCU:     ctrl=%08x int=%08x\n", st.DPMCUCtrl, st.DPMCUInt)
			fmt.Fprintf(&b, "DP ready:  scu_dac=%08x pcie0=%08x(code=%02x) pcie1=%08x(code=%02x)",
				st.SCUDAC, st.PCIE0DP, (st.PCIE0DP>>8)&0xff, st.PCIE1DP, (st.PCIE1DP>>8)&0xff)
			return b.String(), nil
		},
	}
}
