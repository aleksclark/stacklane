package reconcile

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/aleksclark/stacklane/internal/dns"
	"github.com/aleksclark/stacklane/internal/domain"
	"github.com/aleksclark/stacklane/internal/proxy"
	"github.com/aleksclark/stacklane/internal/state"
)

// applyConfig is the subset of Deps needed to apply a desired state.
type applyConfig struct {
	Store         state.Store
	DNS           dns.Controller
	Proxy         proxy.Manager
	BaseDomain    string
	DNSTTL        uint32
	VIPLeaseGrace time.Duration // retained for status/lease labeling if needed
	DNSListen     string
	Logger        *slog.Logger
	Now           func() time.Time
}

// apply runs lease expiry, DNS replace, proxy reconcile, store save, and
// returns the status snapshot to publish. Store save failures are logged and
// do not tear down live proxies.
func apply(ctx context.Context, desired domain.DesiredState, snap state.Snapshot, cfg applyConfig) StatusSnapshot {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	// DNS records only for stacks with ≥1 endpoint.
	// (Expired leases are released in the engine before BuildDesired.)
	recs := buildDNSRecords(desired, cfg.BaseDomain, cfg.DNSTTL)
	if cfg.DNS != nil {
		if err := cfg.DNS.SetRecords(recs); err != nil {
			log.Error("dns set records failed", "err", err)
		}
	}

	// Proxy endpoints.
	eps := make([]domain.Endpoint, 0, len(desired.Endpoints))
	for _, ep := range desired.Endpoints {
		eps = append(eps, ep)
	}
	sort.Slice(eps, func(i, j int) bool {
		if eps[i].FQDN != eps[j].FQDN {
			return eps[i].FQDN < eps[j].FQDN
		}
		return eps[i].PublicPort < eps[j].PublicPort
	})
	if cfg.Proxy != nil {
		if err := cfg.Proxy.Reconcile(ctx, eps); err != nil {
			log.Error("proxy reconcile failed", "err", err)
		}
	}

	// Persist leases.
	if cfg.Store != nil {
		if err := cfg.Store.Save(snap); err != nil {
			log.Error("state save failed; keeping memory", "err", err)
		}
	}

	return buildStatusSnapshot(desired, snap, cfg)
}

