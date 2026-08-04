// ASPEED AST2700 DC-SCM EVB hardware target for cairn.
// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.
//
// This binary targets the ASPEED AST2700 DC-SCM evaluation board (dual
// Cortex-A35). It is the first cairn target and is intentionally a structural
// stub: it wires the shared microkernel substrate (core/operator supervision,
// core/telemetry, core/cfgstore + pkg/config, pkg/bmcdev) exactly as vein's
// target/vega does, but performs no hardware management yet. Real sensor/power
// drivers land during hardware enablement; the layering and patterns are what
// this stub establishes.
//
// This package is only meant to be used with `GOOS=tamago GOARCH=arm64` as
// supported by the TamaGo framework for bare metal Go on ARM64 SoCs, see
// https://github.com/usbarmory/tamago.

//go:build tamago

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"runtime"
	"time"

	// Board import provides CA35 hardware bring-up (UART/GIC/timer) and the
	// runtime console hook, entirely via the upstream TamaGo board package.
	board "src.kyanite.computer/cairn/target/ast2700-dcscm-evb/board"

	// Baseboard-management driver (stub implementation of pkg/bmcdev.Chassis).
	"src.kyanite.computer/cairn/target/ast2700-dcscm-evb/chassis"

	// Interactive serial console (core/console engine over UART12).
	sercon "src.kyanite.computer/cairn/target/ast2700-dcscm-evb/console"

	// Hardware-agnostic management contract.
	"src.kyanite.computer/cairn/pkg/bmcdev"

	// Configuration management (Scree-backed KV store).
	"src.kyanite.computer/cairn/pkg/config"
	"src.kyanite.computer/core/cfgstore"

	// Platform network bring-up (FTGMAC + lneto → net.SocketFunc).
	dcscmnet "src.kyanite.computer/cairn/target/ast2700-dcscm-evb/net"

	// Platform storage (Scree over FMC SPI NOR).
	"src.kyanite.computer/cairn/target/ast2700-dcscm-evb/store"

	// Optional DisplayPort console (built only with the ast2700video tag).
	"src.kyanite.computer/cairn/target/ast2700-dcscm-evb/video"

	// slog/telemetry.
	"src.kyanite.computer/core/telemetry"

	// Management plane: embedded NATS + auth callout + client bus.
	"src.kyanite.computer/core/auth"
	"src.kyanite.computer/core/bus"
	"src.kyanite.computer/core/mgmt"
	"src.kyanite.computer/core/natscore"

	// SSH management server (shared core/sshd bridged to the core/console shell).
	"src.kyanite.computer/core/sshd"

	// Microkernel assembly: declarative service set supervised together.
	"src.kyanite.computer/core/operator"
)

// cairnMemoryLimit is the soft heap limit applied via the operator. The DC-SCM
// board has 1 GB DRAM; cap the Go heap to leave headroom for the (future)
// management-plane budget.
const cairnMemoryLimit = 256 * 1024 * 1024

// startTime is set at the start of main() for uptime calculation.
var startTime time.Time

// configStore is the raw KV store.
var configStore *cfgstore.Store

// cfg is the typed config manager (RAM-backed KV; replace with SPI-NOR for
// persistence during hardware enablement).
var cfg *config.Manager

// board2700 is the stub baseboard-management driver.
var board2700 *chassis.Driver

// mgmtPlane is the management plane (in-process NATS + auth callout). nil if
// bootstrap failed, in which case the node still runs locally without the bus.
var mgmtPlane *mgmt.Plane

// storeReady reports whether the flash-backed Scree store was registered; when
// false the management plane falls back to a RAM-backed store.
var storeReady bool

// beatConn is the heartbeat actor's authorized in-process connection. nil if the
// management plane is unavailable.
var beatConn *bus.Conn

// heartbeatSubject is the subject the heartbeat actor is authorized to publish.
var heartbeatSubject string

// ast2700Defaults is the AST2700 DC-SCM factory-default system identity. The
// platform-neutral config package holds no hardware identity; the target
// supplies it here.
var ast2700Defaults = config.Defaults{
	Hostname: "ast2700-dcscm",
	MgmtIP:   netip.PrefixFrom(netip.AddrFrom4([4]byte{192, 168, 0, 2}), 24),
	MgmtMAC:  [6]byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x02},
}

