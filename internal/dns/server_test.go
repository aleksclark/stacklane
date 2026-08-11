package dns_test

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aleksclark/stacklane/internal/dns"
	mdns "github.com/miekg/dns"
)

const baseDomain = "stacklane.test"

func startServer(t *testing.T) *dns.Server {
	t.Helper()
	s, err := dns.NewServer("127.0.0.1:0", baseDomain)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
	if s.ListenAddr() == "" {
		t.Fatal("ListenAddr empty")
	}
	// Ensure port is bound.
	if _, err := net.ResolveUDPAddr("udp", s.ListenAddr()); err != nil {
		t.Fatalf("ListenAddr %q: %v", s.ListenAddr(), err)
	}
	return s
}

func query(t *testing.T, serverAddr, name string, qtype uint16) *mdns.Msg {
	t.Helper()
	c := new(mdns.Client)
	c.Timeout = 2 * time.Second
	m := new(mdns.Msg)
	m.SetQuestion(mdns.Fqdn(name), qtype)
	m.RecursionDesired = false
	resp, _, err := c.Exchange(m, serverAddr)
	if err != nil {
		t.Fatalf("Exchange %s type=%d: %v", name, qtype, err)
	}
	if resp == nil {
		t.Fatal("nil response")
	}
	return resp
}

func mustSet(t *testing.T, s dns.Controller, recs []dns.Record) {
	t.Helper()
	if err := s.SetRecords(recs); err != nil {
		t.Fatalf("SetRecords: %v", err)
	}
}

func TestARecordHit_NOERROR(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	vip := netip.MustParseAddr("127.77.0.10")
	name := "postgres.feature-a.curri.stacklane.test"
	mustSet(t, s, []dns.Record{{
		Name: name,
		Type: "A",
		VIP:  vip,
		TTL:  5,
	}})

	resp := query(t, s.ListenAddr(), name, mdns.TypeA)
	if resp.Rcode != mdns.RcodeSuccess {
		t.Fatalf("rcode=%v want NOERROR", mdns.RcodeToString[resp.Rcode])
	}
	if !resp.Authoritative {
		t.Fatal("expected AA bit")
	}
	if len(resp.Answer) != 1 {
		t.Fatalf("answers=%d want 1: %v", len(resp.Answer), resp.Answer)
	}
	a, ok := resp.Answer[0].(*mdns.A)
	if !ok {
		t.Fatalf("answer type %T want *A", resp.Answer[0])
	}
	if got := a.A.String(); got != vip.String() {
		t.Fatalf("A=%s want %s", got, vip)
	}
	if a.Hdr.Ttl != 5 {
		t.Fatalf("TTL=%d want 5", a.Hdr.Ttl)
	}
	if a.Hdr.Name != mdns.Fqdn(name) {
		t.Fatalf("answer name=%q want %q", a.Hdr.Name, mdns.Fqdn(name))
	}
}

func TestUnknownName_NXDOMAIN(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	mustSet(t, s, []dns.Record{{
		Name: "feature-a.curri.stacklane.test",
		Type: "A",
		VIP:  netip.MustParseAddr("127.77.0.1"),
		TTL:  5,
	}})

	// Unknown under known stack / zone.
	resp := query(t, s.ListenAddr(), "unknown.feature-a.curri.stacklane.test", mdns.TypeA)
	if resp.Rcode != mdns.RcodeNameError {
		t.Fatalf("rcode=%v want NXDOMAIN", mdns.RcodeToString[resp.Rcode])
	}
	if !resp.Authoritative {
		t.Fatal("expected AA bit on NXDOMAIN")
	}

	// Completely unknown in-zone name.
	resp2 := query(t, s.ListenAddr(), "other.stacklane.test", mdns.TypeA)
	if resp2.Rcode != mdns.RcodeNameError {
		t.Fatalf("rcode=%v want NXDOMAIN", mdns.RcodeToString[resp2.Rcode])
	}
	if !resp2.Authoritative {
		t.Fatal("expected AA bit")
	}
}

func TestOutOfZone_REFUSED(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	resp := query(t, s.ListenAddr(), "example.com", mdns.TypeA)
	if resp.Rcode != mdns.RcodeRefused {
		t.Fatalf("rcode=%v want REFUSED", mdns.RcodeToString[resp.Rcode])
	}
}

func TestAAAA_ExistingA_NOERROREmpty(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	name := "postgres.feature-a.curri.stacklane.test"
	mustSet(t, s, []dns.Record{{
		Name: name,
		Type: "A",
		VIP:  netip.MustParseAddr("127.77.0.10"),
		TTL:  5,
	}})

	resp := query(t, s.ListenAddr(), name, mdns.TypeAAAA)
	if resp.Rcode != mdns.RcodeSuccess {
		t.Fatalf("rcode=%v want NOERROR", mdns.RcodeToString[resp.Rcode])
	}
	if !resp.Authoritative {
		t.Fatal("expected AA bit")
	}
	if len(resp.Answer) != 0 {
		t.Fatalf("answers=%d want 0: %v", len(resp.Answer), resp.Answer)
	}
}

