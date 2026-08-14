// FTGMAC Ethernet MAC bring-up for the AST2700 DC-SCM board.
//
// Ported from the cmd/nats validation harness (ftgmac_adapter_dcscm.go). It
// enables MAC0 in RGMII mode, brings up the AN8801R PHY over MDIO0, waits for
// link, and initialises the FTGMAC100 DMA engine, returning a netdev.DevEthernet
// for the lneto stack.
//
// The register/PHY sequence here is specific to the AST2750-A1 DC-SCM DDR4
// board. The A2 silicon may require different RGMII delays or PHY handling; if
// so this file gains a build-tagged sibling.

//go:build tamago

package net

import (
	"fmt"
	"time"
	"unsafe"

	"src.kyanite.computer/aspeed-go/hal/an8801r"
	"src.kyanite.computer/aspeed-go/hal/ast2700net"
	"src.kyanite.computer/aspeed-go/hal/ftgmac100"
	"src.kyanite.computer/aspeed-go/hal/mdio"
	"github.com/usbarmory/tamago/soc/aspeed/ast2700"
)

var ftgmacDMA [ftgmac100.DMABytes]byte

// fixedMAC is the locally-administered MAC used during bring-up. The management
// identity MAC (config.Defaults.MgmtMAC) supersedes this once config wiring
// lands.
var fixedMAC = [6]byte{0x02, 0x4b, 0x59, 0x27, 0x00, 0x01}

// NewNIC enables MAC0 RGMII, brings up the AN8801R PHY, waits for link, and
// initialises the FTGMAC100 DMA engine. It returns the NIC (which satisfies
// netdev.DevEthernet) and its hardware address.
func NewNIC() (*ftgmac100.Device, [6]byte, error) {
	ast2700net.EnableMAC0RGMII()

	// Boot diagnostic: report the RGMII MAC interface delays from SCU1
	// mac_delay (0x14C02390): MAC0 TX=bits[5:0], RX=bits[17:12]. These are set
	// by EnableMAC0RGMII; the RGMII reference clock itself is configured by the
	// BootMCU.
	macDelay := *(*uint32)(unsafe.Pointer(uintptr(0x14C02390)))
	chipID1 := *(*uint32)(unsafe.Pointer(uintptr(0x14C02000)))
	fmt.Printf("RGMII: SCU1 mac_delay=0x%08x (MAC0 tx=%d rx=%d) chip_id1=0x%08x rev=%d\n",
		macDelay, macDelay&0x3f, (macDelay>>12)&0x3f, chipID1, (chipID1>>16)&0xff)

	bus := &mdio.Bus{Base: mdio.AST2700MDIO0}
	phy := &an8801r.Device{MDIO: bus, Addr: 0}
	id, err := phy.ID()
	if err != nil {
		return nil, fixedMAC, fmt.Errorf("read phy id: %w", err)
	}
	fmt.Printf("MDIO0: PHY addr=0 id=%08x\n", id)
	if err := phy.InitRGMIIID(); err != nil {
		return nil, fixedMAC, fmt.Errorf("init AN8801R: %w", err)
	}
	if err := phy.InitLEDs(); err != nil {
		return nil, fixedMAC, fmt.Errorf("init AN8801R LEDs: %w", err)
	}
	txd, rxd := phy.ConfiguredRGMIIDelays()
	fmt.Printf("AN8801R: init OK rgmii-id tx_delay=0x%08x rx_delay=0x%08x (PHY-side, write-only)\n", txd, rxd)
	link, err := phy.WaitLink(10 * time.Second)
	if err != nil {
		return nil, fixedMAC, fmt.Errorf("wait phy link: %w", err)
	}
	fmt.Printf("AN8801R: link up %d/%s\n", link.SpeedMbps, duplexString(link.FullDuplex))
	if err := phy.ApplyLinkSpeed(link); err != nil {
		return nil, fixedMAC, fmt.Errorf("apply phy link speed: %w", err)
	}

	dev := &ftgmac100.Device{
		Base: ftgmac100.AST2700MAC0,
		MAC:  fixedMAC,
		Link: ftgmac100.Link{SpeedMbps: link.SpeedMbps, FullDuplex: link.FullDuplex},
		DMA: ftgmac100.DMAOps{
			Clean:      ast2700.ARM.CleanDataCacheRange,
			Invalidate: ast2700.ARM.InvalidateDataCacheRange,
		},
	}
	addr := uint64(uintptr(unsafe.Pointer(&ftgmacDMA[0])))
	if err := dev.Init(addr, ftgmacDMA[:]); err != nil {
		return nil, fixedMAC, err
	}
	return dev, fixedMAC, nil
}

func duplexString(full bool) string {
	if full {
		return "full"
	}
	return "half"
}