func buildDNSRecords(desired domain.DesiredState, baseDomain string, ttl uint32) []dns.Record {
	type pair struct {
		name string
		vip  string
		addr domain.Endpoint // unused placeholder
	}
	_ = pair{}

	seen := make(map[string]struct{})
	var recs []dns.Record

	// Stack A records.
	keys := make([]domain.StackKey, 0, len(desired.Stacks))
	for k := range desired.Stacks {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, k := range keys {
		st := desired.Stacks[k]
		// Only publish stack DNS if it has ≥1 endpoint (desired.Stacks only has active).
		_, stackFQDN := domain.BuildFQDNs("_", st.ProjectSlug, st.InstanceSlug, baseDomain)
		if _, ok := seen[stackFQDN]; !ok {
			seen[stackFQDN] = struct{}{}
			recs = append(recs, dns.Record{Name: stackFQDN, Type: "A", VIP: st.VIP, TTL: ttl})
		}
	}

	// Endpoint A records.
	epKeys := make([]string, 0, len(desired.Endpoints))
	for k := range desired.Endpoints {
		epKeys = append(epKeys, k)
	}
	sort.Strings(epKeys)
	for _, k := range epKeys {
		ep := desired.Endpoints[k]
		if _, ok := seen[ep.FQDN]; ok {
			continue
		}
		seen[ep.FQDN] = struct{}{}
		recs = append(recs, dns.Record{Name: ep.FQDN, Type: "A", VIP: ep.VIP, TTL: ttl})
	}
	return recs
}

func buildStatusSnapshot(desired domain.DesiredState, snap state.Snapshot, cfg applyConfig) StatusSnapshot {
	dnsListen := cfg.DNSListen
	if dnsListen == "" && cfg.DNS != nil {
		dnsListen = cfg.DNS.ListenAddr()
	}

	// Index leases by key.
	leaseByKey := make(map[domain.StackKey]state.Lease, len(snap.Leases))
	for _, l := range snap.Leases {
		leaseByKey[l.StackKey] = l
	}

	// Active stacks from desired.
	stackKeys := make([]domain.StackKey, 0, len(desired.Stacks))
	for k := range desired.Stacks {
		stackKeys = append(stackKeys, k)
	}
	sort.Slice(stackKeys, func(i, j int) bool { return stackKeys[i] < stackKeys[j] })

	// Also include leased-but-inactive stacks for status visibility.
	inactiveKeys := make([]domain.StackKey, 0)
	activeSet := make(map[domain.StackKey]struct{}, len(desired.Stacks))
	for k := range desired.Stacks {
		activeSet[k] = struct{}{}
	}
	for _, l := range snap.Leases {
		if _, ok := activeSet[l.StackKey]; !ok {
			inactiveKeys = append(inactiveKeys, l.StackKey)
		}
	}
	sort.Slice(inactiveKeys, func(i, j int) bool { return inactiveKeys[i] < inactiveKeys[j] })

	statusStacks := make([]StatusStack, 0, len(stackKeys)+len(inactiveKeys))
	for _, k := range stackKeys {
		st := desired.Stacks[k]
		leaseState := "active"
		ss := StatusStack{
			Key:   string(k),
			VIP:   st.VIP.String(),
			Lease: leaseState,
		}
		// Attach endpoints for this stack.
		for _, ep := range desired.Endpoints {
			if ep.StackKey != k {
				continue
			}
			ss.Endpoints = append(ss.Endpoints, StatusEndpoint{
				FQDN:        ep.FQDN,
				PublicPort:  ep.PublicPort,
				Target:      ep.TargetHost.String() + ":" + itoaPort(ep.TargetBind),
				ContainerID: ep.ContainerID,
				Service:     ep.ServiceName,
				Protocol:    string(ep.Protocol),
				VIP:         ep.VIP.String(),
			})
		}
		sort.Slice(ss.Endpoints, func(i, j int) bool { return ss.Endpoints[i].FQDN < ss.Endpoints[j].FQDN })
		statusStacks = append(statusStacks, ss)
	}
	for _, k := range inactiveKeys {
		l := leaseByKey[k]
		statusStacks = append(statusStacks, StatusStack{
			Key:   string(k),
			VIP:   l.VIP.String(),
			Lease: "grace",
		})
	}

	// Flat endpoint list for tests / resolve.
	flat := make([]domain.Endpoint, 0, len(desired.Endpoints))
	for _, ep := range desired.Endpoints {
		flat = append(flat, ep)
	}
	sort.Slice(flat, func(i, j int) bool {
		if flat[i].FQDN != flat[j].FQDN {
			return flat[i].FQDN < flat[j].FQDN
		}
		return flat[i].PublicPort < flat[j].PublicPort
	})

	// Name→VIP map for resolve (endpoint + stack FQDNs).
	resolve := make(map[string]string)
	for _, st := range desired.Stacks {
		_, stackFQDN := domain.BuildFQDNs("_", st.ProjectSlug, st.InstanceSlug, cfg.BaseDomain)
		resolve[normalizeName(stackFQDN)] = st.VIP.String()
	}
	for _, ep := range desired.Endpoints {
		resolve[normalizeName(ep.FQDN)] = ep.VIP.String()
	}

	return StatusSnapshot{
		Daemon:     "running",
		DNSListen:  dnsListen,
		BaseDomain: cfg.BaseDomain,
		VIPPool:    "", // filled by engine if known
		Stacks:     statusStacks,
		Endpoints:  flat,
		Resolve:    resolve,
		Leases:     append([]state.Lease(nil), snap.Leases...),
	}
}

func itoaPort(p uint16) string {
	// small local helper avoiding strconv import churn in hot path — use strconv.
	if p == 0 {
		return "0"
	}
	var buf [5]byte
	i := len(buf)
	for p > 0 {
		i--
		buf[i] = byte('0' + p%10)
		p /= 10
	}
	return string(buf[i:])
}
