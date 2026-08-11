package dns

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"

	mdns "github.com/miekg/dns"
)

// recordSet is an immutable snapshot of dynamic A records keyed by lower-case FQDN.
type recordSet map[string]Record // key = dns.CanonicalName / lower FQDN without trailing rules via CanonicalName

// Server is an authoritative DNS server for a single base domain.
type Server struct {
	baseDomain string // FQDN form with trailing dot, lower-case
	baseLabel  string // without trailing dot, lower-case (for display)

	udp    *mdns.Server
	tcp    *mdns.Server
	pc     net.PacketConn
	tl     net.Listener
	addr   string // host:port actually bound
	listen string // original listen request

	records atomic.Pointer[recordSet]
	serial  atomic.Uint32

	mu       sync.Mutex
	started  bool
	shutdown bool
}

// NewServer constructs a Server for baseDomain listening on listenAddr (e.g. "127.0.0.1:0").
// Call Start to begin serving; after Start, ListenAddr returns the bound address.
func NewServer(listenAddr string, baseDomain string) (*Server, error) {
	if listenAddr == "" {
		return nil, fmt.Errorf("dns: listen address required")
	}
	base := strings.TrimSpace(baseDomain)
	if base == "" {
		return nil, fmt.Errorf("dns: base domain required")
	}
	// Normalize to FQDN lower-case with trailing dot.
	baseFQDN := mdns.CanonicalName(mdns.Fqdn(base))

	s := &Server{
		baseDomain: baseFQDN,
		baseLabel:  strings.TrimSuffix(baseFQDN, "."),
		listen:     listenAddr,
	}
	empty := make(recordSet)
	s.records.Store(&empty)
	s.serial.Store(1)
	return s, nil
}

// Start binds UDP (and TCP) and begins answering queries. Idempotent-safe after first success.
func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shutdown {
		return fmt.Errorf("dns: server shut down")
	}
	if s.started {
		return nil
	}

	pc, err := net.ListenPacket("udp", s.listen)
	if err != nil {
		return fmt.Errorf("dns: listen udp %s: %w", s.listen, err)
	}
	s.pc = pc
	s.addr = pc.LocalAddr().String()

	// Optional TCP on same port for completeness (miekg clients default UDP).
	tl, err := net.Listen("tcp", s.addr)
	if err != nil {
		_ = pc.Close()
		s.pc = nil
		return fmt.Errorf("dns: listen tcp %s: %w", s.addr, err)
	}
	s.tl = tl

	mux := mdns.NewServeMux()
	mux.HandleFunc(".", s.serveDNS)

	s.udp = &mdns.Server{PacketConn: pc, Handler: mux}
	s.tcp = &mdns.Server{Listener: tl, Handler: mux}

	errCh := make(chan error, 2)
	go func() {
		if err := s.udp.ActivateAndServe(); err != nil {
			errCh <- err
		}
	}()
	go func() {
		if err := s.tcp.ActivateAndServe(); err != nil {
			errCh <- err
		}
	}()

	// Non-blocking peek: if bind failed immediately, surface it.
	select {
	case err := <-errCh:
		_ = pc.Close()
		_ = tl.Close()
		return fmt.Errorf("dns: serve: %w", err)
	default:
	}

	s.started = true
	return nil
}

// ListenAddr returns the actual bound host:port.
func (s *Server) ListenAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// SetRecords replaces the entire dynamic A record set atomically.
func (s *Server) SetRecords(recs []Record) error {
	next := make(recordSet, len(recs))
	for _, r := range recs {
		if r.Type != "" && !strings.EqualFold(r.Type, "A") {
			return fmt.Errorf("dns: unsupported record type %q", r.Type)
		}
		if !r.VIP.IsValid() || !r.VIP.Is4() {
			return fmt.Errorf("dns: record %q requires IPv4 VIP", r.Name)
		}
		key := mdns.CanonicalName(mdns.Fqdn(r.Name))
		if !s.inZone(key) {
			return fmt.Errorf("dns: record %q outside base domain %s", r.Name, s.baseLabel)
		}
		// Store a copy with normalized name key; keep original Name for reference.
		cp := r
		cp.Name = key
		if cp.TTL == 0 {
			cp.TTL = 5
		}
		next[key] = cp
	}
	s.records.Store(&next)
	s.serial.Add(1)
	return nil
}

