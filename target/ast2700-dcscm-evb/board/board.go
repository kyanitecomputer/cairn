// ASPEED AST2700 DC-SCM EVB board identity for cairn.
//
// Hardware bring-up (DRAM is prepared by the BootMCU; this handles CA35-side
// UART/GIC/timer init) is provided entirely by the upstream TamaGo board
// package, imported for its side effects below. It installs the runtime hooks
// (runtime/goos.Hwinit1, Printk, RamStart/RamSize) via //go:linkname, so simply
// importing it makes the console and scheduler work. cairn contains no
// hardware-init code of its own; this package only re-exports the platform
// identity used by the target's main for banners and inventory.
//
// This package is only meant to be used with GOOS=tamago GOARCH=arm64.

//go:build tamago

package board

// Hardware bring-up on import: the upstream board package wires the runtime
// console and memory layout for the AST2700 DC-SCM via //go:linkname.
import _ "github.com/usbarmory/tamago/board/aspeed/ast2700dcscm"

// Model returns the human-readable board model identifier.
func Model() string { return "ASPEED AST2700 DC-SCM EVB (AST2750-A1 DDR4)" }

// SOC returns the human-readable SoC identifier.
func SOC() string { return "ASPEED AST2700 (dual Cortex-A35 @ ARMv8-A)" }
