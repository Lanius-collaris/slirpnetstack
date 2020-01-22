package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/fdbased"
	"gvisor.dev/gvisor/pkg/tcpip/link/rawfile"
	"gvisor.dev/gvisor/pkg/tcpip/link/sniffer"
	"gvisor.dev/gvisor/pkg/tcpip/link/tun"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

func GetTunTap(netNsPath string, ifName string) (int, bool, uint32, error) {
	var (
		restore func()
		err     error
	)
	if netNsPath != "" {
		fmt.Fprintf(os.Stderr, "[.] Joininig netns %s\n", netNsPath)
		restore, err = joinNetNS(netNsPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[!] Can't join netns %s: %s\n", netNsPath, err)
			return 0, false, 0, err
		}
	}

	fmt.Fprintf(os.Stderr, "[.] Opening tun interface %s\n", ifName)
	mtu, err := rawfile.GetMTU(ifName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[!] GetMTU(%s) = %s\n", ifName, err)
		return 0, false, 0, err
	}

	tapMode := false

	fd, err := tun.Open(ifName)
	if err != nil {
		tapMode = true
		fd, err = tun.OpenTAP(ifName)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[!] open(%s) = %s\n", ifName, err)
			return 0, false, 0, err
		}
	}

	if netNsPath != "" {
		fmt.Fprintf(os.Stderr, "[.] Restoring root netns\n")
		restore()
	}

	return fd, tapMode, mtu, nil
}

func NewStack() *stack.Stack {
	// Create the stack with ipv4 and tcp protocols, then add a tun-based
	// NIC and ipv4 address.
	opts := stack.Options{
		NetworkProtocols: []stack.NetworkProtocol{
			ipv4.NewProtocol(),
			ipv6.NewProtocol(),
			arp.NewProtocol()},
		TransportProtocols: []stack.TransportProtocol{
			tcp.NewProtocol(),
			udp.NewProtocol(),
			icmp.NewProtocol4(),
			icmp.NewProtocol6()},
		HandleLocal: false,
	}

	s := stack.New(opts)
	s.SetForwarding(true)
	s.SetTransportProtocolOption(tcp.ProtocolNumber, tcp.SACKEnabled(true))
	s.SetNetworkProtocolOption(ipv4.ProtocolNumber, tcpip.DefaultTTLOption(64))
	s.SetNetworkProtocolOption(ipv6.ProtocolNumber, tcpip.DefaultTTLOption(64))
	s.SetTransportProtocolOption(tcp.ProtocolNumber, tcpip.ModerateReceiveBufferOption(true))

	// We expect no packet loss, therefore we can bump
	// buffers. Too large buffers thrash cache, so there is a
	// wrong value, benchmark required.
	s.SetTransportProtocolOption(tcp.ProtocolNumber, tcp.ReceiveBufferSizeOption{1, 4 * tcp.DefaultReceiveBufferSize, 8 * tcp.DefaultReceiveBufferSize})
	s.SetTransportProtocolOption(tcp.ProtocolNumber, tcp.SendBufferSizeOption{1, 4 * tcp.DefaultSendBufferSize, 8 * tcp.DefaultSendBufferSize})
	return s
}

func AddTunTap(s *stack.Stack, nic tcpip.NICID, tunFd int, tapMode bool, macAddress net.HardwareAddr, tapMtu uint32) error {
	parms := fdbased.Options{FDs: []int{tunFd},
		MTU: tapMtu,
	}
	if tapMode {
		parms.EthernetHeader = true
		parms.Address = tcpip.LinkAddress(macAddress)
	}

	linkEP, err := fdbased.New(&parms)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[!] fdbased.New(%s) = %s\n", ifName, err)
		return err
	}
	if err := s.CreateNIC(nic, sniffer.New(linkEP)); err != nil {
		fmt.Fprintf(os.Stderr, "[!] CreateNIC(%s) = %s\n", ifName, err)
		return fmt.Errorf("%s", err)
	}

	s.SetSpoofing(nic, true)

	// Assign L2 and L3 addresses
	s.AddAddress(nic, arp.ProtocolNumber, arp.ProtocolAddress)

	s.AddAddressRange(nic, ipv4.ProtocolNumber, header.IPv4EmptySubnet)
	s.AddAddressRange(nic, ipv6.ProtocolNumber, header.IPv6EmptySubnet)

	return nil
}

func MustSubnet(ipNet *net.IPNet) *tcpip.Subnet {
	subnet, errx := tcpip.NewSubnet(tcpip.Address(ipNet.IP), tcpip.AddressMask(ipNet.Mask))
	if errx != nil {
		panic(fmt.Sprintf("Unable to MustSubnet(%s): %s", ipNet, errx))
	}
	return &subnet
}

