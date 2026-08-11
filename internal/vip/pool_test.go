package vip_test

import (
	"net/netip"
	"testing"

	"github.com/aleksclark/stacklane/internal/vip"
)

func TestParsePool_ValidLoopback(t *testing.T) {
	t.Parallel()
	p, err := vip.ParsePool("127.77.0.0/24")
	if err != nil {
		t.Fatalf("ParsePool: %v", err)
	}
	if p.Prefix().String() != "127.77.0.0/24" {
		t.Fatalf("prefix = %s", p.Prefix())
	}
}

func TestParsePool_RejectsNonLoopback(t *testing.T) {
	t.Parallel()
	if _, err := vip.ParsePool("10.0.0.0/24"); err == nil {
		t.Fatal("expected error for non-loopback pool")
	}
}

func TestParsePool_RejectsIPv6(t *testing.T) {
	t.Parallel()
	if _, err := vip.ParsePool("::1/128"); err == nil {
		t.Fatal("expected error for IPv6")
	}
}

func TestParsePool_RejectsPrefixTooWide(t *testing.T) {
	t.Parallel()
	if _, err := vip.ParsePool("127.0.0.0/8"); err == nil {
		t.Fatal("expected error for /8")
	}
}

func TestParsePool_RejectsPrefixTooNarrow(t *testing.T) {
	t.Parallel()
	if _, err := vip.ParsePool("127.77.0.1/31"); err == nil {
		t.Fatal("expected error for /31")
	}
}

func TestUsableSorted_SkipsDotZeroAndDot255(t *testing.T) {
	t.Parallel()
	p, err := vip.ParsePool("127.77.0.0/24")
	if err != nil {
		t.Fatalf("ParsePool: %v", err)
	}
	addrs := p.UsableSorted()
	if len(addrs) == 0 {
		t.Fatal("expected usable addresses")
	}
	// First usable should be .1, last .254; never .0 or .255.
	if addrs[0].String() != "127.77.0.1" {
		t.Fatalf("first = %s, want 127.77.0.1", addrs[0])
	}
	last := addrs[len(addrs)-1]
	if last.String() != "127.77.0.254" {
		t.Fatalf("last = %s, want 127.77.0.254", last)
	}
	for _, a := range addrs {
		b := a.As4()
		if b[3] == 0 || b[3] == 255 {
			t.Fatalf("usable set includes excluded last-octet address %s", a)
		}
	}
	// 256 - network(.0) - broadcast(.255) = 254
	if len(addrs) != 254 {
		t.Fatalf("len = %d, want 254", len(addrs))
	}
}

func TestUsableSorted_AscendingDeterministic(t *testing.T) {
	t.Parallel()
	p, err := vip.ParsePool("127.77.1.0/29")
	if err != nil {
		t.Fatalf("ParsePool: %v", err)
	}
	a := p.UsableSorted()
	b := p.UsableSorted()
	if len(a) != len(b) {
		t.Fatalf("length mismatch %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("non-deterministic at %d: %s vs %s", i, a[i], b[i])
		}
		if i > 0 && !a[i-1].Less(a[i]) {
			t.Fatalf("not strictly ascending: %s then %s", a[i-1], a[i])
		}
	}
}

func TestUsableSorted_Contains(t *testing.T) {
	t.Parallel()
	p, err := vip.ParsePool("127.77.0.0/30")
	if err != nil {
		t.Fatalf("ParsePool: %v", err)
	}
	// /30: 127.77.0.0 network, .1, .2, .3 broadcast
	// skip .0 and .255 → usable .1, .2
	addrs := p.UsableSorted()
	want := []string{"127.77.0.1", "127.77.0.2"}
	if len(addrs) != len(want) {
		t.Fatalf("got %v, want %v", addrs, want)
	}
	for i, s := range want {
		if addrs[i].String() != s {
			t.Fatalf("addrs[%d]=%s want %s", i, addrs[i], s)
		}
	}
	ok, _ := netip.ParseAddr("127.77.0.1")
	if !p.Contains(ok) {
		t.Fatal("expected Contains(.1)")
	}
	bad, _ := netip.ParseAddr("127.77.0.0")
	if p.Contains(bad) {
		t.Fatal("network address should not be Contains")
	}
}