// Shutdown stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shutdown {
		return nil
	}
	s.shutdown = true
	if !s.started {
		return nil
	}

	done := make(chan error, 1)
	go func() {
		var first error
		if s.udp != nil {
			if err := s.udp.Shutdown(); err != nil && first == nil {
				first = err
			}
		}
		if s.tcp != nil {
			if err := s.tcp.Shutdown(); err != nil && first == nil {
				first = err
			}
		}
		done <- first
	}()

	select {
	case <-ctx.Done():
		if s.pc != nil {
			_ = s.pc.Close()
		}
		if s.tl != nil {
			_ = s.tl.Close()
		}
		return ctx.Err()
	case err := <-done:
		return err
	}
}

// Ensure Server implements Controller.
var _ Controller = (*Server)(nil)

func (s *Server) inZone(name string) bool {
	// name and baseDomain are canonical FQDNs (lower, trailing dot).
	if name == s.baseDomain {
		return true
	}
	return strings.HasSuffix(name, "."+s.baseDomain)
}

func (s *Server) serveDNS(w mdns.ResponseWriter, r *mdns.Msg) {
	m := new(mdns.Msg)
	m.SetReply(r)
	m.Authoritative = false
	m.RecursionAvailable = false

	if r == nil || len(r.Question) == 0 {
		m.Rcode = mdns.RcodeFormatError
		_ = w.WriteMsg(m)
		return
	}

	// Answer the first question (standard for simple auth servers).
	q := r.Question[0]
	qNameCanon := mdns.CanonicalName(q.Name)

	// PTR always refused (not supported).
	if q.Qtype == mdns.TypePTR {
		m.Rcode = mdns.RcodeRefused
		m.Authoritative = false
		_ = w.WriteMsg(m)
		return
	}

	if !s.inZone(qNameCanon) {
		m.Rcode = mdns.RcodeRefused
		_ = w.WriteMsg(m)
		return
	}

	// In-zone → authoritative.
	m.Authoritative = true

	recs := *s.records.Load()

	switch q.Qtype {
	case mdns.TypeA:
		s.handleA(m, q, qNameCanon, recs)
	case mdns.TypeAAAA:
		s.handleAAAA(m, q, qNameCanon, recs)
	case mdns.TypeSOA:
		s.handleSOA(m, q, qNameCanon)
	case mdns.TypeNS:
		s.handleNS(m, q, qNameCanon)
	default:
		// TXT/MX/etc: existing name → NOERROR empty; unknown → NXDOMAIN
		if s.nameExists(qNameCanon, recs) {
			m.Rcode = mdns.RcodeSuccess
		} else {
			m.Rcode = mdns.RcodeNameError
			s.addSOAAuthority(m)
		}
	}

	_ = w.WriteMsg(m)
}

func (s *Server) nameExists(canon string, recs recordSet) bool {
	if _, ok := recs[canon]; ok {
		return true
	}
	// Apex and ns.<base> always exist for polite zone.
	if canon == s.baseDomain {
		return true
	}
	if canon == mdns.CanonicalName("ns."+s.baseDomain) {
		return true
	}
	return false
}

