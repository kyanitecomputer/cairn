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

	"github.com/kyanitecomputer/aspeed-go/hal/usb/ehci"
	"github.com/kyanitecomputer/aspeed-go/hal/usb/vhub"
	tboard "github.com/usbarmory/tamago/board/aspeed/ast2700dcscm"
	"src.kyanite.computer/core/cfgstore"
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

	// ConfigStore is the persistent Scree config volume, if available. When
	// set, the `ls` command lists its stored files (config keys + TLS cert).
	ConfigStore *cfgstore.Store

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
		lsCmd(opt),
		mdCmd(),
		mwCmd(),
		usbCmd(),
		ehciCmd(),
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

// lsCmd lists the files stored in the persistent Scree config volume (config
// keys and the web TLS certificate/key), with size and a content-kind hint —
// an `ls`-style view of what lives in the flash filesystem.
func lsCmd(opt Options) console.Command {
	return console.Command{
		Name: "ls",
		Help: "ls — list files in the persistent Scree config store (name, size, kind)",
		Run: func(_ []string) (string, error) {
			if opt.ConfigStore == nil {
				return "no persistent config store (running on RAM fallback)", nil
			}
			keys, err := opt.ConfigStore.Keys()
			if err != nil {
				return "", err
			}
			if len(keys) == 0 {
				return "config store is empty", nil
			}
			var b strings.Builder
			fmt.Fprintf(&b, "%-24s %8s  %s\n", "NAME", "SIZE", "KIND")
			total := 0
			for _, k := range keys {
				v, err := opt.ConfigStore.Get(k)
				if err != nil {
					fmt.Fprintf(&b, "%-24s %8s  %s\n", k, "?", "err")
					continue
				}
				total += len(v)
				fmt.Fprintf(&b, "%-24s %8d  %s\n", k, len(v), contentKind(v))
			}
			fmt.Fprintf(&b, "%d file(s), %d bytes\n", len(keys), total)
			return b.String(), nil
		},
	}
}

