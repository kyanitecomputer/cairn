// Package net brings up standard Go networking on the AST2700 DC-SCM board.
//
// It bridges lneto's userspace TCP/IP stack (github.com/soypat/lneto/x/xnet)
// with TamaGo's net.SocketFunc hook so that stdlib net.Listen / net.Dial work
// over the AST2700 FTGMAC Ethernet MAC. This is the platform-layer network
// bring-up that the shared management plane (core/mgmt) sits on top of; it is
// the AST2700 analog of vein's target/vega/net.
//
// Ported from the cmd/nats validation harness (netdev.go); the NIC bring-up
// itself lives in the build-tagged ftgmac.go.
//
// This package is only compiled for GOOS=tamago.

//go:build tamago

package net

import (
	"context"
	"fmt"
	stdnet "net"
	"net/netip"
	"syscall"
	"time"

	"github.com/soypat/lneto"
	"github.com/soypat/lneto/x/netdev"
	"github.com/soypat/lneto/x/xnet"
)

var (
	nstack  xnet.Netstack
	iface   netdev.Interface[struct{}]
	runner  netdev.Runner[struct{}]
	backoff lneto.BackoffStrategy = func(uint) time.Duration { return 5 * time.Millisecond }
)

// Start performs full network bring-up: it initialises the FTGMAC NIC, resets
// the lneto stack, installs net.SocketFunc, and kicks off the RX/TX runner plus
// a background DHCP loop. It returns the NIC's hardware address. The hostname is
// advertised via DHCP.
func Start(hostname string) ([6]byte, error) {
	nic, mac, err := NewNIC()
	if err != nil {
		return mac, fmt.Errorf("nic init: %w", err)
	}
	// Optional raw-frame probe (built only with the netdiag tag).
	diagnose(nic)
	if err := Init(nic, mac, hostname); err != nil {
		return mac, err
	}
	return mac, nil
}

// Init initialises the lneto networking stack with the given NIC and configures
// net.SocketFunc for standard Go networking. It performs DHCP in the background.
func Init(nic netdev.DevEthernet, mac [6]byte, hostname string) error {
	poolCfg := xnet.TCPPoolConfig{
		PoolSize:           16,
		QueueSize:          8,
		TxBufSize:          4096,
		RxBufSize:          4096,
		NewBackoff:         func() lneto.BackoffStrategy { return backoff },
		NanoTime:           func() int64 { return time.Now().UnixNano() },
		EstablishedTimeout: 30 * time.Second,
		ClosingTimeout:     5 * time.Second,
	}

	err := nstack.Reset(xnet.StackConfig{
		RandSeed:          time.Now().UnixNano() | 1,
		Hostname:          hostname,
		MaxActiveTCPPorts: 64,
		MaxActiveUDPPorts: 16,
		ICMPQueueLimit:    4,
		MTU:               1500,
		HardwareAddress:   mac,
		// PassivePeers enables the egress MAC patcher + passive ARP learning so
		// replies are unicast to the peer's learned MAC instead of falling back
		// to the broadcast gateway address.
		PassivePeers: 8,
	}, backoff, poolCfg)
	if err != nil {
		return fmt.Errorf("stack reset: %w", err)
	}

	err = iface.Init(
		&netlinkStub{},
		nic,
		netdev.InterfaceConfig{
			HardwareAddr6: mac,
		},
	)
	if err != nil {
		return fmt.Errorf("interface init: %w", err)
	}

	nstack.EnableICMP(true)

	// Route all stdlib net.Listen/net.Dial calls through the lneto stack.
	stdnet.SocketFunc = socketFunc
	run(mac)

	return nil
}

func run(mac [6]byte) {
	// Configure the runner before Run. The FTGMAC NIC is poll-driven (see
	// EthPoll usage), so it runs in RunnerInterfacePoll mode with an idle
	// backoff. Without this the runner has a nil backoff strategy and faults on
	// the first idle poll iteration.
	if err := runner.Configure(netdev.RunnerConfig[struct{}]{
		Buffers: iface.RunnerBuffers(4),
		Backoff: backoff,
		Flags:   netdev.RunnerInterfacePoll,
	}); err != nil {
		fmt.Printf("Network runner configure failed: %v\n", err)
		return
	}

	go func() {
		if err := runner.Run(context.Background(), &iface, &nstack); err != nil {
			fmt.Printf("Network runner stopped: %v\n", err)
		}
	}()

	go func() {
		time.Sleep(2 * time.Second)
		for {
			fmt.Println("Network: DHCP attempt starting...")
			dhcpCtx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			addr, gateway, subnetBits, err := nstack.EnableDHCP(dhcpCtx, true, netip.Addr{})
			cancel()
			if err == nil {
				fmt.Printf("Network: DHCP IP=%s/%d gateway=%s\n", addr, subnetBits, gateway)
				return
			}
			fmt.Printf("Network: DHCP failed: %v; retrying in 5s\n", err)
			time.Sleep(5 * time.Second)
		}
	}()
}

// socketFunc implements TamaGo's net.SocketFunc hook, routing all
// net.Listen/net.Dial calls through lneto's TCP/IP stack.
func socketFunc(ctx context.Context, network string, family, sotype int, laddr, raddr stdnet.Addr) (interface{}, error) {
	switch network {
	case "tcp", "tcp4":
	default:
		return nil, fmt.Errorf("unsupported network: %s", network)
	}

	var local, remote netip.AddrPort

	if laddr != nil {
		p, err := netip.ParseAddrPort(laddr.String())
		if err != nil {
			return nil, fmt.Errorf("parse local addr: %w", err)
		}
		local = p
	}
	if raddr != nil {
		p, err := netip.ParseAddrPort(raddr.String())
		if err != nil {
			return nil, fmt.Errorf("parse remote addr: %w", err)
		}
		remote = p
	}

	return nstack.Socket(ctx, network, syscall.AF_INET, sotype, local, remote)
}

// netlinkStub is a no-op netlink for wired Ethernet (always connected).
type netlinkStub struct{}

func (nl *netlinkStub) LinkConnect(struct{}) error { return nil }
func (nl *netlinkStub) LinkDisconnect()            {}
func (nl *netlinkStub) LinkNotify(cb netdev.NotifyCallback[struct{}]) {
	// Immediately signal connected for wired Ethernet.
	cb(true)
}