func main() {
	startTime = time.Now()

	// Telemetry first, so all subsequent log calls are captured.
	telemetry.Init("cairn")

	printBanner()
	slog.Info("telemetry ready")

	initConfig()
	initChassis()
	initNet()
	initStore()
	initMgmt()

	// Bring up the DisplayPort console (no-op unless the ast2700video tag is
	// set). Done after DRAM/storage so the GFX scanout grant is in place.
	video.Init()

	printSystemInfo()

	// Assemble the microkernel declaratively: this platform's service set. The
	// operator supervises them all (panic recovery + per-service restart), so
	// no single routine can crash the system. The Services slice is caller-
	// owned; the operator never appends to it.
	svcs := []operator.Service{
		operator.PermanentFunc("heartbeat", heartbeatLoop),
		operator.PermanentFunc("console", sercon.Service(consoleOptions())),
	}

	// SSH management server (port 22), sharing the console command set over an
	// encrypted channel. Non-fatal if it cannot start; serial console remains.
	if svc, ok := initSSH(); ok {
		svcs = append(svcs, svc)
	}

	// The management plane is a supervised child when it came up.
	if mgmtPlane != nil {
		svcs = append(svcs, operator.Permanent(mgmtPlane.Service()))
	}

	printStatus()
	slog.Info("all services started")

	// Assemble and run the microkernel. The context is never cancelled on
	// hardware, so Run blocks forever.
	opCfg := operator.DefaultConfig()
	opCfg.Name = "cairn"
	opCfg.MemoryLimit = cairnMemoryLimit
	opCfg.Services = svcs
	_ = operator.New(opCfg).Run(context.Background())
}

// initConfig opens the config store (RAM-backed for now). On real hardware this
// is replaced by an SPI-NOR-backed cfgstore.Open.
func initConfig() {
	store, err := cfgstore.OpenRAM(2 * 1024 * 1024)
	if err != nil {
		fmt.Printf("[cfg] store open error: %v — using defaults\n", err)
		store, _ = cfgstore.OpenRAM(2 * 1024 * 1024)
	}
	configStore = store
	cfg = config.New(store, ast2700Defaults)
	fmt.Printf("[cfg] %s\n", cfg.SystemSummary())
}

// initChassis wires the stub baseboard-management driver behind pkg/bmcdev.
func initChassis() {
	board2700 = chassis.New(board.Model())
	fmt.Printf("[bmc] chassis driver ready: %s (power %s)\n",
		board2700.Model(), board2700.Power())
}

// initNet brings up the FTGMAC Ethernet interface and installs net.SocketFunc so
// stdlib networking (and, later, the management plane's network listeners) works
// over lneto. Failure is non-fatal: the node keeps running on the in-process
// bus. The NIC bring-up waits briefly for PHY link.
func initNet() {
	mac, err := dcscmnet.Start(cfg.Hostname())
	if err != nil {
		slog.Error("net: bring-up failed — continuing without network", "err", err)
		return
	}
	slog.Info("net: interface up",
		slog.String("mac", fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x",
			mac[0], mac[1], mac[2], mac[3], mac[4], mac[5])))
}

// initStore opens the FMC SPI NOR, formats a Scree volume, and registers it as
// the JetStream backing store. On failure the management plane falls back to a
// RAM-backed store (storeReady stays false).
func initStore() {
	dev, err := store.Open(store.DefaultNORSize)
	if err != nil {
		slog.Error("store: flash volume unavailable — falling back to RAM", "err", err)
		return
	}
	if _, err := natscore.RegisterStore(dev); err != nil {
		slog.Error("store: register failed — falling back to RAM", "err", err)
		return
	}
	storeReady = true
	slog.Info("store: flash-backed Scree registered (FMC SPI NOR)")
}

// initMgmt starts the management plane: an in-process NATS server governed by
// the auth callout, with LOCAL actors authenticated by per-boot NKeys. It is
// the baseline IPC substrate; real management services (sensor telemetry,
// Redfish backend, Scree) become LOCAL actors on it during hardware enablement.
// Failure is non-fatal: the node keeps running locally without the bus.
func initMgmt() {
	device := cfg.Hostname()
	pb := auth.NewPolicy().DenySystem()
	pb.Capability("heartbeat.publish").Publish("kyanite.{domain}.{device}.heartbeat")
	policy, err := pb.Build()
	if err != nil {
		slog.Error("mgmt: policy build failed", "err", err)
		return
	}

	// When the flash-backed Scree store is registered, skip the RAM store;
	// otherwise fall back to a small RAM-backed store.
	var ramStoreSize int64 = 4 * 1024 * 1024
	if storeReady {
		ramStoreSize = 0
	}

	plane, err := mgmt.Start(mgmt.Config{
		Domain:       "local",
		Device:       device,
		Policy:       policy,
		StoreDir:     "/nats",
		RAMStoreSize: ramStoreSize,
	})
	if err != nil {
		slog.Error("mgmt: management plane unavailable", "err", err)
		return
	}
	mgmtPlane = plane

	conn, err := plane.ConnectLocal("heartbeat", "heartbeat.publish")
	if err != nil {
		slog.Error("mgmt: heartbeat actor connect failed", "err", err)
		return
	}
	beatConn = conn
	heartbeatSubject = fmt.Sprintf("kyanite.local.%s.heartbeat", device)
	slog.Info("mgmt: management plane up (in-process NATS + auth callout)")
}

