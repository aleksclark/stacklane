package reconcile

import (
	"log/slog"
	"net/netip"
	"sort"
	"time"

	"github.com/aleksclark/stacklane/internal/dockerapi"
	"github.com/aleksclark/stacklane/internal/domain"
	"github.com/aleksclark/stacklane/internal/labels"
	"github.com/aleksclark/stacklane/internal/state"
	"github.com/aleksclark/stacklane/internal/vip"
)

// BuildConfig carries build-time knobs from the engine.
type BuildConfig struct {
	BaseDomain      string
	DefaultInstance string
	Logger          *slog.Logger
}

// candidate is a parsed container endpoint prior to conflict resolution.
type candidate struct {
	meta       labels.Meta
	container  dockerapi.Container
	targetBind uint16
	epFQDN     string
	stackFQDN  string
	mapKey     string
}

// BuildDesired derives DesiredState from running containers and mutates snap leases
// (allocate VIPs, touch active leases). Per-container errors are logged and skipped.
// Conflict rule: same EndpointMapKey → smaller container ID wins.
func BuildDesired(
	containers []dockerapi.Container,
	snap *state.Snapshot,
	alloc vip.Allocator,
	cfg BuildConfig,
	now time.Time,
) domain.DesiredState {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}

	desired := domain.DesiredState{
		Stacks:    make(map[domain.StackKey]domain.Stack),
		Endpoints: make(map[string]domain.Endpoint),
	}

	// Collect candidates; isolate per-container failures.
	var cands []candidate
	for _, c := range containers {
		cand, ok := parseContainer(c, cfg, log)
		if !ok {
			continue
		}
		cands = append(cands, cand)
	}

	// Conflict resolution: group by map key, pick smallest container ID.
	byKey := make(map[string][]candidate)
	for _, cand := range cands {
		byKey[cand.mapKey] = append(byKey[cand.mapKey], cand)
	}
	winners := make([]candidate, 0, len(byKey))
	for _, group := range byKey {
		sort.Slice(group, func(i, j int) bool {
			return group[i].container.ID < group[j].container.ID
		})
		winners = append(winners, group[0])
		if len(group) > 1 {
			log.Warn("endpoint conflict; smaller container_id wins",
				"key", group[0].mapKey,
				"winner", truncateID(group[0].container.ID),
				"losers", len(group)-1,
			)
		}
	}
	// Stable order for deterministic VIP allocation across stacks.
	sort.Slice(winners, func(i, j int) bool {
		if winners[i].meta.StackKey != winners[j].meta.StackKey {
			return winners[i].meta.StackKey < winners[j].meta.StackKey
		}
		return winners[i].mapKey < winners[j].mapKey
	})

	activeStacks := make(map[domain.StackKey]struct{})
	loopback := netip.MustParseAddr("127.0.0.1")

	for _, cand := range winners {
		key := cand.meta.StackKey
		if alloc == nil {
			log.Error("vip allocator missing", "stack", string(key))
			continue
		}
		addr, err := alloc.Allocate(snap, key)
		if err != nil {
			log.Error("vip allocate failed", "stack", string(key), "err", err)
			continue
		}
		activeStacks[key] = struct{}{}

		if _, ok := desired.Stacks[key]; !ok {
			desired.Stacks[key] = domain.Stack{
				Key:            key,
				ProjectSlug:    cand.meta.ProjectSlug,
				InstanceSlug:   cand.meta.InstanceSlug,
				ComposeProject: cand.meta.ComposeProject,
				VIP:            addr,
			}
		} else {
			// Keep VIP from first allocation for the stack.
			addr = desired.Stacks[key].VIP
		}

		ep := domain.Endpoint{
			StackKey:    key,
			Name:        cand.meta.EndpointName,
			FQDN:        cand.epFQDN,
			Protocol:    cand.meta.Protocol,
			VIP:         addr,
			PublicPort:  cand.meta.PublicPort,
			TargetPort:  cand.meta.TargetPort,
			TargetHost:  loopback,
			TargetBind:  cand.targetBind,
			ContainerID: cand.container.ID,
			ServiceName: cand.meta.ServiceName,
		}
		desired.Endpoints[cand.mapKey] = ep
	}

	// Touch last_active_at + last_seen_at for stacks with ≥1 live endpoint.
	touchActiveLeases(snap, activeStacks, now)

	return desired
}

func parseContainer(c dockerapi.Container, cfg BuildConfig, log *slog.Logger) (candidate, bool) {
	meta, ok, err := labels.Parse(c.Labels, cfg.DefaultInstance)
	if err != nil {
		log.Warn("invalid labels; skipping container",
			"container", truncateID(c.ID),
			"err", err,
		)
		return candidate{}, false
	}
	if !ok {
		return candidate{}, false
	}

	hostPort, bindOK := dockerapi.LoopbackBinding(c.Ports, meta.TargetPort, string(meta.Protocol))
	if !bindOK {
		log.Warn("no loopback binding; skipping container",
			"container", truncateID(c.ID),
			"target_port", meta.TargetPort,
			"protocol", meta.Protocol,
		)
		return candidate{}, false
	}

	epFQDN, stackFQDN := domain.BuildFQDNs(meta.EndpointName, meta.ProjectSlug, meta.InstanceSlug, cfg.BaseDomain)
	mapKey := domain.EndpointMapKey(epFQDN, meta.PublicPort, meta.Protocol)
	return candidate{
		meta:       meta,
		container:  c,
		targetBind: hostPort,
		epFQDN:     epFQDN,
		stackFQDN:  stackFQDN,
		mapKey:     mapKey,
	}, true
}

func touchActiveLeases(snap *state.Snapshot, active map[domain.StackKey]struct{}, now time.Time) {
	if snap == nil || len(active) == 0 {
		return
	}
	for i := range snap.Leases {
		if _, ok := active[snap.Leases[i].StackKey]; ok {
			snap.Leases[i].LastActiveAt = now
			snap.Leases[i].LastSeenAt = now
		}
	}
}

func truncateID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}