func TestAAAA_Unknown_NXDOMAIN(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	resp := query(t, s.ListenAddr(), "nope.stacklane.test", mdns.TypeAAAA)
	if resp.Rcode != mdns.RcodeNameError {
		t.Fatalf("rcode=%v want NXDOMAIN", mdns.RcodeToString[resp.Rcode])
	}
	if !resp.Authoritative {
		t.Fatal("expected AA")
	}
}

func TestTXT_MX_ExistingAndUnknown(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	name := "feature-a.curri.stacklane.test"
	mustSet(t, s, []dns.Record{{
		Name: name,
		Type: "A",
		VIP:  netip.MustParseAddr("127.77.0.2"),
		TTL:  5,
	}})

	for _, qt := range []uint16{mdns.TypeTXT, mdns.TypeMX} {
		resp := query(t, s.ListenAddr(), name, qt)
		if resp.Rcode != mdns.RcodeSuccess {
			t.Fatalf("type %d rcode=%v want NOERROR", qt, mdns.RcodeToString[resp.Rcode])
		}
		if !resp.Authoritative {
			t.Fatalf("type %d expected AA", qt)
		}
		if len(resp.Answer) != 0 {
			t.Fatalf("type %d answers=%d want 0", qt, len(resp.Answer))
		}
	}

	resp := query(t, s.ListenAddr(), "missing.stacklane.test", mdns.TypeTXT)
	if resp.Rcode != mdns.RcodeNameError {
		t.Fatalf("unknown TXT rcode=%v want NXDOMAIN", mdns.RcodeToString[resp.Rcode])
	}
}

func TestPTR_REFUSED(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	resp := query(t, s.ListenAddr(), "10.0.77.127.in-addr.arpa", mdns.TypePTR)
	if resp.Rcode != mdns.RcodeRefused {
		t.Fatalf("rcode=%v want REFUSED", mdns.RcodeToString[resp.Rcode])
	}
}

func TestCaseInsensitiveMatch_RespondsWithQuestionCase(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	vip := netip.MustParseAddr("127.77.0.10")
	mustSet(t, s, []dns.Record{{
		Name: "Postgres.Feature-A.Curri.Stacklane.Test",
		Type: "A",
		VIP:  vip,
		TTL:  5,
	}})

	qName := "POSTGRES.feature-a.CURRI.stacklane.TEST"
	resp := query(t, s.ListenAddr(), qName, mdns.TypeA)
	if resp.Rcode != mdns.RcodeSuccess {
		t.Fatalf("rcode=%v want NOERROR", mdns.RcodeToString[resp.Rcode])
	}
	if len(resp.Answer) != 1 {
		t.Fatalf("answers=%d want 1", len(resp.Answer))
	}
	// Response should echo question case in RR owner name.
	if got := resp.Answer[0].Header().Name; got != mdns.Fqdn(qName) {
		t.Fatalf("answer name=%q want question case %q", got, mdns.Fqdn(qName))
	}
	if len(resp.Question) != 1 || resp.Question[0].Name != mdns.Fqdn(qName) {
		t.Fatalf("question name not preserved: %+v", resp.Question)
	}
}

func TestStackBaseA_Works(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	vip := netip.MustParseAddr("127.77.0.5")
	stack := "feature-a.curri.stacklane.test"
	ep := "postgres.feature-a.curri.stacklane.test"
	mustSet(t, s, []dns.Record{
		{Name: stack, Type: "A", VIP: vip, TTL: 5},
		{Name: ep, Type: "A", VIP: vip, TTL: 5},
	})

	for _, name := range []string{stack, ep} {
		resp := query(t, s.ListenAddr(), name, mdns.TypeA)
		if resp.Rcode != mdns.RcodeSuccess {
			t.Fatalf("%s rcode=%v", name, mdns.RcodeToString[resp.Rcode])
		}
		a := resp.Answer[0].(*mdns.A)
		if a.A.String() != vip.String() {
			t.Fatalf("%s A=%s want %s", name, a.A, vip)
		}
	}
}

func TestSetRecords_ReplacesAtomically(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	oldVIP := netip.MustParseAddr("127.77.0.1")
	newVIP := netip.MustParseAddr("127.77.0.2")
	name := "app.stacklane.test"
	mustSet(t, s, []dns.Record{{Name: name, Type: "A", VIP: oldVIP, TTL: 5}})
	mustSet(t, s, []dns.Record{{Name: name, Type: "A", VIP: newVIP, TTL: 10}})

	resp := query(t, s.ListenAddr(), name, mdns.TypeA)
	a := resp.Answer[0].(*mdns.A)
	if a.A.String() != newVIP.String() {
		t.Fatalf("A=%s want %s after replace", a.A, newVIP)
	}
	if a.Hdr.Ttl != 10 {
		t.Fatalf("TTL=%d want 10", a.Hdr.Ttl)
	}

	// Old-only name should be gone after full replace.
	mustSet(t, s, []dns.Record{{Name: "other.stacklane.test", Type: "A", VIP: newVIP, TTL: 5}})
	resp2 := query(t, s.ListenAddr(), name, mdns.TypeA)
	if resp2.Rcode != mdns.RcodeNameError {
		t.Fatalf("removed name rcode=%v want NXDOMAIN", mdns.RcodeToString[resp2.Rcode])
	}
}

