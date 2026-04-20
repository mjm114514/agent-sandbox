package netstack

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"sync"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const (
	nicID  = 1
	mtu    = 1500
	chanSz = 512
)

type Service struct {
	s      *stack.Stack
	ep     *channel.Endpoint
	vsock  io.ReadWriteCloser
	ctx    context.Context
	cancel context.CancelFunc
}

func New(vsockConn io.ReadWriteCloser) (*Service, error) {
	ep := channel.New(chanSz, mtu, "")

	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})

	if err := s.CreateNIC(nicID, ep); err != nil {
		return nil, fmt.Errorf("create nic: %v", err)
	}

	s.SetRouteTable([]tcpip.Route{
		{Destination: header.IPv4EmptySubnet, NIC: nicID},
		{Destination: header.IPv6EmptySubnet, NIC: nicID},
	})

	s.SetPromiscuousMode(nicID, true)
	s.SetSpoofing(nicID, true)

	svc := &Service{s: s, ep: ep, vsock: vsockConn}
	svc.ctx, svc.cancel = context.WithCancel(context.Background())
	return svc, nil
}

func (svc *Service) Run() error {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		svc.readFromVsock()
	}()

	go func() {
		defer wg.Done()
		svc.writeToVsock()
	}()

	tcpForwarder := tcp.NewForwarder(svc.s, 0, 1024, func(r *tcp.ForwarderRequest) {
		id := r.ID()
		go svc.handleTCP(r, id)
	})
	svc.s.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpForwarder.HandlePacket)

	udpForwarder := udp.NewForwarder(svc.s, func(r *udp.ForwarderRequest) bool {
		id := r.ID()
		go svc.handleUDP(r, id)
		return true
	})
	svc.s.SetTransportProtocolHandler(udp.ProtocolNumber, udpForwarder.HandlePacket)

	wg.Wait()
	return nil
}

func (svc *Service) readFromVsock() {
	defer svc.cancel()
	hdr := make([]byte, 4)
	for {
		if _, err := io.ReadFull(svc.vsock, hdr); err != nil {
			log.Printf("vsock read header: %v", err)
			return
		}
		length := binary.BigEndian.Uint32(hdr)
		frame := make([]byte, length)
		if _, err := io.ReadFull(svc.vsock, frame); err != nil {
			log.Printf("vsock read frame: %v", err)
			return
		}

		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
			Payload: buffer.MakeWithData(frame),
		})
		svc.ep.InjectInbound(determineProtocol(frame), pkt)
		pkt.DecRef()
	}
}

func (svc *Service) writeToVsock() {
	for {
		pkt := svc.ep.ReadContext(svc.ctx)
		if pkt == nil {
			return
		}
		view := pkt.ToView()
		if view == nil {
			pkt.DecRef()
			continue
		}
		data := view.AsSlice()
		pkt.DecRef()

		hdr := make([]byte, 4)
		binary.BigEndian.PutUint32(hdr, uint32(len(data)))
		if _, err := svc.vsock.Write(hdr); err != nil {
			log.Printf("netstack: vsock write: %v", err)
			return
		}
		if _, err := svc.vsock.Write(data); err != nil {
			log.Printf("netstack: vsock write: %v", err)
			return
		}
	}
}

func (svc *Service) handleTCP(r *tcp.ForwarderRequest, id stack.TransportEndpointID) {
	var wq waiter.Queue
	ep, err := r.CreateEndpoint(&wq)
	if err != nil {
		r.Complete(true)
		return
	}
	r.Complete(false)

	remote := net.JoinHostPort(id.LocalAddress.String(), fmt.Sprintf("%d", id.LocalPort))
	hostConn, dialErr := net.Dial("tcp", remote)
	if dialErr != nil {
		ep.Close()
		return
	}

	guestConn := gonet.NewTCPConn(&wq, ep)
	relay(guestConn, hostConn)
}

func (svc *Service) handleUDP(r *udp.ForwarderRequest, id stack.TransportEndpointID) {
	var wq waiter.Queue
	ep, err := r.CreateEndpoint(&wq)
	if err != nil {
		return
	}

	remote := net.JoinHostPort(id.LocalAddress.String(), fmt.Sprintf("%d", id.LocalPort))
	hostConn, dialErr := net.Dial("udp", remote)
	if dialErr != nil {
		ep.Close()
		return
	}

	guestConn := gonet.NewUDPConn(&wq, ep)
	relay(guestConn, hostConn)
}

func relay(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(b, a)
		b.Close()
	}()
	go func() {
		defer wg.Done()
		io.Copy(a, b)
		a.Close()
	}()
	wg.Wait()
}

func determineProtocol(frame []byte) tcpip.NetworkProtocolNumber {
	if len(frame) < 14 {
		return ipv4.ProtocolNumber
	}
	etherType := binary.BigEndian.Uint16(frame[12:14])
	switch etherType {
	case 0x0800:
		return ipv4.ProtocolNumber
	case 0x86DD:
		return ipv6.ProtocolNumber
	default:
		return ipv4.ProtocolNumber
	}
}
