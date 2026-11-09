// Gigabyte MJ11-EC1 BMC board identity for cairn.
//
// Hardware bring-up is the TamaGo board package, imported for its side
// effects below: it installs the runtime hooks (runtime/goos.Hwinit1, Printk,
// RamSize) via //go:linkname. The loader (fstart) has already trained DRAM
// and configured the UART5 console. This package only adds the platform
// identity used by the target's main for banners and inventory.
//
// This package is only meant to be used with GOOS=tamago GOARCH=arm GOARM=5.

//go:build tamago

package board

import (
	_ "github.com/usbarmory/tamago/board/gigabyte/mj11ec1bmc"
	"github.com/usbarmory/tamago/soc/aspeed/ast2500"
)

// Model returns the human-readable board model identifier.
func Model() string { return "Gigabyte MJ11-EC1 BMC" }

// SOC returns the human-readable SoC identifier, with the silicon revision.
func SOC() string { return ast2500.Model() + " (ARM1176JZ-S, ARMv6)" }