func TestSetRecords_RejectsNonLoopbackVIP(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	good := netip.MustParseAddr("127.77.0.1")
	name := "app.stacklane.test"
	mustSet(t, s, []dns.Record{{Name: name, Type: "A", VIP: good, TTL: 5}})

	// Pure rejection: non-loopback VIP must error.
	nonLoop := netip.MustParseAddr("8.8.8.8")
	err := s.SetRecords([]dns.Record{{Name: name, Type: "A", VIP: nonLoop, TTL: 5}})
	if err == nil {
		t.Fatal("expected error for non-loopback A-record VIP")
	}
	if !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("error %q should mention loopback", err)
	}

	// Defense in depth: prior good records must remain when validation fails.
	resp := query(t, s.ListenAddr(), name, mdns.TypeA)
	if resp.Rcode != mdns.RcodeSuccess {
		t.Fatalf("prior record cleared on reject: rcode=%v", mdns.RcodeToString[resp.Rcode])
	}
	a := resp.Answer[0].(*mdns.A)
	if a.A.String() != good.String() {
		t.Fatalf("prior A=%s want %s after rejected SetRecords", a.A, good)
	}

	// Mixed batch with one bad VIP must reject entirely (no partial apply).
	err = s.SetRecords([]dns.Record{
		{Name: name, Type: "A", VIP: good, TTL: 5},
		{Name: "other.stacklane.test", Type: "A", VIP: netip.MustParseAddr("10.0.0.1"), TTL: 5},
	})
	if err == nil {
		t.Fatal("expected error for mixed batch with non-loopback VIP")
	}
	resp2 := query(t, s.ListenAddr(), name, mdns.TypeA)
	a2 := resp2.Answer[0].(*mdns.A)
	if a2.A.String() != good.String() {
		t.Fatalf("mixed reject mutated prior A=%s want %s", a2.A, good)
	}
}

func TestConcurrentSetRecordsDuringQuery_RaceSafe(t *testing.T) {
	s := startServer(t)
	// Not t.Parallel — this is the race-focused stress test.

	var stop sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Writers thrash SetRecords.
	for w := 0; w < 4; w++ {
		stop.Add(1)
		go func(id int) {
			defer stop.Done()
			vip := netip.MustParseAddr("127.77.0." + string(rune('1'+id%9)))
			// Prefer stable IPs via netip construction.
			vip = netip.AddrFrom4([4]byte{127, 77, 0, byte(1 + id)})
			i := 0
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				name := "app.stacklane.test"
				_ = s.SetRecords([]dns.Record{
					{Name: name, Type: "A", VIP: vip, TTL: uint32(1 + (i % 5))},
					{Name: "other.stacklane.test", Type: "A", VIP: vip, TTL: 5},
				})
				i++
			}
		}(w)
	}

	// Readers hammer queries.
	errCh := make(chan error, 8)
	for r := 0; r < 8; r++ {
		stop.Add(1)
		go func() {
			defer stop.Done()
			c := new(mdns.Client)
			c.Timeout = time.Second
			deadline := time.Now().Add(500 * time.Millisecond)
			for time.Now().Before(deadline) {
				m := new(mdns.Msg)
				m.SetQuestion(mdns.Fqdn("app.stacklane.test"), mdns.TypeA)
				resp, _, err := c.Exchange(m, s.ListenAddr())
				if err != nil {
					// Transient under heavy load is ok; race detector is the point.
					continue
				}
				if resp.Rcode != mdns.RcodeSuccess && resp.Rcode != mdns.RcodeNameError {
					errCh <- errString("unexpected rcode " + mdns.RcodeToString[resp.Rcode])
					return
				}
				if resp.Rcode == mdns.RcodeSuccess && !resp.Authoritative {
					errCh <- errString("missing AA")
					return
				}
			}
		}()
	}

	time.Sleep(600 * time.Millisecond)
	cancel()
	stop.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestSOA_NS_ApexPolite(t *testing.T) {
	t.Parallel()
	s := startServer(t)

	resp := query(t, s.ListenAddr(), baseDomain, mdns.TypeSOA)
	if resp.Rcode != mdns.RcodeSuccess {
		t.Fatalf("SOA rcode=%v want NOERROR", mdns.RcodeToString[resp.Rcode])
	}
	if !resp.Authoritative {
		t.Fatal("SOA expected AA")
	}
	if len(resp.Answer) < 1 {
		t.Fatal("expected SOA answer")
	}
	if _, ok := resp.Answer[0].(*mdns.SOA); !ok {
		t.Fatalf("got %T want *SOA", resp.Answer[0])
	}

	respNS := query(t, s.ListenAddr(), baseDomain, mdns.TypeNS)
	if respNS.Rcode != mdns.RcodeSuccess || len(respNS.Answer) < 1 {
		t.Fatalf("NS rcode=%v answers=%d", mdns.RcodeToString[respNS.Rcode], len(respNS.Answer))
	}
}
