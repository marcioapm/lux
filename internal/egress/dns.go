package egress

import (
	"context"
	"io"
	"net"
	"net/netip"
	"strings"

	"golang.org/x/net/dns/dnsmessage"
)

// stub is one Run's DNS server, on its gateway address. It answers A
// queries for allowed names with the addresses the firewall allows (so an
// answer is always reachable), and refuses everything else: DNS is not a
// way out (no tunnelling through lookups of arbitrary names).
type stub struct {
	udp *net.UDPConn
	tcp *net.TCPListener
}

func startStub(gw netip.Addr, r *run, f *Firewall) (*stub, error) {
	addr := netip.AddrPortFrom(gw, StubPort)
	udp, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(addr))
	if err != nil {
		return nil, err
	}
	tcp, err := net.ListenTCP("tcp", net.TCPAddrFromAddrPort(addr))
	if err != nil {
		udp.Close()
		return nil, err
	}
	s := &stub{udp: udp, tcp: tcp}
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := udp.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			if resp := answer(buf[:n], r, f); resp != nil {
				udp.WriteToUDPAddrPort(resp, from)
			}
		}
	}()
	go func() {
		for {
			c, err := tcp.Accept()
			if err != nil {
				return
			}
			go serveTCP(c, r, f)
		}
	}()
	return s, nil
}

func serveTCP(c net.Conn, r *run, f *Firewall) {
	defer c.Close()
	var l [2]byte
	if _, err := io.ReadFull(c, l[:]); err != nil {
		return
	}
	msg := make([]byte, int(l[0])<<8|int(l[1]))
	if _, err := io.ReadFull(c, msg); err != nil {
		return
	}
	if resp := answer(msg, r, f); resp != nil {
		c.Write(append([]byte{byte(len(resp) >> 8), byte(len(resp))}, resp...))
	}
}

func (s *stub) close() {
	s.udp.Close()
	s.tcp.Close()
}

func answer(query []byte, r *run, f *Firewall) []byte {
	var p dnsmessage.Parser
	h, err := p.Start(query)
	if err != nil {
		return nil
	}
	q, err := p.Question()
	if err != nil {
		return nil
	}
	name := strings.TrimSuffix(strings.ToLower(q.Name.String()), ".")
	allowed := r.hosts[name]
	resp := dnsmessage.Header{ID: h.ID, Response: true, RecursionAvailable: true, RecursionDesired: h.RecursionDesired}
	var answers []netip.Addr
	switch {
	case !allowed:
		resp.RCode = dnsmessage.RCodeRefused
	case q.Type == dnsmessage.TypeA:
		answers = f.resolve(context.Background(), r, name)
		if len(answers) == 0 {
			resp.RCode = dnsmessage.RCodeNameError
		}
	default:
		// AAAA and others: no answer (egress is IPv4), but the name exists.
	}
	if r.onDNS != nil && (q.Type == dnsmessage.TypeA || !allowed) {
		l := Lookup{Name: name, Allowed: allowed}
		for _, a := range answers {
			l.Answers = append(l.Answers, a.String())
		}
		r.onDNS(l)
	}
	b := dnsmessage.NewBuilder(nil, resp)
	b.EnableCompression()
	_ = b.StartQuestions()
	_ = b.Question(q)
	_ = b.StartAnswers()
	for _, a := range answers {
		_ = b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 60}, dnsmessage.AResource{A: a.As4()})
	}
	out, err := b.Finish()
	if err != nil {
		return nil
	}
	return out
}