func (s *Server) handleA(m *mdns.Msg, q mdns.Question, canon string, recs recordSet) {
	// Dynamic A records.
	if rec, ok := recs[canon]; ok {
		m.Rcode = mdns.RcodeSuccess
		m.Answer = append(m.Answer, &mdns.A{
			Hdr: mdns.RR_Header{
				Name:   q.Name, // respond with question case
				Rrtype: mdns.TypeA,
				Class:  mdns.ClassINET,
				Ttl:    rec.TTL,
			},
			A: rec.VIP.AsSlice(),
		})
		return
	}

	// Static ns.<base> A → listen IP if loopback else 127.0.0.1
	if canon == mdns.CanonicalName("ns."+s.baseDomain) {
		m.Rcode = mdns.RcodeSuccess
		ip := s.nsIP()
		m.Answer = append(m.Answer, &mdns.A{
			Hdr: mdns.RR_Header{
				Name:   q.Name,
				Rrtype: mdns.TypeA,
				Class:  mdns.ClassINET,
				Ttl:    5,
			},
			A: ip,
		})
		return
	}

	m.Rcode = mdns.RcodeNameError
	s.addSOAAuthority(m)
}

func (s *Server) handleAAAA(m *mdns.Msg, q mdns.Question, canon string, recs recordSet) {
	_ = q
	if s.nameExists(canon, recs) {
		// Name has A (or is apex/ns) → NOERROR empty answer.
		m.Rcode = mdns.RcodeSuccess
		return
	}
	m.Rcode = mdns.RcodeNameError
	s.addSOAAuthority(m)
}

func (s *Server) handleSOA(m *mdns.Msg, q mdns.Question, canon string) {
	if canon != s.baseDomain {
		// SOA only at apex; other names: if exist NOERROR empty else NXDOMAIN
		recs := *s.records.Load()
		if s.nameExists(canon, recs) {
			m.Rcode = mdns.RcodeSuccess
			return
		}
		m.Rcode = mdns.RcodeNameError
		s.addSOAAuthority(m)
		return
	}
	m.Rcode = mdns.RcodeSuccess
	m.Answer = append(m.Answer, s.soaRR(q.Name))
}

func (s *Server) handleNS(m *mdns.Msg, q mdns.Question, canon string) {
	if canon != s.baseDomain {
		recs := *s.records.Load()
		if s.nameExists(canon, recs) {
			m.Rcode = mdns.RcodeSuccess
			return
		}
		m.Rcode = mdns.RcodeNameError
		s.addSOAAuthority(m)
		return
	}
	m.Rcode = mdns.RcodeSuccess
	nsName := "ns." + s.baseDomain
	m.Answer = append(m.Answer, &mdns.NS{
		Hdr: mdns.RR_Header{
			Name:   q.Name,
			Rrtype: mdns.TypeNS,
			Class:  mdns.ClassINET,
			Ttl:    5,
		},
		Ns: nsName,
	})
}

func (s *Server) soaRR(owner string) *mdns.SOA {
	return &mdns.SOA{
		Hdr: mdns.RR_Header{
			Name:   owner,
			Rrtype: mdns.TypeSOA,
			Class:  mdns.ClassINET,
			Ttl:    5,
		},
		Ns:      "ns." + s.baseDomain,
		Mbox:    "hostmaster." + s.baseDomain,
		Serial:  s.serial.Load(),
		Refresh: 3600,
		Retry:   600,
		Expire:  86400,
		Minttl:  5,
	}
}

func (s *Server) addSOAAuthority(m *mdns.Msg) {
	// Minimal polite NXDOMAIN authority section.
	m.Ns = append(m.Ns, s.soaRR(s.baseDomain))
}

func (s *Server) nsIP() net.IP {
	host, _, err := net.SplitHostPort(s.ListenAddr())
	if err != nil {
		return net.IPv4(127, 0, 0, 1)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return net.IPv4(127, 0, 0, 1)
	}
	if ip4 := ip.To4(); ip4 != nil {
		addr, ok := netip.AddrFromSlice(ip4)
		if ok && addr.IsLoopback() {
			return ip4
		}
		// Non-loopback listen: still advertise 127.0.0.1 per plan.
		return net.IPv4(127, 0, 0, 1)
	}
	return net.IPv4(127, 0, 0, 1)
}
