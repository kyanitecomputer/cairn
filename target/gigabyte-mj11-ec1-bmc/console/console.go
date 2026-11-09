// Package console wires the shared core/console shell onto the MJ11-EC1 BMC
// serial console (UART5) and registers the cairn BMC command set.
//
// The shell engine (line editing, history, completion, dispatch) lives in
// src.kyanite.computer/core/console; this package supplies the board transport
// (a blocking reader over the polled UART) and the domain commands (system
// info, chassis power, raw register access for bring-up, reboot).
//
// This package is only compiled for GOOS=tamago.

//go:build tamago

package console

import (
	"context"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unsafe"

	tboard "github.com/usbarmory/tamago/board/gigabyte/mj11ec1bmc"
	"src.kyanite.computer/core/console"

	"src.kyanite.computer/cairn/pkg/bmcdev"
	"src.kyanite.computer/cairn/pkg/config"
)

// mdMaxWords caps a single md read so a typo cannot dump megabytes of MMIO.
const mdMaxWords = 64

// AST2500 SPI flash windows: an AHB write there becomes a NOR page program
// while the flash controller is write-enabled.
var flashWindows = [][2]uint64{
	{0x0000_0000, 0x1000_0000},
	{0x2000_0000, 0x3000_0000},
}

// WDT1 registers, for the reboot command.
const (
	wdt1Reload    = 0x1e78_5004
	wdt1Restart   = 0x1e78_5008
	wdt1Ctrl      = 0x1e78_500c
	wdtRestartKey = 0x4755
	// enable, reset the SoC on timeout, 1 MHz clock
	wdtCtrlReset = 1<<0 | 1<<1 | 1<<4
)

// Options carries the target surfaces the console commands operate on.
type Options struct {
	Cfg     *config.Manager
	Chassis bmcdev.Chassis
	Start   time.Time // process start, for uptime
}

// NewShell builds a cairn BMC console shell from opt. Each call returns an
// independent shell instance carrying its own line-editing and history state.
func NewShell(opt Options) *console.Shell {
	return console.New(console.Config{
		Prompt: "cairn# ",
		Banner: "Cairn BMC console",
		Commands: []console.Command{
			showCmd(opt),
			powerCmd(opt),
			mdCmd(),
			mwCmd(),
			rebootCmd(),
		},
	})
}

// Service returns a supervised service function that runs the interactive
// console on UART5. It never returns under normal operation.
func Service(opt Options) func(ctx context.Context) error {
	sh := NewShell(opt)
	return func(ctx context.Context) error {
		sh.Run(&uartRW{})
		return nil // unreachable on hardware; restart if the loop ever exits
	}
}

// --- commands ----------------------------------------------------------------

func showCmd(opt Options) console.Command {
	return console.Command{
		Name: "show",
		Help: "show system — system info (hostname, uptime, memory)",
		Complete: func(prev []string, _ string) []string {
			if len(prev) == 0 {
				return []string{"system"}
			}
			return nil
		},
		Run: func(args []string) (string, error) {
			if len(args) == 0 || args[0] != "system" && args[0] != "sys" {
				return "usage: show system", nil
			}
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			var b strings.Builder
			fmt.Fprintf(&b, "Hostname:    %s\n", opt.Cfg.Hostname())
			fmt.Fprintf(&b, "Chassis:     %s (power %s)\n", opt.Chassis.Model(), opt.Chassis.Power())
			fmt.Fprintf(&b, "Uptime:      %s\n", time.Since(opt.Start).Round(time.Second))
			fmt.Fprintf(&b, "Heap:        %d KiB in use, %d KiB from the system\n", ms.HeapInuse>>10, ms.Sys>>10)
			fmt.Fprintf(&b, "Runtime:     %s/%s %s\n", runtime.GOOS, runtime.GOARCH, runtime.Version())
			return b.String(), nil
		},
	}
}

func powerCmd(opt Options) console.Command {
	return console.Command{
		Name: "power",
		Help: "power on|off|status — host chassis power control",
		Complete: func(prev []string, _ string) []string {
			if len(prev) == 0 {
				return []string{"on", "off", "status"}
			}
			return nil
		},
		Run: func(args []string) (string, error) {
			if len(args) != 1 {
				return "usage: power on|off|status", nil
			}
			switch args[0] {
			case "status":
				return fmt.Sprintf("chassis power: %s", opt.Chassis.Power()), nil
			case "on":
				if err := opt.Chassis.SetPower(true); err != nil {
					return "", err
				}
				return "chassis power on requested", nil
			case "off":
				if err := opt.Chassis.SetPower(false); err != nil {
					return "", err
				}
				return "chassis power off requested", nil
			default:
				return "usage: power on|off|status", nil
			}
		},
	}
}