// contentKind classifies a stored value for the ls listing: "pem" for a
// PEM-encoded blob, "text" for otherwise-printable data, else "binary".
func contentKind(v []byte) string {
	if strings.HasPrefix(string(v), "-----BEGIN") {
		return "pem"
	}
	for _, c := range v {
		if c != '\t' && c != '\n' && c != '\r' && (c < 0x20 || c > 0x7e) {
			return "binary"
		}
	}
	return "text"
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

// usbCmd exercises the AST2700 vHub gadget controller bring-up (hal/usb/vhub):
// inspect the SCU + controller registers, run a step-by-step diagnostic
// bring-up that verifies every clock/reset/access-control write, connect to the
// host, and detach. This is the USB equivalent of md/mw — a hardware bring-up
// probe with heavy instrumentation so clock/reset/mux problems surface early.
//
// Subcommands: `status` (read-only snapshot), `diag` (full instrumented
// bring-up + connect + bus poll + anomaly summary), `up` (bring-up + connect +
// poll), `down` (disconnect). `diag`/`up`/`down` mutate the controller and its
// SCU clock/reset/mux. Default target is the DC-SCM port-A gadget (vhuba0);
// pass `b0` for vhubb0.
func usbCmd() console.Command {
	return console.Command{
		Name: "usb",
		Help: "usb status|diag|up|down [a1|b1|a0|b0] — vHub gadget bring-up probe (default a1, host-facing)",
		Complete: func(prev []string, _ string) []string {
			switch len(prev) {
			case 0:
				return []string{"status", "diag", "up", "down"}
			case 1:
				return []string{"a1", "b1", "a0", "b0"}
			}
			return nil
		},
		Run: func(args []string) (string, error) {
			if len(args) < 1 {
				return "usage: usb status|diag|up|down [a1|b1|a0|b0]", nil
			}
			// a1/b1 are the host-facing vHub1 gadgets (routed to the physical
			// PHY); a0/b0 are the internal EHCI-companion vHubs.
			port := vhub.VHubA1
			if len(args) >= 2 {
				switch args[1] {
				case "a1":
					port = vhub.VHubA1
				case "b1":
					port = vhub.VHubB1
				case "a0":
					port = vhub.VHubA0
				case "b0":
					port = vhub.VHubB0
				default:
					return "port must be a1, b1, a0 or b0", nil
				}
			}
			c := vhub.New(port)

			switch args[0] {
			case "status":
				var b strings.Builder
				usbPortInfo(&b, port)
				usbState(&b, c, port)
				return b.String(), nil

			case "diag", "up":
				var b strings.Builder
				usbPortInfo(&b, port)

				if args[0] == "diag" {
					b.WriteString("\n--- pre-bring-up state (as left by boot firmware) ---\n")
					usbState(&b, c, port)
				}

				b.WriteString("\n--- bring-up steps (each write verified by read-back) ---\n")
				steps := c.InitSteps()
				anomalies := usbSteps(&b, steps)

				b.WriteString("\n--- upstream connect + 2s bus-event poll ---\n")
				c.Connect()
				seen := c.PollBusEvents(2 * time.Second)
				usbBusEvents(&b, seen)

				if args[0] == "diag" {
					b.WriteString("\n--- post-connect state ---\n")
					usbState(&b, c, port)
					b.WriteString("\n--- summary ---\n")
					usbSummary(&b, c.Status(), port, steps, seen, anomalies)
				}
				return b.String(), nil

			case "down":
				c.Disconnect()
				return fmt.Sprintf("%s: upstream disconnect asserted (CTRL now %#08x)",
					port.Name, c.Status().Ctrl), nil

			default:
				return "usage: usb status|diag|up|down [a0|b0]", nil
			}
		},
	}
}

// usbPortInfo prints the static port wiring (bases and the SCU bits the driver
// will touch), so a wrong constant is obvious before anything is poked.
func usbPortInfo(b *strings.Builder, p vhub.Port) {
	fmt.Fprintf(b, "vHub %s\n", p.Name)
	fmt.Fprintf(b, "  ctrl base   = %#010x   irq (GIC SPI) = %d\n", p.Base, p.IRQ)
	fmt.Fprintf(b, "  scu base    = %#010x   io-die = %v\n", p.SCUBase, p.IODie)
	fmt.Fprintf(b, "  clock bit   = %#010x   (SCU_CLK_STOP; 0 = running)\n", p.ClockBit)
	fmt.Fprintf(b, "  reset bit   = %#010x   (SCU_RST_CTRL2@0x220, controller)\n", p.ResetBit)
	fmt.Fprintf(b, "  phy reset   = %#010x   phy base = %#010x (shared USB2 PHY)\n", p.PHYResetBit, p.PHYBase)
	fmt.Fprintf(b, "  func mux    = off %#05x mask %#010x device-val %#010x\n", p.FuncMux, p.FuncMask, p.FuncBits)
}

// usbState prints a fully decoded snapshot of the controller and its SCU
// clock/reset/mux, so missing clocks / wrong access-control / wrong mux are
// immediately visible.
func usbState(b *strings.Builder, c *vhub.Controller, port vhub.Port) {
	s := c.Status()
	mux, muxMasked := port.MuxMode(s.SCUFuncMux)

	fmt.Fprintf(b, "  SCU clkstop = %#010x   clock_running=%v\n", s.SCUClkStop, s.ClockRunning(port))
	fmt.Fprintf(b, "  SCU reset   = %#010x   in_reset=%v\n", s.SCUReset, s.InReset(port))
	fmt.Fprintf(b, "  SCU funcmux = %#010x   mode=%s (masked %#010x)\n", s.SCUFuncMux, mux, muxMasked)
	fmt.Fprintf(b, "  CTRL        = %#010x   phy_up=%v connected=%v %s\n", s.Ctrl, s.PHYUp(), s.Connected(), names(vhub.DecodeCtrl(s.Ctrl)))
	fmt.Fprintf(b, "  CONF        = %#010x   dev_addr=%d\n", s.Conf, s.Conf&0x7f)
	fmt.Fprintf(b, "  USBSTS      = %#010x   hispeed=%v frame=%d\n", s.USBSTS, s.HighSpeed(), s.FrameNumber())
	fmt.Fprintf(b, "  IER / ISR   = %#010x / %#010x %s\n", s.IER, s.ISR, names(vhub.DecodeEvents(s.ISR)))
	fmt.Fprintf(b, "  EP0 / EP1   = %#010x / %#010x\n", s.EP0Ctrl, s.EP1Ctrl)
	fmt.Fprintf(b, "  PHY_CTRL    = %#010x   %s\n", s.PHYCtrl, names(vhub.DecodePHY(s.PHYCtrl)))
	if s.HasPHY {
		fmt.Fprintf(b, "  USB2 PHY    = STS2 %#010x (clk60=%v) STS3 %#010x (preemph2=%v)\n",
			s.PHYCtlSts2, s.PHYCtlSts2&(0x3<<26) == (0x3<<26),
			s.PHYCtlSts3, s.PHYCtlSts3&(0x3<<21) == (0x2<<21))
	}

	if s.Ctrl == 0xffffffff {
		b.WriteString("  !! CTRL reads all-ones: controller not clocked or not mapped\n")
	}
}

// usbSteps prints each recorded bring-up step with a pass/fail marker and the
// before->after values, returning the number of failed (mismatched) steps.
func usbSteps(b *strings.Builder, steps []vhub.Step) int {
	fails := 0
	for _, s := range steps {
		if s.RegName == "" { // pure delay / informational
			fmt.Fprintf(b, "  ..   %s\n", s.Name)
			continue
		}
		mark := "  ok "
		if s.Checked() && !s.OK() {
			mark = "  !! "
			fails++
		}
		fmt.Fprintf(b, "%s %-38s %s@%#010x  %08x -> %08x", mark, s.Name, s.RegName, s.Addr, s.Before, s.After)
		if s.Checked() {
			fmt.Fprintf(b, "  (want set %08x clr %08x)", s.WantSet, s.WantClr)
		}
		b.WriteByte('\n')
		if s.Note != "" {
			fmt.Fprintf(b, "         %s\n", s.Note)
		}
	}
	return fails
}

// usbBusEvents prints the ISR bits observed during the poll and interprets the
// key signal (BUS_RESET = the host enumerated and saw the device).
func usbBusEvents(b *strings.Builder, seen uint32) {
	fmt.Fprintf(b, "  observed ISR bits: %#010x %s\n", seen, names(vhub.DecodeEvents(seen)))
	switch {
	case seen&(1<<6) != 0: // BUS_RESET
		b.WriteString("  => BUS_RESET seen: the port is wired and the host sees the device\n")
	case seen != 0:
		b.WriteString("  => bus activity seen but no reset; host may still be settling\n")
	default:
		b.WriteString("  => no bus activity: check cable/host, port routing, PHY, or mux\n")
	}
}

// usbSummary emits a short pass/fail verdict flagging the classic bring-up
// failure modes (SCU locked, clock gated, reset stuck, wrong mux, PHY down).
func usbSummary(b *strings.Builder, s vhub.Status, port vhub.Port, steps []vhub.Step, seen uint32, stepFails int) {
	warn := func(cond bool, msg string) {
		if cond {
			fmt.Fprintf(b, "  !! %s\n", msg)
		}
	}
	mux, _ := port.MuxMode(s.SCUFuncMux)
	warn(stepFails > 0, fmt.Sprintf("%d bring-up step(s) failed read-back verification (see !! above)", stepFails))
	warn(!s.ClockRunning(port), "port clock still gated: SCU write blocked (locked?) or wrong clock bit")
	warn(s.InReset(port), "port still in reset: SCU write blocked or wrong reset bit")
	warn(mux != "device", "port function mux is not device mode: gadget will not attach")
	warn(!s.PHYUp(), "core PHY bits not set (CTRL writes dropped): USB2 PHY not clocked — check the PHY reset (PORTx_VHUB) and PHY tuning")
	warn(s.Ctrl == 0xffffffff, "controller unreachable (CTRL all-ones)")
	warn(!s.Connected(), "upstream not connected (pull-up not asserted)")
	warn(seen == 0, "no bus events observed during poll")
	if stepFails == 0 && s.ClockRunning(port) && !s.InReset(port) && mux == "device" &&
		s.PHYUp() && s.Connected() && seen != 0 {
		b.WriteString("  OK: clocks/reset/mux/PHY all good and bus activity seen\n")
	}
}

// ehciCmd exercises the AST2700 EHCI USB-2.0 *host* controller bring-up
// (hal/usb/ehci) so a device plugged into the board's physical USB port can be
// detected. It is the host-side counterpart of the `usb` (gadget) command: same
// instrumented, read-back-verified bring-up trace, but driving a standard EHCI
// operational register set behind the SCU clock/reset/routing wrapper.
//
// The default (and, for now, only) target is die1 Port D (`ehci3`), which on
// the DC-SCM is wired to the card's physical USB Type-A receptacle; a SuperSpeed
// stick there appears here at high speed.
//
// Subcommands: `status` (read-only snapshot), `diag` (full instrumented
// bring-up + port reset if a device is present + speed report + summary),
// `reset` (drive a root-port reset and report the resulting speed). `diag`/
// `reset` mutate the controller and its SCU clock/reset/route.
func ehciCmd() console.Command {
	return console.Command{
		Name: "ehci",
		Help: "ehci status|diag|reset [d] — EHCI USB2 host bring-up probe (default Port D, physical port)",
		Complete: func(prev []string, _ string) []string {
			switch len(prev) {
			case 0:
				return []string{"status", "diag", "reset"}
			case 1:
				return []string{"d"}
			}
			return nil
		},
		Run: func(args []string) (string, error) {
			if len(args) < 1 {
				return "usage: ehci status|diag|reset [d]", nil
			}
			port := ehci.EHCI3PortD
			if len(args) >= 2 && args[1] != "d" {
				return "port must be d (ehci3, the only wired host port)", nil
			}
			c := ehci.New(port)

			switch args[0] {
			case "status":
				var b strings.Builder
				ehciPortInfo(&b, port)
				ehciState(&b, c, port)
				return b.String(), nil

			case "diag":
				var b strings.Builder
				ehciPortInfo(&b, port)

				b.WriteString("\n--- pre-bring-up state (as left by boot firmware) ---\n")
				ehciState(&b, c, port)

				b.WriteString("\n--- bring-up steps (each write verified by read-back) ---\n")
				steps := c.InitSteps()
				anomalies := ehciSteps(&b, steps)

				b.WriteString("\n--- post-bring-up state ---\n")
				st := c.Status()
				ehciState(&b, c, port)

				b.WriteString("\n--- poll root port for a device (up to 5s) ---\n")
				portsc, connected, waited := c.PollConnect(5 * time.Second)
				fmt.Fprintf(&b, "  PORTSC %#010x connected=%v after %v %s\n",
					portsc, connected, waited.Round(time.Millisecond), names2(ehci.DecodePortSC(portsc)))
				st = c.Status()

				if connected {
					b.WriteString("\n--- device present: driving root-port reset ---\n")
					before, after := c.ResetPort()
					fmt.Fprintf(&b, "  PORTSC %#010x -> %#010x\n", before, after)
					st = c.Status()
					fmt.Fprintf(&b, "  speed after reset: %s\n", st.Speed())
				}

				b.WriteString("\n--- summary ---\n")
				ehciSummary(&b, st, port, anomalies)
				return b.String(), nil

			case "reset":
				var b strings.Builder
				portsc, connected, waited := c.PollConnect(5 * time.Second)
				fmt.Fprintf(&b, "%s: waited %v for connect, PORTSC %#010x connected=%v\n",
					port.Name, waited.Round(time.Millisecond), portsc, connected)
				if !connected {
					b.WriteString("no device connected — nothing to reset\n")
					return b.String(), nil
				}
				before, after := c.ResetPort()
				fmt.Fprintf(&b, "root-port reset PORTSC %#010x -> %#010x, speed=%s\n",
					before, after, c.Status().Speed())
				return b.String(), nil

			default:
				return "usage: ehci status|diag|reset [d]", nil
			}
		},
	}
}

// ehciPortInfo prints the static host-port wiring (bases and the SCU bits the
// driver will touch), so a wrong constant is obvious before anything is poked.
func ehciPortInfo(b *strings.Builder, p ehci.Port) {
	fmt.Fprintf(b, "EHCI %s (USB2 host)\n", p.Name)
	fmt.Fprintf(b, "  ctrl base   = %#010x   irq = %d\n", p.Base, p.IRQ)
	fmt.Fprintf(b, "  scu base    = %#010x\n", p.SCUBase)
	fmt.Fprintf(b, "  clkstop reg = %#05x       clock bit = %#010x (0 = running)\n", p.ClkStopReg, p.ClockBit)
	fmt.Fprintf(b, "  reset bit   = %#010x   (SCU_RST_CTRL2@0x220)\n", p.ResetBit)
	fmt.Fprintf(b, "  func route  = off %#05x mask %#010x host-val %#010x\n", p.FuncMux, p.FuncMask, p.FuncBits)
	fmt.Fprintf(b, "  phy base    = %#010x   (shared Port D USB2 PHY)\n", p.PHYBase)
}

// ehciState prints a fully decoded snapshot of the controller and its SCU
// clock/reset/route so missing clocks / wrong routing / a disconnected port are
// immediately visible.
func ehciState(b *strings.Builder, c *ehci.Controller, port ehci.Port) {
	s := c.Status()
	mux, muxMasked := port.MuxMode(s.SCUFuncMux)

	fmt.Fprintf(b, "  SCU clkstop = %#010x   clock_running=%v\n", s.SCUClkStop, s.ClockRunning(port))
	fmt.Fprintf(b, "  SCU reset   = %#010x   in_reset=%v\n", s.SCUReset, s.InReset(port))
	fmt.Fprintf(b, "  SCU route   = %#010x   mode=%s (masked %#010x)\n", s.SCUFuncMux, mux, muxMasked)
	fmt.Fprintf(b, "  CAPLENGTH   = %#04x         HCIVERSION=%#06x nports=%d ppc=%v\n",
		s.CapLength, s.HCIVersion, s.NPorts(), s.PortPowerControl())
	fmt.Fprintf(b, "  USBCMD      = %#010x   running=%v %s\n", s.USBCmd, s.Running(), names2(ehci.DecodeCmd(s.USBCmd)))
	fmt.Fprintf(b, "  USBSTS      = %#010x   halted=%v %s\n", s.USBSts, s.Halted(), names2(ehci.DecodeSts(s.USBSts)))
	fmt.Fprintf(b, "  CONFIGFLAG  = %#010x   configured=%v\n", s.ConfigF, s.Configured())
	fmt.Fprintf(b, "  PORTSC      = %#010x   connected=%v enabled=%v powered=%v %s\n",
		s.PortSC, s.DeviceConnected(), s.PortEnabled(), s.PortPowered(), names2(ehci.DecodePortSC(s.PortSC)))
	fmt.Fprintf(b, "                             speed=%s companion_owned=%v\n", s.Speed(), s.OwnedByCompanion())
	if s.HasPHY {
		fmt.Fprintf(b, "  USB2 PHY    = STS2 %#010x (clk60=%v) STS3 %#010x (preemph2=%v)\n",
			s.PHYCtlSts2, s.PHYCtlSts2&(0x3<<26) == (0x3<<26),
			s.PHYCtlSts3, s.PHYCtlSts3&(0x3<<21) == (0x2<<21))
	}
	if s.CapLength == 0xff {
		b.WriteString("  !! CAPLENGTH reads 0xff: controller not clocked or not mapped\n")
	}
}

// ehciSteps prints each bring-up step with a pass/fail marker and before->after
// values, returning the number of failed (mismatched) steps.
func ehciSteps(b *strings.Builder, steps []ehci.Step) int {
	fails := 0
	for _, s := range steps {
		if s.RegName == "" { // pure delay / informational
			fmt.Fprintf(b, "  ..   %s\n", s.Name)
			continue
		}
		mark := "  ok "
		if s.Checked() && !s.OK() {
			mark = "  !! "
			fails++
		}
		fmt.Fprintf(b, "%s %-38s %s@%#010x  %08x -> %08x", mark, s.Name, s.RegName, s.Addr, s.Before, s.After)
		if s.Checked() {
			fmt.Fprintf(b, "  (want set %08x clr %08x)", s.WantSet, s.WantClr)
		}
		b.WriteByte('\n')
		if s.Note != "" {
			fmt.Fprintf(b, "         %s\n", s.Note)
		}
	}
	return fails
}

// ehciSummary emits a short pass/fail verdict flagging the classic host bring-up
// failure modes (SCU locked, clock gated, reset stuck, wrong routing, HC not
// responding) and reports whether a device was detected.
func ehciSummary(b *strings.Builder, s ehci.Status, port ehci.Port, stepFails int) {
	warn := func(cond bool, msg string) {
		if cond {
			fmt.Fprintf(b, "  !! %s\n", msg)
		}
	}
	mux, _ := port.MuxMode(s.SCUFuncMux)
	warn(stepFails > 0, fmt.Sprintf("%d bring-up step(s) failed read-back verification (see !! above)", stepFails))
	warn(!s.ClockRunning(port), "port clock still gated: SCU write blocked (locked?) or wrong clock bit")
	warn(s.InReset(port), "port still in reset: SCU write blocked or wrong reset bit")
	warn(mux != "host", "port not routed to the EHCI host: device will not appear")
	warn(s.CapLength == 0xff, "controller unreachable (CAPLENGTH 0xff): core unclocked or PHY down")
	warn(!s.Configured(), "CONFIGFLAG not set: ports not handed to EHCI")

	switch {
	case s.CapLength == 0 || s.CapLength == 0xff:
		b.WriteString("  !! host controller not reachable — check clock/reset/route above\n")
	case s.DeviceConnected() && s.PortEnabled():
		fmt.Fprintf(b, "  OK: host up, device connected and enabled at %s\n", s.Speed())
	case s.DeviceConnected() && s.OwnedByCompanion():
		b.WriteString("  device connected but low/full-speed: released to UHCI companion (not yet driven)\n")
	case s.DeviceConnected():
		fmt.Fprintf(b, "  device connected (%s) but port not enabled — reset may need a retry\n", s.Speed())
	default:
		b.WriteString("  host up, no device connected — plug a stick into the card's USB port and re-run\n")
	}
}

// names2 joins decoded EHCI event names into a bracketed list, or "" if empty.
func names2(evs []ehci.Event) string {
	if len(evs) == 0 {
		return ""
	}
	parts := make([]string, len(evs))
	for i, e := range evs {
		parts[i] = e.Name
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// names joins decoded event/bit names into a bracketed list, or "" if empty.
func names(evs []vhub.Event) string {
	if len(evs) == 0 {
		return ""
	}
	parts := make([]string, len(evs))
	for i, e := range evs {
		parts[i] = e.Name
	}
	return "[" + strings.Join(parts, " ") + "]"
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