// consoleOptions builds the console command surface shared by the serial
// console and the SSH server, so both expose the identical cairn command set.
func consoleOptions() sercon.Options {
	return sercon.Options{
		Cfg:     cfg,
		Chassis: board2700,
		Start:   startTime,
		Extra:   video.Commands(),
	}
}

// initSSH builds the SSH management server (port 22) and returns it as a
// supervised service. The server generates (or loads) an Ed25519 host key from
// the config store and authenticates with the stored password (default:
// "admin"). Each session gets its own console shell instance over the SSH
// channel. If the server cannot be created it returns ok=false, and SSH is
// disabled while the serial console remains available.
func initSSH() (operator.Service, bool) {
	opt := consoleOptions()
	srv, err := sshd.New(configStore, func(rw io.ReadWriter) {
		// Fresh shell per session: console.Shell carries per-session line and
		// history state and is not safe to share across concurrent sessions.
		sercon.NewShell(opt).Run(rw)
	})
	if err != nil {
		slog.Error("ssh: server init failed — serial console still available", "err", err)
		return operator.Service{}, false
	}
	slog.Info("ssh: management server registered (port 22, Ed25519, password auth)")
	return operator.PermanentFunc("ssh", func(context.Context) error {
		srv.ListenAndServe(22)
		return nil // unreachable on hardware; restart if the listener exits
	}), true
}

// heartbeatLoop is a supervised placeholder service: it periodically polls for
// monitor hotplug and publishes uptime/power over the authorized bus, exercising
// the operator/service and LOCAL auth patterns until real management loops
// replace it. It does not log to the console — the interactive shell is the
// liveness signal — and returns promptly on ctx cancel.
func heartbeatLoop(ctx context.Context) error {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			// Re-acquire the display on monitor hotplug (no-op without video).
			video.PollHotplug()
			// Publish over the authorized bus when the management plane is up,
			// exercising the LOCAL auth path end to end.
			if beatConn != nil {
				var power bmcdev.PowerState
				if board2700 != nil {
					power = board2700.Power()
				}
				uptime := time.Since(startTime).Round(time.Second)
				_ = beatConn.Publish(heartbeatSubject,
					[]byte(fmt.Sprintf("uptime=%s power=%s", uptime, power)))
			}
		}
	}
}

func printBanner() {
	fmt.Println()
	fmt.Println("Cairn Baseboard Management Controller")
	fmt.Println("ASPEED AST2700 DC-SCM / TamaGo (arm64)")
	fmt.Println("=====================================")
	fmt.Println()
}

func printSystemInfo() {
	fmt.Printf("Board:   %s\n", board.Model())
	fmt.Printf("SoC:     %s\n", board.SOC())
	fmt.Printf("Runtime: %s/%s %s\n", runtime.GOOS, runtime.GOARCH, runtime.Version())
	fmt.Println()
}

func printStatus() {
	fmt.Println("Services:")
	fmt.Println("  [x] CA35 bring-up + UART console (upstream board)")
	fmt.Println("  [x] slog/telemetry           (in-process ring buffer)")
	fmt.Println("  [x] Config manager           (RAM-backed; SPI-NOR pending)")
	fmt.Println("  [x] Chassis driver           (stub; hardware pending)")
	fmt.Println("  [x] Network                  (FTGMAC + lneto → net.SocketFunc)")
	if storeReady {
		fmt.Println("  [x] Storage                  (Scree over FMC SPI NOR)")
	} else {
		fmt.Println("  [x] Storage                  (RAM-backed; FMC NOR pending 4-byte addressing)")
	}
	fmt.Println("  [x] Management plane         (in-process NATS + auth callout)")
	fmt.Println("  [x] SSH console              (port 22; shares BMC command set)")
	fmt.Println("  [x] Heartbeat                (supervised; publishes over bus)")
	fmt.Println()
}