// mdCmd implements md <hex-addr> [words]: 32-bit memory/MMIO display. This is a
// bring-up tool (register inspection over the console).
func mdCmd() console.Command {
	return console.Command{
		Name: "md",
		Help: "md <hex-addr> [words] — display 32-bit words (bring-up tool)",
		Run: func(args []string) (string, error) {
			if len(args) < 1 || len(args) > 2 {
				return "usage: md <hex-addr> [words]", nil
			}
			addr, err := parseHex(args[0])
			if err != nil {
				return "", err
			}
			words := 1
			if len(args) == 2 {
				words, err = strconv.Atoi(args[1])
				if err != nil || words < 1 || words > mdMaxWords {
					return fmt.Sprintf("words must be 1..%d", mdMaxWords), nil
				}
			}
			if addr%4 != 0 {
				return "address must be 4-byte aligned", nil
			}
			if addr+uint64(words)*4 > 1<<32 {
				return "range must lie below 4 GiB", nil
			}
			var b strings.Builder
			for i := 0; i < words; i++ {
				a := addr + uint64(i)*4
				if i%4 == 0 {
					if i > 0 {
						b.WriteByte('\n')
					}
					fmt.Fprintf(&b, "%08x:", a)
				}
				fmt.Fprintf(&b, " %08x", read32(a))
			}
			return b.String(), nil
		},
	}
}

// mwCmd implements mw <hex-addr> <hex-value>: 32-bit memory/MMIO write. It
// refuses the SPI flash windows.
func mwCmd() console.Command {
	return console.Command{
		Name: "mw",
		Help: "mw <hex-addr> <hex-value> — write a 32-bit word (bring-up tool)",
		Run: func(args []string) (string, error) {
			if len(args) != 2 {
				return "usage: mw <hex-addr> <hex-value>", nil
			}
			addr, err := parseHex(args[0])
			if err != nil {
				return "", err
			}
			if addr%4 != 0 {
				return "address must be 4-byte aligned", nil
			}
			if addr >= 1<<32 {
				return "address must lie below 4 GiB", nil
			}
			for _, w := range flashWindows {
				if addr >= w[0] && addr < w[1] {
					return "refusing to write the SPI flash window", nil
				}
			}
			val, err := parseHex(args[1])
			if err != nil {
				return "", err
			}
			if val > 0xffffffff {
				return "value must fit in 32 bits", nil
			}
			write32(addr, uint32(val))
			return fmt.Sprintf("%08x: %08x", addr, read32(addr)), nil
		},
	}
}

// rebootCmd resets the SoC through WDT1. The DRAM controller keeps running
// across that reset, so the loader skips DRAM training on the way back.
func rebootCmd() console.Command {
	return console.Command{
		Name: "reboot",
		Help: "reboot — reset the BMC SoC (WDT1)",
		Run: func(_ []string) (string, error) {
			write32(wdt1Reload, 1000) // 1 ms at 1 MHz
			write32(wdt1Restart, wdtRestartKey)
			write32(wdt1Ctrl, wdtCtrlReset)
			time.Sleep(time.Second)
			return "", fmt.Errorf("WDT1 did not reset the SoC")
		},
	}
}

func parseHex(s string) (uint64, error) {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	v, err := strconv.ParseUint(s, 16, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid hex %q", s)
	}
	return v, nil
}

//go:nosplit
func read32(addr uint64) uint32 {
	return *(*uint32)(unsafe.Pointer(uintptr(addr)))
}

//go:nosplit
func write32(addr uint64, val uint32) {
	*(*uint32)(unsafe.Pointer(uintptr(addr))) = val
}

// --- transport ----------------------------------------------------------------

// uartRW adapts the polled UART5 into the blocking io.ReadWriter the console
// engine expects. Read blocks until a byte arrives, yielding to the scheduler
// between polls; Write transmits synchronously.
type uartRW struct{}

func (uartRW) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if c, ok := tboard.UART5.Rx(); ok {
			p[0] = c
			return 1, nil
		}
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
}

func (uartRW) Write(p []byte) (int, error) {
	tboard.UART5.Write(p)
	return len(p), nil
}
