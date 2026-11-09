// Gigabyte MJ11-EC1 BMC hardware target for cairn.
//
// This binary targets the ASPEED AST2500 baseboard management controller of
// the Gigabyte MJ11-EC1 (ARM1176JZ-S), booted by fstart, which trains DRAM
// and enters this ELF. It is console-first: the shared microkernel substrate
// (core/operator supervision, core/telemetry, core/cfgstore + pkg/config,
// pkg/bmcdev) with the interactive serial console. Networking, persistent
// storage and the management plane follow once fstart sets up the MACs.
//
// This package is only meant to be used with `GOOS=tamago GOARCH=arm GOARM=5`
// as supported by the TamaGo framework for bare metal Go, see
// https://github.com/usbarmory/tamago.

//go:build tamago

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"runtime"
	"time"

	// Board import provides the AST2500 bring-up (vectors, timers, console)
	// through the TamaGo board package.
	board "src.kyanite.computer/cairn/target/gigabyte-mj11-ec1-bmc/board"

	// Baseboard-management driver (stub implementation of pkg/bmcdev.Chassis).
	"src.kyanite.computer/cairn/target/gigabyte-mj11-ec1-bmc/chassis"

	// Interactive serial console (core/console engine over UART5).
	sercon "src.kyanite.computer/cairn/target/gigabyte-mj11-ec1-bmc/console"

	// Configuration management (RAM-backed KV store for now).
	"src.kyanite.computer/cairn/pkg/config"
	"src.kyanite.computer/core/cfgstore"

	// slog/telemetry.
	"src.kyanite.computer/core/telemetry"

	// Microkernel assembly: declarative service set supervised together.
	"src.kyanite.computer/core/operator"
)

// cairnMemoryLimit is the soft heap limit applied via the operator: a quarter
// of the 424 MiB the loader leaves usable.
const cairnMemoryLimit = 106 * 1024 * 1024

// mj11Defaults is the MJ11-EC1 BMC factory-default system identity.
var mj11Defaults = config.Defaults{
	Hostname: "mj11-ec1-bmc",
	MgmtIP:   netip.PrefixFrom(netip.AddrFrom4([4]byte{192, 168, 0, 2}), 24),
	MgmtMAC:  [6]byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x02},
}

func main() {
	start := time.Now()

	// Telemetry first, so all subsequent log calls are captured.
	telemetry.Init("cairn")

	printBanner()
	slog.Info("telemetry ready")

	store, err := cfgstore.OpenRAM(2 * 1024 * 1024)
	if err != nil {
		slog.Error("cfg: RAM store unavailable", "err", err)
	}
	cfg := config.New(store, mj11Defaults)
	fmt.Printf("[cfg] %s\n", cfg.SystemSummary())

	chassisDrv := chassis.New(board.Model())
	fmt.Printf("[bmc] chassis driver ready: %s (power %s)\n",
		chassisDrv.Model(), chassisDrv.Power())

	printSystemInfo()

	svcs := []operator.Service{
		operator.PermanentFunc("console", sercon.Service(sercon.Options{
			Cfg:     cfg,
			Chassis: chassisDrv,
			Start:   start,
		})),
	}

	printStatus()
	slog.Info("all services started", "after", time.Since(start).Round(time.Millisecond))

	// Assemble and run the microkernel. The context is never cancelled on
	// hardware, so Run blocks forever.
	opCfg := operator.DefaultConfig()
	opCfg.Name = "cairn"
	opCfg.MemoryLimit = cairnMemoryLimit
	opCfg.Services = svcs
	_ = operator.New(opCfg).Run(context.Background())
}

func printBanner() {
	fmt.Println()
	fmt.Println("Cairn Baseboard Management Controller")
	fmt.Println("Gigabyte MJ11-EC1 BMC (ASPEED AST2500) / TamaGo (arm)")
	fmt.Println("=====================================================")
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
	fmt.Println("  [x] AST2500 bring-up + UART5 console (TamaGo board)")
	fmt.Println("  [x] slog/telemetry           (in-process ring buffer)")
	fmt.Println("  [x] Config manager           (RAM-backed)")
	fmt.Println("  [x] Chassis driver           (stub; hardware pending)")
	fmt.Println("  [ ] Network                  (pending fstart MAC setup)")
	fmt.Println()
}
