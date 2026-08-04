// Package console wires the shared core/console shell onto the AST2700 DC-SCM
// serial console (UART12) and registers the cairn BMC command set.
//
// The shell engine (line editing, history, completion, dispatch) lives in
// src.kyanite.computer/core/console; this package supplies the board transport
// (a blocking reader over the polled UART) and the domain commands (system
// info, chassis power, raw register access for bring-up).
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

	tboard "github.com/usbarmory/tamago/board/aspeed/ast2700dcscm"
	"src.kyanite.computer/core/console"

	"src.kyanite.computer/cairn/pkg/bmcdev"
	"src.kyanite.computer/cairn/pkg/config"
)

// mdMaxWords caps a single md read so a typo cannot dump megabytes of MMIO.
const mdMaxWords = 64

// Options carries the target surfaces the console commands operate on.
type Options struct {
	Cfg     *config.Manager
	Chassis bmcdev.Chassis
	Start   time.Time // process start, for uptime

	// Extra registers additional target commands (e.g. the video bring-up
	// commands, present only in the ast2700video build).
	Extra []console.Command
}

// NewShell builds a cairn BMC console shell from opt. Each call returns an
// independent shell instance carrying its own line-editing and history state;
// callers that serve concurrent transports (e.g. one shell per SSH session)
// must build a fresh shell per connection, as [console.Shell] is not safe to
// share across concurrent sessions.
func NewShell(opt Options) *console.Shell {
	cmds := []console.Command{
		showCmd(opt),
		powerCmd(opt),
		mdCmd(),
		mwCmd(),
	}
	cmds = append(cmds, opt.Extra...)
	return console.New(console.Config{
		Prompt:   "cairn# ",
		Banner:   "Cairn BMC console",
		Commands: cmds,
	})
}

// Service returns a supervised service function that runs the interactive
// console on UART12. It never returns under normal operation.
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
		Help: "show system — system info (hostname, IP, MAC, uptime)",
		Run: func(args []string) (string, error) {
			if len(args) == 0 || args[0] != "system" && args[0] != "sys" {
				return "usage: show system", nil
			}
			var b strings.Builder
			fmt.Fprintf(&b, "Hostname:    %s\n", opt.Cfg.Hostname())
			fmt.Fprintf(&b, "Mgmt IP:     %s\n", opt.Cfg.MgmtIP())
			mac := opt.Cfg.MgmtMAC()
			fmt.Fprintf(&b, "Mgmt MAC:    %02x:%02x:%02x:%02x:%02x:%02x\n",
				mac[0], mac[1], mac[2], mac[3], mac[4], mac[5])
			fmt.Fprintf(&b, "Chassis:     %s (power %s)\n", opt.Chassis.Model(), opt.Chassis.Power())
			fmt.Fprintf(&b, "Uptime:      %s\n", time.Since(opt.Start).Round(time.Second))
			fmt.Fprintf(&b, "Runtime:     %s/%s %s\n", runtime.GOOS, runtime.GOARCH, runtime.Version())
			return b.String(), nil
		},
	}
}

func powerCmd(opt Options) console.Command {
	return console.Command{
		Name: "power",
		Help: "power on|off|status — host chassis power control",
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
// bring-up tool (register inspection over the console); addresses are used as
// given, so a bad address can fault the system.
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
			var b strings.Builder
			for i := 0; i < words; i++ {
				a := addr + uint64(i)*4
				if i%4 == 0 {
					if i > 0 {
						b.WriteByte('\n')
					}
					fmt.Fprintf(&b, "%09x:", a)
				}
				fmt.Fprintf(&b, " %08x", read32(a))
			}
			return b.String(), nil
		},
	}
}

// mwCmd implements mw <hex-addr> <hex-value>: 32-bit memory/MMIO write.
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
			val, err := parseHex(args[1])
			if err != nil {
				return "", err
			}
			if val > 0xffffffff {
				return "value must fit in 32 bits", nil
			}
			write32(addr, uint32(val))
			return fmt.Sprintf("%09x: %08x", addr, read32(addr)), nil
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

// uartRW adapts the polled UART12 into the blocking io.ReadWriter the console
// engine expects. Read blocks until a byte arrives, yielding to the scheduler
// between polls; Write transmits synchronously.
type uartRW struct{}

func (uartRW) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if c, ok := tboard.UART12.Rx(); ok {
			p[0] = c
			return 1, nil
		}
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
}

func (uartRW) Write(p []byte) (int, error) {
	tboard.UART12.Write(p)
	return len(p), nil
}