func StackRoutingSetup(s *stack.Stack, nic tcpip.NICID, assignNet string) {
	ipAddr, ipNet, err := net.ParseCIDR(assignNet)
	if err != nil {
		panic(fmt.Sprintf("Unable to ParseCIDR(%s): %s", assignNet, err))
	}

	if ipAddr.To4() != nil {
		s.AddAddress(nic, ipv4.ProtocolNumber, tcpip.Address(ipAddr.To4()))
	} else {
		s.AddAddress(nic, ipv6.ProtocolNumber, tcpip.Address(ipAddr))
	}

	rt := s.GetRouteTable()
	rt = append(rt, tcpip.Route{
		Destination: *MustSubnet(ipNet),
		NIC:         nic,
	})
	s.SetRouteTable(rt)
}

func StackPrimeArp(s *stack.Stack, nic tcpip.NICID, ip net.IP) {
	// Prime the arp cache. Otherwise we get "no remote link
	// address" on first write.
	if ip.To4() != nil {
		s.GetLinkAddress(nic,
			tcpip.Address(ip.To4()),
			"",
			ipv4.ProtocolNumber,
			nil)
	}

}

func GonetDialTCP(s *stack.Stack, laddr, raddr *tcpip.FullAddress, network tcpip.NetworkProtocolNumber) (*GonetTCPConn, error) {
	// Create TCP endpoint, then connect.
	var wq waiter.Queue
	ep, err := s.NewEndpoint(tcp.ProtocolNumber, network, &wq)
	if err != nil {
		return nil, errors.New(err.String())
	}

	if laddr != nil {
		if err := ep.Bind(*laddr); err != nil {
			ep.Close()
			return nil, errors.New(err.String())
		}
	}

	// Create wait queue entry that notifies a channel.
	//
	// We do this unconditionally as Connect will always return an error.
	waitEntry, notifyCh := waiter.NewChannelEntry(nil)
	wq.EventRegister(&waitEntry, waiter.EventOut)
	defer wq.EventUnregister(&waitEntry)

	err = ep.Connect(*raddr)
	if err == tcpip.ErrConnectStarted {
		select {
		case <-notifyCh:
		}

		err = ep.GetSockOpt(tcpip.ErrorOption{})
	}
	if err != nil {
		ep.Close()
		return nil, errors.New(err.String())
	}

	return &GonetTCPConn{gonet.NewConn(&wq, ep), ep}, nil
}

type GonetTCPConn struct {
	net.Conn
	ep tcpip.Endpoint
}

func (c *GonetTCPConn) SetTimeouts(kaInterval time.Duration, kaCount int) error {
	c.ep.SetSockOpt(tcpip.KeepaliveEnabledOption(1))
	c.ep.SetSockOpt(tcpip.KeepaliveIdleOption(kaInterval))
	c.ep.SetSockOpt(tcpip.KeepaliveIntervalOption(kaInterval))
	c.ep.SetSockOpt(tcpip.KeepaliveCountOption(kaCount))
	ut := UserTimeoutFromKeepalive(kaInterval, kaCount)
	c.ep.SetSockOpt(tcpip.TCPUserTimeoutOption(ut))
	return nil
}

func networkProtocolNumberFromIP(ip net.IP) tcpip.NetworkProtocolNumber {
	var nn tcpip.NetworkProtocolNumber
	switch {
	case ip == nil:
	case ip.To4() != nil:
		nn = ipv4.ProtocolNumber
	case ip.To16() != nil:
		nn = ipv6.ProtocolNumber
	}
	return nn
}

func GonetDialUDP(s *stack.Stack, laddr, raddr *tcpip.FullAddress, network tcpip.NetworkProtocolNumber) (*KaUDPConn, error) {

	c, err := gonet.DialUDP(
		s,
		laddr,
		raddr,
		network)
	if err != nil {
		return nil, err
	}
	return &KaUDPConn{Conn: c}, nil
}

func GonetDial(s *stack.Stack, laddr, raddr net.Addr) (KaConn, error) {
	switch raddr.Network() {
	case "tcp":
		return GonetDialTCP(s,
			FullAddressFromAddr(laddr),
			FullAddressFromAddr(raddr),
			networkProtocolNumberFromIP(netAddrIP(raddr)),
		)
	case "udp":
		return GonetDialUDP(s,
			FullAddressFromAddr(laddr),
			FullAddressFromAddr(raddr),
			networkProtocolNumberFromIP(netAddrIP(raddr)),
		)
	}
	return nil, nil
}
