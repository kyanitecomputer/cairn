// Raw-frame network diagnostic, ported from the cmd/nats validation harness
// (netdiag.go). It probes the FTGMAC for received traffic before the lneto stack
// takes ownership of the NIC, which is useful when bringing up a new board/cable
// but competes with the stack's own polling — so it is compiled only with the
// `netdiag` build tag.

//go:build tamago && netdiag

package net

import (
	"encoding/binary"
	"fmt"
	"time"

	"github.com/kyanitecomputer/aspeed-go/hal/ftgmac100"
	"github.com/kyanitecomputer/aspeed-go/reg"
)

func diagnose(nic *ftgmac100.Device) {
	fmt.Println("Network: probing traffic for 5 seconds...")
	isr := reg.Read(nic.Base + 0x00)
	maccr := reg.Read(nic.Base + 0x50)
	fmt.Printf("Network: ISR=%08x MACCR=%08x\n", isr, maccr)
	reg.Write(nic.Base+0x00, isr)

	buf := make([]byte, 1600)
	var total, ipv4, arp, ipv6, vlan, lldp, other int
	var firstFew [5][20]byte
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, n, err := nic.EthPoll(buf)
		if err != nil {
			fmt.Printf("Network: probe err: %v\n", err)
			continue
		}
		if n == 0 {
			time.Sleep(time.Millisecond)
			continue
		}
		if n < 14 {
			total++
			other++
			continue
		}
		total++
		if total <= len(firstFew) {
			copy(firstFew[total-1][:], buf[:min(n, 20)])
		}
		etherType := binary.BigEndian.Uint16(buf[12:14])
		switch etherType {
		case 0x0800:
			ipv4++
		case 0x0806:
			arp++
		case 0x86DD:
			ipv6++
		case 0x8100:
			vlan++
		case 0x88CC:
			lldp++
		default:
			other++
		}
	}
	fmt.Printf("Network: probe done: %d frames (IPv4=%d ARP=%d IPv6=%d VLAN=%d LLDP=%d other=%d)\n",
		total, ipv4, arp, ipv6, vlan, lldp, other)
	for i := 0; i < min(total, len(firstFew)); i++ {
		f := firstFew[i]
		fmt.Printf("  frame[%d] dst=%02x:%02x:%02x:%02x:%02x:%02x src=%02x:%02x:%02x:%02x:%02x:%02x type=%04x\n",
			i, f[0], f[1], f[2], f[3], f[4], f[5],
			f[6], f[7], f[8], f[9], f[10], f[11],
			binary.BigEndian.Uint16(f[12:14]))
	}
	if total == 0 {
		fmt.Println("Network: WARNING no frames received - check cable/switch port")
	}
}
