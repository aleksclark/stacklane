# Stacklane Initial Functional MVP — Implementation Plan

> **For Hermes:** Use subagent-driven-development (or parallel lane implementers per the ownership matrix) to implement this plan task-by-task with strict TDD.
>
> **Plan-only commit target:** this file alone on `feat/initial-mvp`.
> **Later run report (do not write in this PR):** `docs/reviews/initial-implementation-run.md`

**Goal:** Ship a working local daemon that gives each Docker Compose project a stable loopback VIP, authoritative `*.test` DNS, and TCP proxies from standard ports on that VIP to Docker-assigned ephemeral loopback host ports — so multiple Compose worktrees can expose the same service ports without host-port conflicts.

**Architecture:** Event-driven reconciler watches Docker Engine lifecycle/health, derives one endpoint per labeled container from Compose + Stacklane labels and inspected `127.0.0.1` host bindings, allocates a durable per-stack VIP from a configurable loopback pool, publishes A records under `test`, and runs TCP listeners on `VIP:publicPort` that proxy only to inspected loopback targets.

**Tech stack:** Go 1.22+ module `github.com/aleksclark/stacklane`; stdlib-first; Docker Engine SDK; optional single DNS library only if stdlib is insufficient; atomic JSON state store (SQLite allowed only if JSON proves inadequate — default JSON); Makefile + GitHub Actions CI.

**Base SHA (parent of plan commit):** `5c68ab515bf63b2d568fa010dd188c7174f7139c`

**Module path:** `github.com/aleksclark/stacklane`

**Default branch:** `master` (never edit; never push from this plan work)

---

## 1. Product summary

Stacklane lets developers run multiple Docker Compose worktrees with **normal service ports** and **hierarchical local DNS names**, without visible host-port conflicts.

### Naming examples

```text
feature-a.curri.stacklane.test
postgres.feature-a.curri.stacklane.test:5432
nats.feature-a.curri.stacklane.test:4222
```

Rules:

- TLD is always `.test` (never `.local`).
- One unique loopback VIP per Compose project/stack (pool default `127.77.0.0/16`).
- All endpoint names for a stack resolve to that stack’s VIP.
- Compose services publish target ports to **random loopback-only** host ports: `"127.0.0.1::<target>"`.
- Stacklane discovers containers + actual host mappings, binds **standard ports on the stack VIP**, and proxies to those loopback ports.
- **DNS + VIPs** solve conflicts; DNS alone does not.

### User-visible MVP outcome

1. Start `stacklane serve`.
2. `docker compose up` two projects that both publish container port 5432 via `127.0.0.1::5432` with Stacklane labels.
3. Each project gets a distinct VIP; `postgres.<stack>.stacklane.test` A-records point at the correct VIP.
4. TCP to `VIP:5432` (or via DNS name) reaches the correct container backend and returns distinct responses.
5. `stacklane status` / `stacklane resolve` show the mapping read-only.
6. Stopping containers removes DNS/listeners; VIP leases are retained per policy across restarts.

---

## 2. Current state (repository evidence)

As of base `5c68ab5`:

| Path | State |
|------|--------|
| `README.md` | Stub product blurb only |
| `.gitignore` | Ignores `.stacklane/`, `.env*` |
| Go module / `cmd/` / `internal/` | **Absent** |
| CI / Makefile | **Absent** |
| Tests | **Absent** |

Everything in this plan is greenfield implementation on top of the stub README/gitignore.

---

## 3. Scope

### 3.1 In scope (MVP)

1. Go module `github.com/aleksclark/stacklane` with composition under `cmd/stacklane` and implementation under `internal/` only — **no repository-root Go source files**.
2. `stacklane serve` daemon + read-only inspection (`stacklane status`, `stacklane resolve`).
3. Docker integration:
   - Subscribe to Docker Engine container lifecycle/health events.
   - Startup full reconciliation + periodic full reconciliation.
   - Canonical labels `com.docker.compose.project` and `com.docker.compose.service`.
   - Minimal safe Stacklane label contract (project slug, optional instance slug, endpoint name/protocol/public port/container target port).
   - Inspect actual Docker-assigned loopback host ports; labels **must not** allow arbitrary host destinations.
   - One-endpoint-per-container MVP with clear validation and deterministic behavior.
4. Stable VIP allocation persisted across daemon restarts (atomic JSON by default). Crash-safe enough for MVP; mode-safe; deterministic; tested for corruption/error behavior.
5. Authoritative DNS for `test` with exact base and endpoint A records; configurable listen address/port for unprivileged testing. **Do not** modify host resolver or install system service in this PR.
6. TCP proxy on stack VIP + declared public standard port → only Docker-inspected `127.0.0.1` ephemeral host bindings. Lifecycle replacement, cancellation, bounded timeouts, half-close where practical, cleanup. HTTP as ordinary TCP OK; no hostname-specific HTTP fanout.
7. Reconciliation removes stale DNS/listeners when containers/stacks disappear; preserve stable VIPs with explicit retained lease policy.
8. Security defaults (see §14).
9. Docs: README architecture + Compose example + labels + commands + naming + limitations + Linux/macOS notes + future resolver setup marked **NOT implemented**; this plan; note future run report path.
10. Quality: `gofmt`, `go vet`, race tests, build; Makefile local `ci`; GitHub Actions CI pinned full commit SHAs; `git diff --check master...HEAD`.
11. Functional E2E: two Compose projects, same container port, different ephemeral loopback + different VIPs; prove DNS A + TCP distinct responses. Fake-Docker integration tests always. No fabricated PASS.

### 3.2 Explicit non-goals

- Headscale, WireGuard, Nomad, or any overlay/mesh networking
- TLS / local CA / HTTPS termination
- UDP proxy (including DNS proxying of upstream)
- Host resolver installer / system package / launchd/systemd unit installation in this PR
- GUI / TUI
- Production packaging (Homebrew, AUR, deb/rpm)
- Multi-endpoint-per-container
- Remote control API / HTTP admin API
- Arbitrary host/IP targets from labels
- IPv6 VIP pool (IPv4 loopback only for MVP)
- Windows support (document as unsupported)
- Automatic `ports:` rewriting of user Compose files
- Shared stack identity across machines

---

## 4. Architecture overview and data flow

### 4.1 Components

```text
┌──────────────────────────────────────────────────────────────────────┐
│                         stacklane serve                              │
│                                                                      │
│  ┌─────────────┐   events    ┌──────────────┐   desired   ┌────────┐ │
│  │ docker.Watch│────────────▶│  reconcile.  │────────────▶│  dns   │ │
│  │  + Inspect  │  full snap  │  Engine      │  records    │ Server │ │
│  └─────────────┘             │              │────────────▶│        │ │
│         │                    │  - validate  │  endpoints  └────────┘ │
│         │ inspect            │  - allocate  │────────────▶┌────────┐ │
│         ▼                    │  - diff      │  listeners  │ proxy  │ │
│  HostPortBindings            │  - apply     │             │Manager │ │
│  (127.0.0.1 only)            └──────┬───────┘             └────────┘ │
│                                     │                                │
│                                     ▼                                │
│                              ┌──────────────┐                        │
│                              │ state.Store  │  atomic JSON           │
│                              │ VIP leases   │  ~/.stacklane/state    │
│                              └──────────────┘                        │
│                                                                      │
│  CLI: status / resolve  ──read──▶ in-memory snapshot (+ store)       │
└──────────────────────────────────────────────────────────────────────┘
```

### 4.2 Data flow (happy path)

1. **Discover:** On start, list all containers; filter those with required Compose + Stacklane labels and a running/healthy-enough state.
2. **Inspect ports:** For each candidate, read `NetworkSettings.Ports` (or equivalent SDK fields). Accept only host IP `127.0.0.1` (or empty host IP treated as all-interfaces — **reject** unless host IP is exactly `127.0.0.1`). Match `HostPort` for the labeled container target port/protocol.
3. **Identity:** Build stack key and endpoint FQDN from labels (§6).
4. **Allocate VIP:** Lookup durable lease by stack key; if missing, allocate next free VIP from pool; persist before advertising.
5. **Desired state:** For each valid endpoint, desired = `{fqdn, vip, publicPort, target=127.0.0.1:hostPort, protocol=tcp}`.
6. **Apply DNS:** Upsert A records for stack base name and each endpoint name → VIP. Remove records not in desired set (except policy-retained empty stacks — base A may remain while lease retained; see §7.4).
7. **Apply proxy:** Ensure TCP listener on `vip:publicPort`; proxy to inspected target. Replace on target change; close on removal.
8. **Events:** Container start/stop/die/destroy/health_status → debounced reconcile trigger.
9. **Periodic:** Full reconcile every N seconds (default 15s) to heal missed events.
10. **Shutdown:** Cancel proxies, close DNS, flush store; VIP leases remain on disk.

### 4.3 Core domain types (contracts)

```go
// internal/domain/types.go (illustrative — implement under internal/domain)

type StackKey string // stable identity: projectSlug[/instanceSlug]

type Stack struct {
    Key          StackKey
    ProjectSlug  string
    InstanceSlug string // empty if unused
    ComposeProject string // raw com.docker.compose.project
    VIP          netip.Addr
}

type Endpoint struct {
    StackKey     StackKey
    Name         string // label endpoint name, e.g. "postgres"
    FQDN         string // postgres.feature-a.curri.stacklane.test
    Protocol     Protocol // tcp only in MVP
    PublicPort   uint16
    TargetPort   uint16 // container port (for matching published binding)
    TargetHost   netip.Addr // must be 127.0.0.1
    TargetBind   uint16 // docker-assigned host port
    ContainerID  string
    ServiceName  string // com.docker.compose.service
}

type Protocol string // "tcp"

type DesiredState struct {
    Stacks    map[StackKey]Stack
    Endpoints map[string]Endpoint // key = FQDN + "|" + port + "|" + protocol
}
```

---

## 5. Exact package layout

**Rule:** No `*.go` files at repository root. All implementation under `internal/`. Composition root under `cmd/stacklane`.

```text
github.com/aleksclark/stacklane
├── cmd/
│   └── stacklane/
│       ├── main.go                 # os.Exit(run())
│       └── root.go                 # cobra/flag wiring OR stdlib flag subcommands
├── internal/
│   ├── app/
│   │   ├── serve.go                # daemon wiring, lifecycle, signal handling
│   │   ├── status.go               # status command implementation
│   │   └── resolve.go              # resolve command implementation
│   ├── config/
│   │   ├── config.go               # Config struct, defaults, validation
│   │   ├── load.go                 # flags + env + optional file merge
│   │   └── config_test.go
│   ├── domain/
│   │   ├── types.go                # Stack, Endpoint, DesiredState, errors
│   │   ├── names.go                # FQDN construction
│   │   ├── names_test.go
│   │   ├── validate.go             # shared validation helpers
│   │   └── validate_test.go
│   ├── labels/
│   │   ├── labels.go               # parse/validate Stacklane + Compose labels
│   │   ├── contract.go             # key constants, limits, charset
│   │   └── labels_test.go
│   ├── dockerapi/
│   │   ├── client.go               # Docker client interface + SDK adapter
│   │   ├── inspect.go              # port binding extraction (loopback-only)
│   │   ├── watch.go                # events subscription + decode
│   │   ├── fake/
│   │   │   └── fake.go             # in-memory fake for tests
│   │   └── *_test.go
│   ├── vip/
│   │   ├── pool.go                 # CIDR pool iteration, allocation
│   │   ├── allocator.go            # lease-aware allocator
│   │   └── *_test.go
│   ├── state/
│   │   ├── store.go                # Store interface
│   │   ├── jsonstore.go            # atomic JSON implementation
│   │   ├── schema.go               # versioned on-disk schema
│   │   └── *_test.go
│   ├── dns/
│   │   ├── server.go               # authoritative server
│   │   ├── records.go              # record set / zone view
│   │   └── *_test.go
│   ├── proxy/
│   │   ├── manager.go              # listener set lifecycle
│   │   ├── tcp.go                  # single TCP proxy conn handling
│   │   └── *_test.go
│   ├── reconcile/
│   │   ├── engine.go               # event+periodic reconcile loop
│   │   ├── build.go                # containers → DesiredState
│   │   ├── apply.go                # diff + apply dns/proxy/state
│   │   └── *_test.go
│   └── version/
│       └── version.go              # build-time version vars
├── testdata/
│   └── compose/
│       ├── stack-a/docker-compose.yml
│       └── stack-b/docker-compose.yml
├── docs/
│   ├── plans/
│   │   └── initial-mvp.md          # THIS FILE
│   └── reviews/
│       └── .gitkeep                # placeholder; run report written later
├── .github/
│   └── workflows/
│       └── ci.yml
├── Makefile
├── go.mod
├── go.sum
├── .gitignore
└── README.md
```

### Package responsibilities

| Package | Responsibility | Must not |
|---------|----------------|----------|
| `cmd/stacklane` | CLI entry, flag parse, call `internal/app` | Business logic, Docker SDK details |
| `internal/app` | Wire components for serve/status/resolve | Low-level DNS/proxy algorithms |
| `internal/config` | Defaults, validation, env/flags/file | I/O beyond loading config |
| `internal/domain` | Pure types, naming, validation primitives | Docker, filesystem, network |
| `internal/labels` | Label contract parse/validate | Port inspection, VIP allocation |
| `internal/dockerapi` | SDK + Fake, events, inspect bindings | DNS/proxy side effects |
| `internal/vip` | Pool + allocation algorithms | Persistence I/O (uses store interface) |
| `internal/state` | Durable leases, atomic write, corruption handling | Docker/DNS |
| `internal/dns` | Authoritative A/SOA/NS behavior | Proxy, Docker |
| `internal/proxy` | TCP listen/dial/copy/cancel | DNS, VIP policy |
| `internal/reconcile` | Orchestrate build+apply; single writer of desired runtime | CLI formatting |
| `internal/version` | Version string only | Anything else |

---

## 6. Label contract

### 6.1 Canonical Docker Compose labels (read-only inputs)

| Key | Required | Source |
|-----|----------|--------|
| `com.docker.compose.project` | **Yes** | Compose engine |
| `com.docker.compose.service` | **Yes** | Compose engine |
| `com.docker.compose.project.working_dir` | No | Ignored in MVP |
| `com.docker.compose.container-number` | No | Ignored; multi-replica not supported |

Absence of either required Compose label → container ignored (not an error log at warn once per id).

### 6.2 Stacklane labels (exact key names)

Prefix: `stacklane.`

| Key | Required | Type | Description |
|-----|----------|------|-------------|
| `stacklane.enable` | **Yes** | enum | Must be `1` or `true` (case-sensitive lowercase `true` or `1`). Any other value / missing → ignore container. |
| `stacklane.project` | **Yes** | slug | Project slug used in DNS hierarchy. |
| `stacklane.instance` | No | slug | Optional instance/org segment (e.g. `curri`). If empty, FQDN omits this label segment only if `dns.default_instance` config is empty; see naming. |
| `stacklane.endpoint` | **Yes** | slug | Endpoint hostname label (e.g. `postgres`). |
| `stacklane.protocol` | No | enum | Default `tcp`. Only `tcp` allowed in MVP. |
| `stacklane.port` | **Yes** | uint16 | **Public** port on the stack VIP clients connect to (e.g. `5432`). |
| `stacklane.target_port` | No | uint16 | Container port to match in Docker port bindings. Default = `stacklane.port`. |

**Forbidden labels / behaviors:**

- No `stacklane.host`, `stacklane.target_host`, `stacklane.target_addr`, `stacklane.vip` — targets and VIPs are never label-controlled.
- Labels must not embed IPs or host:port destinations.
- Unknown `stacklane.*` keys: ignore with debug log (forward compatible), do not fail the endpoint unless a required key is invalid.

### 6.3 Slug validation rules

Apply to `stacklane.project`, `stacklane.instance`, `stacklane.endpoint`:

| Rule | Value |
|------|-------|
| Charset | `a-z`, `0-9`, hyphen `-` only |
| Must match regex | `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$` (single DNS label form) **or** single char `^[a-z0-9]$` |
| Min length | 1 |
| Max length | 63 (DNS label limit) |
| No leading/trailing hyphen | Enforced by regex |
| No uppercase | Reject (do not auto-lowercase — fail closed) |
| No underscores | Reject |
| No dots inside slug | Reject (dots only from FQDN joining) |

### 6.4 Port validation

| Field | Rules |
|-------|-------|
| `stacklane.port` | Integer 1–65535; reject 0; reject non-numeric; reject leading `+`/spaces |
| `stacklane.target_port` | Same; default to public port if omitted |
| Protocol | Only `tcp` (case-sensitive). `udp`/`http`/`https` → reject endpoint |

### 6.5 Enable flag

Accepted: `1`, `true`.
Rejected (ignore container, warn): empty, `yes`, `TRUE`, `True`, `on`, `enabled`, `0`, `false`.

### 6.6 Max lengths / size caps (fail closed)

| Item | Max |
|------|-----|
| Any single label value | 128 bytes |
| Total labels map entries considered | 64 stacklane.* keys scanned |
| Compose project string retained | 128 bytes (truncate not allowed — reject if over) |
| Container ID | 64 hex (Docker standard); accept 12–64 hex |

### 6.7 One-endpoint-per-container

- Exactly one logical endpoint per container in MVP.
- If multiple published bindings match `target_port/tcp`, pick the binding with host IP `127.0.0.1` and, if multiple, the **lowest host port** (deterministic). Log warn if >1 loopback binding for same target.
- If no loopback binding for target port → endpoint invalid; skip; warn.
- If host binding is `0.0.0.0`, `::`, or non-loopback → **reject** (do not proxy to non-loopback). Document that Compose must use `"127.0.0.1::<target>"`.

### 6.8 DNS naming from labels + config

Config:

- `dns.base_domain` default `test`
- `dns.default_instance` default empty string; if set (e.g. `curri`), used when `stacklane.instance` absent

FQDN construction:

```text
endpoint = stacklane.endpoint
project  = stacklane.project
instance = stacklane.instance if set else config.dns.default_instance

if instance != "":
  endpoint_fqdn = "{endpoint}.{project}.{instance}.{base_domain}"
  stack_fqdn    = "{project}.{instance}.{base_domain}"
else:
  endpoint_fqdn = "{endpoint}.{project}.{base_domain}"
  stack_fqdn    = "{project}.{base_domain}"
```

Examples with `default_instance=curri` or label instance `curri`:

```text
postgres.feature-a.curri.stacklane.test
feature-a.curri.stacklane.test
```

**Stack key** (VIP identity):

```text
if instance != "":
  stack_key = "{project}/{instance}"
else:
  stack_key = "{project}"
```

Compose project name is **not** the stack key (Compose names can be directory-derived and collide across worktrees). Stacklane project+instance labels define identity. Still require Compose labels to ensure we only manage Compose-created containers.

### 6.9 Conflict rules (deterministic)

When building desired state:

1. Two containers same `endpoint_fqdn` + `publicPort` + `protocol` → keep the one with lexicographically smaller `container_id`; warn on loser.
2. Two different stack keys claiming same VIP already allocated — should be impossible if allocator is correct; on load corruption see §10.
3. Same container valid → one endpoint only.

### 6.10 Example Compose service

```yaml
services:
  db:
    image: postgres:16
    labels:
      stacklane.enable: "true"
      stacklane.project: "feature-a"
      stacklane.instance: "curri"
      stacklane.endpoint: "postgres"
      stacklane.port: "5432"
      stacklane.target_port: "5432"
      stacklane.protocol: "tcp"
    ports:
      - "127.0.0.1::5432"
```

---

## 7. VIP pool, allocation, persistence, lease retention

### 7.1 Pool

| Setting | Default | Notes |
|---------|---------|-------|
| `vip.pool_cidr` | `127.77.0.0/16` | Must be IPv4 loopback range contained in `127.0.0.0/8` |
| Excluded addresses | network & broadcast if applicable; always exclude `0` and `255` in last octet when iterating /16 as two-byte counter — implement as: iterate host bits, skip addr equal to prefix network address | Practical: treat pool as enumerable set of unicast addresses in CIDR, skip `.0` and `.255` in each /24 for broader OS compatibility |

**Validation:**

- CIDR must parse as IPv4.
- Every address in pool must satisfy `addr.IsLoopback()`.
- Prefix length must be between `/16` and `/30` inclusive for MVP (avoid huge iteration and tiny pools).
- Refuse to start if pool has fewer than 2 usable addresses.

### 7.2 Allocation algorithm

```text
function Allocate(stackKey):
  if lease exists for stackKey and VIP still in pool and not owned by other key:
    touch LastSeenAt; return VIP
  if lease exists but VIP outside current pool:
    drop lease; continue allocate new
  for addr in pool.UsableSorted():  # deterministic ascending
    if addr not in allocatedSet:
      create lease {StackKey, VIP: addr, CreatedAt, LastSeenAt, LastActiveAt}
      persist synchronously
      return addr
  return error ErrPoolExhausted
```

- Allocation order is **deterministic** (ascending numeric address).
- Never reuse a VIP still leased to another stack key.
- Persist **before** DNS/proxy advertise (crash between persist and advertise → restart reloads lease; harmless).

### 7.3 Lease record fields

```json
{
  "version": 1,
  "leases": [
    {
      "stack_key": "feature-a/curri",
      "vip": "127.77.0.1",
      "created_at": "2026-08-11T12:00:00Z",
      "last_seen_at": "2026-08-11T12:05:00Z",
      "last_active_at": "2026-08-11T12:05:00Z"
    }
  ]
}
```

- `last_seen_at`: updated whenever reconcile observes stack key (has ≥0 endpoints from live containers belonging to key — including zero after containers gone still updates during grace? **No** — see retention).
- `last_active_at`: updated only when stack has ≥1 valid live endpoint.

### 7.4 Lease retention policy (explicit)

| Condition | DNS base A | Proxies | VIP lease |
|-----------|------------|---------|-----------|
| ≥1 live valid endpoint | Present (stack + endpoints) | Present | Kept; `last_active_at=now`; `last_seen_at=now` |
| 0 live endpoints, within grace | **Remove all records for stack** | None | **Kept** until grace expires |
| 0 live endpoints, grace expired | None | None | **Released** (VIP reusable) |
| Daemon restart mid-grace | None until endpoints return | None | Kept if not expired on load |

**Grace duration:** `vip.lease_grace` default **24h** (`24h`).

Expiry rule on each reconcile and on store load:

```text
if now - lease.LastActiveAt >= lease_grace:
  release lease (delete from store)
```

If stack never had `last_active_at` (should not happen), treat `created_at` as active baseline.

**Rationale:** Stable VIPs across container restarts and short downtime; automatic reclaim so pool does not permanently leak.

### 7.5 Mode safety

- File mode `0600` for state file; directory `0700`.
- Refuse to load if permissions are group/world-readable (warn+continue on platforms where chmod semantics differ? **Linux: fail closed with error**; document macOS same).
- No follow of symlinks on write path: open with `O_NOFOLLOW` where available; on write use temp file in same dir + `Rename`.

---

## 8. DNS record model and query behavior

### 8.1 Server role

- Authoritative only for `dns.base_domain` (default `test`) and names beneath it.
- Does **not** recurse or forward.
- Configurable `dns.listen` default `127.0.0.1:15353` (unprivileged testing). Production doc note: `:53` requires cap/root — **not auto-configured**.

### 8.2 Records published

For each active stack with ≥1 endpoint:

| Name | Type | RDATA | TTL |
|------|------|-------|-----|
| `{stack_fqdn}` | A | stack VIP | `dns.ttl` default 5 |
| `{endpoint_fqdn}` | A | stack VIP | same |

Also static zone apex records:

| Name | Type | RDATA |
|------|------|-------|
| `stacklane.test` (base) | SOA | mname `ns.stacklane.test`, rname `hostmaster.stacklane.test`, serial from state generation counter, refresh 3600, retry 600, expire 86400, minimum 5 |
| `test` | NS | `ns.stacklane.test` |
| `ns.stacklane.test` | A | DNS listen IP if in loopback; else `127.0.0.1` |

MVP may serve SOA/NS minimally to be a polite auth server; tests must cover A behavior primarily.

### 8.3 Query behavior matrix

| Query | Condition | Response |
|-------|-----------|----------|
| A `postgres.feature-a.curri.stacklane.test` | endpoint active | NOERROR + A VIP |
| A `feature-a.curri.stacklane.test` | stack active (≥1 ep) | NOERROR + A VIP |
| A `unknown.feature-a.curri.stacklane.test` | stack exists, name not | **NXDOMAIN** |
| A `other.stacklane.test` | no stack | **NXDOMAIN** |
| A `example.com` | out of zone | REFUSED or ignore (not authoritative) — implement **REFUSED** |
| AAAA any in-zone name | | NOERROR empty ANSWER (no AAAA) **or** NXDOMAIN if name doesn't exist. **Contract:** if name has A, AAAA → NOERROR, empty answer, AA bit set. If name unknown → NXDOMAIN. |
| TXT/MX/etc in-zone existing name | | NOERROR empty answer |
| TXT unknown name | | NXDOMAIN |
| PTR | | not supported; NXDOMAIN or REFUSED — **REFUSED** |

**AA bit:** set on all authoritative answers in zone.

**Case:** respond with question case; match case-insensitive.

### 8.4 Concurrency

- Record set replaced atomically via `atomic.Pointer` or RWMutex.
- Reconcile never mutates map in place without lock.

---

## 9. Proxy lifecycle and safety invariants

### 9.1 Listener identity

```text
ListenerKey = vip.String() + "|" + publicPort + "|" + protocol
```

Each key → at most one `net.Listener`.

### 9.2 Backend

```text
Backend = "127.0.0.1:" + hostPort
```

**Invariants (must be tested):**

1. Dial address host **must** be `127.0.0.1` IPv4 loopback. Reject construction otherwise.
2. Never dial Unix sockets, non-loopback IPs, or hostnames.
3. Public bind address **must** be the allocated VIP (in pool), not `0.0.0.0`.
4. On backend change for same ListenerKey: start new dial target for new conns; existing conns finish or cancel within `proxy.shutdown_timeout` (default 5s).
5. On remove: stop accept loop, close listener, cancel all active conn contexts, wait up to shutdown timeout.
6. Each inbound conn: `DialContext` with `proxy.dial_timeout` default 3s; bidirectional `io.Copy` with optional idle timeout `proxy.idle_timeout` default 5m; half-close via `CloseWrite` when one side EOF (TCPConn type assert).
7. Max concurrent conns global `proxy.max_conns` default 1024; excess accept and immediately close.
8. No TLS; no HTTP parsing; no host-header routing.

### 9.3 Error handling

- Listen failures (EADDRNOTAVAIL if VIP not assigned on host): log error; mark endpoint degraded in status; retry on next reconcile. **MVP VIP bring-up:** see §9.4.
- Dial failures: close inbound; optional counter metric log at debug.

### 9.4 Loopback VIP address assignment

Linux: adding `127.77.x.y` typically works without extra config (loopback accepts whole `127.0.0.0/8`).

macOS: lo0 may need `sudo ifconfig lo0 alias 127.77.x.y`. MVP behavior:

- Attempt `Listen` on VIP; if fail with addr not available, log clear error with platform hint.
- Optional best-effort `vip.ensure` using `exec` of `ip addr add` / `ifconfig` **gated off by default** (`vip.auto_alias=false`). Document manual alias. Do not require root for core tests (use `127.0.0.1` pool of size 1 only in unit tests with free ports — integration tests on Linux use real 127.77/16).

---

## 10. State store schema and crash/corruption behavior

### 10.1 Choice: atomic JSON (default)

**Justification:** MVP state is a small lease table; JSON + temp/rename is easy to test, zero CGO, no SQLite driver dependency. Revisit SQLite only if concurrent writers appear (they will not — single daemon).

### 10.2 Paths

| Item | Default |
|------|---------|
| State dir | `$STACKLANE_STATE_DIR` or `~/.stacklane` |
| State file | `{state_dir}/state.json` |
| Temp file | `{state_dir}/state.json.tmp` |
| Lock file | `{state_dir}/daemon.lock` (serve exclusive) |

### 10.3 Schema version

```json
{
  "version": 1,
  "leases": [ /* Lease */ ]
}
```

Unknown future version → refuse start with explicit error.
Version missing / 0 → treat as corrupt.

### 10.4 Write protocol

1. Acquire in-process mutex.
2. Serialize full snapshot.
3. Write temp file `O_CREATE|O_TRUNC|O_WRONLY` mode 0600.
4. `Sync` file.
5. `Rename` temp over final (atomic on same FS).
6. `Sync` directory (best effort; Linux).

### 10.5 Load protocol / corruption

| Scenario | Behavior |
|----------|----------|
| File missing | Empty leases; OK |
| Empty file | Corrupt → error unless `--state-reset-on-corrupt` (default **false**) |
| Invalid JSON | Error exit on serve (fail closed) |
| Wrong types / invalid VIP | Error exit listing field |
| Duplicate stack_key | Error exit |
| Duplicate VIP | Error exit |
| VIP outside pool | Drop lease with warn (recoverable) |
| Version mismatch | Error exit |
| Permission too open | Error exit on Linux |

**Tests required:** each corruption case with golden broken files under `testdata/state/`.

### 10.6 Daemon lock

- `serve` takes exclusive flock on `daemon.lock`.
- Second serve exits with clear error.
- `status`/`resolve` read state file + optional optional HTTP — **MVP:** status talks to running daemon via local read-only unix socket **or** reads state + best-effort DNS?

**CLI inspection contract (MVP decision):**

- `stacklane serve` exposes a **local read-only Unix socket** control endpoint at `{state_dir}/stacklane.sock` mode 0600, serving a tiny line/JSON protocol **or** simply: status/resolve are implemented by reading the same in-memory snapshot via socket RPC.
- Minimal RPC (no remote API):
  - `GET /status` style over HTTP on Unix socket **OR** JSON request `{"op":"status"}`.
  - Prefer **HTTP over Unix socket** with stdlib only: `GET http://unix/status`, `GET http://unix/resolve?name=...`.
- If daemon not running: `status` exits 1 with message; may still print durable leases from state file under a `--offline` flag (optional). Default: require daemon.

This is **not** a remote control API; bind only Unix socket path on filesystem, never TCP admin port.

---

## 11. Reconciliation algorithm

### 11.1 Triggers

1. Startup (immediate full).
2. Docker events: `container` type actions `start`, `die`, `destroy`, `stop`, `pause`, `unpause`, `health_status`, `rename` — all schedule reconcile.
3. Periodic ticker: default `reconcile.interval=15s`.
4. Debounce: coalesce bursts to `reconcile.debounce=250ms`.

### 11.2 Algorithm (single-threaded apply)

```text
loop:
  wait trigger
  ctx = with timeout reconcile.timeout (default 30s)
  containers = docker.List(ctx, all=false running only? list running + recently stopped no)
             // List running containers; also inspect any id from event if needed
  desired = empty DesiredState
  for c in containers:
    meta = labels.Parse(c.Labels)
    if !meta.OK: continue
    bind = docker.LoopbackBinding(c, meta.TargetPort, meta.Protocol)
    if bind missing: warn; continue
    stack = upsert stack with vip.Allocate(meta.StackKey)
    desired.add endpoint
  // Lease maintenance
  for each lease in store:
    if stack has active endpoints: touch active
    else if expired: release
  // Diff apply
  dns.Replace(records from desired active stacks)
  proxy.Reconcile(desired endpoints)
  store.Save(leases)
  snapshot.Publish(desired) // for status socket
```

### 11.3 Running filter

- Only `State.Running == true`.
- If healthcheck exists and status is `unhealthy`, still publish MVP (document); optional future gate `stacklane.require_healthy=true` non-goal.

### 11.4 Stale removal

- Endpoints not in desired → remove DNS names + proxy listeners.
- Stacks with no endpoints → remove DNS immediately; keep lease until grace.

### 11.5 Failure isolation

- One bad container must not fail whole reconcile: per-container error catch, continue.
- Store save failure: log error; keep memory; retry next loop (do not remove live proxies solely because save failed).

---

## 12. CLI surface

### 12.1 Commands

```text
stacklane serve [flags]
stacklane status [flags]
stacklane resolve <name> [flags]
stacklane version
stacklane help
```

### 12.2 Global flags

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `--state-dir` | `STACKLANE_STATE_DIR` | `~/.stacklane` | State directory |
| `--config` | `STACKLANE_CONFIG` | empty | Optional config file path |

### 12.3 `serve` flags

| Flag | Env | Default |
|------|-----|---------|
| `--docker-host` | `DOCKER_HOST` | SDK default |
| `--vip-pool` | `STACKLANE_VIP_POOL` | `127.77.0.0/16` |
| `--vip-lease-grace` | `STACKLANE_VIP_LEASE_GRACE` | `24h` |
| `--dns-listen` | `STACKLANE_DNS_LISTEN` | `127.0.0.1:15353` |
| `--dns-base-domain` | `STACKLANE_DNS_BASE_DOMAIN` | `test` |
| `--dns-default-instance` | `STACKLANE_DNS_DEFAULT_INSTANCE` | empty |
| `--dns-ttl` | `STACKLANE_DNS_TTL` | `5` |
| `--reconcile-interval` | `STACKLANE_RECONCILE_INTERVAL` | `15s` |
| `--reconcile-debounce` | `STACKLANE_RECONCILE_DEBOUNCE` | `250ms` |
| `--proxy-dial-timeout` | | `3s` |
| `--proxy-idle-timeout` | | `5m` |
| `--proxy-shutdown-timeout` | | `5s` |
| `--proxy-max-conns` | | `1024` |
| `--log-level` | `STACKLANE_LOG_LEVEL` | `info` |
| `--state-reset-on-corrupt` | | `false` |

### 12.4 Config file (optional)

- Format: JSON or TOML — **JSON only for MVP** to avoid deps: `stacklane.json`.
- Merge order (later wins): defaults < config file < env < flags.

Example:

```json
{
  "vip_pool": "127.77.0.0/16",
  "vip_lease_grace": "24h",
  "dns_listen": "127.0.0.1:15353",
  "dns_base_domain": "test",
  "dns_default_instance": "curri",
  "reconcile_interval": "15s"
}
```

### 12.5 `status` output

Human-readable text (stable enough for tests with `-o json`):

```text
Daemon: running
DNS: 127.0.0.1:15353 base=test
VIP pool: 127.77.0.0/16 (12 leased)

STACK feature-a/curri  vip=127.77.0.1  endpoints=1  lease=active
  postgres.feature-a.curri.stacklane.test  127.77.0.1:5432 -> 127.0.0.1:32768  container=abc123  service=db
```

JSON schema (`-o json`):

```json
{
  "daemon": "running",
  "dns_listen": "127.0.0.1:15353",
  "base_domain": "test",
  "stacks": [
    {
      "key": "feature-a/curri",
      "vip": "127.77.0.1",
      "endpoints": [
        {
          "fqdn": "postgres.feature-a.curri.stacklane.test",
          "public_port": 5432,
          "target": "127.0.0.1:32768",
          "container_id": "abc123…",
          "service": "db"
        }
      ]
    }
  ]
}
```

### 12.6 `resolve`

```bash
stacklane resolve postgres.feature-a.curri.stacklane.test
# postgres.feature-a.curri.stacklane.test -> 127.77.0.1
# exit 0

stacklane resolve missing.stacklane.test
# NXDOMAIN / not found
# exit 1
```

Uses daemon snapshot (not system resolver), so tests work without host DNS config.

---

## 13. Dependency choices

| Dependency | Use | Justification |
|------------|-----|---------------|
| `github.com/docker/docker` (+ `docker/go-connections`, `docker/go-units` as required by SDK) | Engine API list/inspect/events | Official SDK; pin exact version in go.mod |
| `github.com/miekg/dns` | Authoritative DNS server | Stdlib has no DNS server; miekg/dns is the de-facto narrow choice |
| `golang.org/x/sync` | errgroup if needed | Optional; prefer stdlib context |
| Cobra/cli toolkit | **No** for MVP | stdlib `flag` + manual subcommands OR single dependency `github.com/spf13/cobra` only if flag UX becomes painful — **default: stdlib** |
| SQLite | **No** by default | Atomic JSON sufficient |
| OpenTelemetry / metrics | **No** | Logs only |
| gRPC/HTTP frameworks | **No** | Unix socket + net/http stdlib |

Pin versions with full module pseudo-versions / semver as resolved by `go mod tidy`; CI uses committed `go.sum`.

---

## 14. Security checklist

- [ ] Bind proxies only on allocated VIPs inside configured loopback pool.
- [ ] DNS listen default loopback only.
- [ ] Reject malformed/overlong labels; reject invalid ports; fail closed.
- [ ] Never take dial targets from labels; only Docker-inspected `127.0.0.1` bindings.
- [ ] Reject non-loopback published bindings.
- [ ] No remote TCP admin API; Unix socket mode 0600 only.
- [ ] State file 0600, dir 0700; refuse world-readable state on load (Linux).
- [ ] Document Docker socket as **root-equivalent**.
- [ ] Never log env vars, registry auth, container env, secrets, or full inspect dumps at info level.
- [ ] Sanitize container names/labels in logs (length caps).
- [ ] Daemon lock prevents two writers.
- [ ] No privilege escalation helpers enabled by default (`vip.auto_alias=false`).
- [ ] CI and Makefile run tests with `-race`.
- [ ] `git diff --check` clean (no trailing whitespace).

---

## 15. E2E design

### 15.1 Fake-Docker integration (always runs in CI)

Location: `internal/reconcile` + `internal/app` tests using `dockerapi/fake`.

Scenarios:

1. Two stacks, same public port 5432, different host ports 40001/40002 → two VIPs, two listeners, DNS A distinct.
2. Container stop → DNS/proxy removed; lease remains within grace.
3. Grace expiry → VIP reallocated to new stack possible.
4. Invalid labels ignored.
5. Non-loopback binding rejected.
6. Restart simulator: reload store → same VIP.
7. Corrupt state → serve error.
8. Endpoint target port change → proxy backend updates.
9. Conflict same FQDN → deterministic winner.

Fake must simulate List + Events + Inspect port maps; no real Docker required.

### 15.2 Real Docker E2E (gated)

- Tag: `//go:build e2e`
- Makefile target: `make e2e` (not default `ci` if no Docker; but `make ci` runs unit/fake).
- If `DOCKER_HOST` reachable and `docker info` OK, `make ci` may run e2e optionally via `E2E=1`.

**Scenario:**

1. Start `stacklane serve` with test state dir and DNS on `127.0.0.1:1853`.
2. `docker compose -p sl_a -f testdata/compose/stack-a up -d` with labels project `alpha`, instance `curri`, endpoint `app`, port 8080; container runs `nc`/`python` HTTP returning `A`.
3. Same for stack-b project `beta` returning `B`.
4. Both publish `127.0.0.1::8080`.
5. Assert:
   - `stacklane resolve app.alpha.curri.stacklane.test` → VIP_A
   - `stacklane resolve app.beta.curri.stacklane.test` → VIP_B
   - VIP_A ≠ VIP_B
   - dig/miekg query against dns-listen matches
   - `curl http://VIP_A:8080` → `A`, `curl http://VIP_B:8080` → `B`
6. Tear down compose + serve; cleanup state dir.

**No fabricated PASS:** test fails if Docker unavailable when `E2E=1`; when `E2E` unset, skip real e2e.

### 15.3 TCP distinct responses without full HTTP stack

Backend can be a tiny `net.Listener` in a container (`hashicorp/http-echo` or `python -m http.server` with unique index files). Prefer `hashicorp/http-echo` pin by digest in compose if network allows; else build local Dockerfile under testdata.

---

## 16. Quality gates and acceptance commands

### 16.1 Makefile targets

```make
.PHONY: fmt vet test race build ci e2e

fmt:
        gofmt -w $(shell find cmd internal -name '*.go')

vet:
        go vet ./...

test:
        go test ./...

race:
        go test -race ./...

build:
        go build -o bin/stacklane ./cmd/stacklane

ci: vet race build
        git diff --check
        test -z "$$(gofmt -l cmd internal)"

e2e:
        E2E=1 go test -race -tags=e2e ./internal/app -count=1
```

### 16.2 GitHub Actions

`.github/workflows/ci.yml`:

- Trigger: push + pull_request
- Runner: `ubuntu-latest`
- Checkout action pinned to **full commit SHA**
- Setup-go action pinned to **full commit SHA**
- Go version: `1.22.x` or project-chosen stable
- Steps: `make ci`
- Optional e2e job with Docker available

### 16.3 Acceptance checklist (definition of done)

```bash
# From worktree root
make ci
go test -race ./...
# With Docker:
make e2e

# Manual smoke
./bin/stacklane serve --state-dir /tmp/sl-test --dns-listen 127.0.0.1:1853
# compose up two stacks…
./bin/stacklane status -o json
./bin/stacklane resolve app.alpha.curri.stacklane.test
```

Must demonstrate:

1. Two projects, same container port, different ephemeral loopback ports, different VIPs.
2. DNS A records correct via daemon DNS listen.
3. TCP responses distinct per VIP.
4. Restart daemon: VIPs stable.
5. Stop containers: proxies/DNS gone; leases retained < 24h.
6. No root Go files; module path correct.
7. README documents limitations + resolver **NOT implemented**.

### 16.4 Post-implementation run report (later)

Write evidence to `docs/reviews/initial-implementation-run.md` after implementation (not part of plan commit).

---

## 17. Parallel implementer ownership matrix

Lanes work in parallel on non-overlapping paths. Shared surfaces merge in order.

### 17.1 Lane ownership

| Lane | Owns (exclusive write) | Produces |
|------|------------------------|----------|
| **Lane A — Domain & Labels** | `internal/domain/**`, `internal/labels/**`, `internal/config/**` | Validated config + label parse + FQDN/stack key pure logic + tests |
| **Lane B — State & VIP** | `internal/state/**`, `internal/vip/**`, `testdata/state/**` | Allocator + atomic JSON store + corruption tests |
| **Lane C — Docker** | `internal/dockerapi/**` | SDK client, inspect loopback bindings, events, fake |
| **Lane D — DNS** | `internal/dns/**` | Authoritative server + record model + tests |
| **Lane E — Proxy** | `internal/proxy/**` | TCP manager + conn lifecycle + tests |
| **Lane F — Reconcile & App/CLI** | `internal/reconcile/**`, `internal/app/**`, `cmd/stacklane/**`, `internal/version/**` | Engine wiring, serve/status/resolve |
| **Lane G — Tooling & Docs** | `Makefile`, `.github/workflows/**`, `README.md`, `testdata/compose/**`, `docs/reviews/.gitkeep` | CI, examples, docs |

### 17.2 Ordered merge of shared surfaces

Merge order (each merge must keep `make ci` green):

1. **Foundation PR/commit series (Lane A + stubs):** create `go.mod`, empty packages with interfaces agreed below, `internal/version`.
2. **Lane A** complete (domain/labels/config).
3. **Lane B** (state/vip) — depends on domain `StackKey` type only.
4. **Lane C** (dockerapi) — depends on labels types.
5. **Lane D** (dns) and **Lane E** (proxy) — parallel after domain types exist.
6. **Lane F** (reconcile/app/cli) — after A–E interfaces stable.
7. **Lane G** README/compose/CI polish — continuous but final README merge last.

### 17.3 Shared interface contracts (freeze early)

```go
// dockerapi.Client
type Client interface {
    ListRunning(ctx context.Context) ([]Container, error)
    Events(ctx context.Context) (<-chan Event, <-chan error)
    Close() error
}

// state.Store
type Store interface {
    Load() (Snapshot, error)
    Save(Snapshot) error
}

// vip.Allocator
type Allocator interface {
    Allocate(snapshot *Snapshot, key domain.StackKey) (netip.Addr, error)
    ReleaseExpired(snapshot *Snapshot, now time.Time, grace time.Duration) []domain.StackKey
}

// dns.Controller
type Controller interface {
    SetRecords(recs []Record) error
    ListenAddr() string
    Shutdown(ctx context.Context) error
}

// proxy.Manager
type Manager interface {
    Reconcile(ctx context.Context, eps []domain.Endpoint) error
    Shutdown(ctx context.Context) error
}
```

Lanes program against these interfaces; Fakes live next to production adapters.

### 17.4 File conflict rules

- Nobody except Lane F edits `cmd/stacklane/**` after initial stub.
- Nobody except Lane G edits `Makefile` / CI except to fix breakage they caused — coordinate via ordered merge.
- `go.mod` / `go.sum`: only the lane introducing a dep runs `go get` / `go mod tidy` in their branch; merge conflicts resolved by re-tidy on integrate branch.
- README: Lane G owns; other lanes open issues/comments for required sections.

---

## 18. Strict TDD phase breakdown

Each phase: **RED → GREEN → REFACTOR → COMMIT**. No production code before failing test. Commit messages below are required subjects (bodies optional).

### Phase 0 — Module skeleton & CI harness

**RED:**

- Test file `internal/version/version_test.go` expects non-empty version OR `cmd` package builds — start with `go test` failing because no module.

**GREEN:**

- Create `go.mod` (`module github.com/aleksclark/stacklane`, Go 1.22+).
- `cmd/stacklane/main.go` prints help / version.
- `internal/version/version.go`.
- Makefile with `ci`, GitHub Actions pinned SHAs.
- `docs/reviews/.gitkeep`.

**Commit:** `chore: bootstrap go module, cli stub, and ci`

**Owner:** Lane G + F stub.

---

### Phase 1 — Domain naming & validation

**RED tests (`internal/domain`, `internal/labels`):**

1. Valid labels → StackKey + FQDNs.
2. Uppercase slug rejected.
3. Overlong label rejected.
4. Invalid port 0 rejected.
5. `stacklane.enable=TRUE` rejected.
6. Missing compose project → ignore.
7. Default instance from config applied when instance label absent.
8. Protocol udp rejected.

**GREEN:** implement `labels` + `domain` pure functions.

**Commit:** `feat: add label contract and dns name construction`

**Owner:** Lane A.

---

### Phase 2 — Config load

**RED:**

- Defaults match §12.
- Flags override env override file.
- Invalid CIDR fails validation.
- Non-loopback pool rejected.

**GREEN:** `internal/config`.

**Commit:** `feat: add config loading and validation`

**Owner:** Lane A.

---

### Phase 3 — VIP pool & allocator

**RED:**

- Allocates ascending addresses.
- Same key returns same VIP.
- Different keys get different VIPs.
- Exhaustion error.
- Skip .0/.255 as specified.
- Deterministic order.

**GREEN:** `internal/vip`.

**Commit:** `feat: add deterministic loopback vip allocator`

**Owner:** Lane B.

---

### Phase 4 — Atomic JSON state store

**RED:**

- Save/load roundtrip.
- Atomic replace survives simulated crash before rename (temp left behind ignored).
- Corrupt JSON → error.
- Duplicate VIP → error.
- Permissions 0600.
- Lease grace release helper integrates with snapshot.

**GREEN:** `internal/state`.

**Commit:** `feat: add crash-safe vip lease state store`

**Owner:** Lane B.

---

### Phase 5 — Docker fake + binding inspect

**RED:**

- Parse port map only `127.0.0.1` host IP.
- Reject `0.0.0.0`.
- Match target port/protocol.
- Deterministic lowest host port on duplicates.
- Fake events deliver start/die.

**GREEN:** `internal/dockerapi` + `fake`.

**Commit:** `feat: add docker inspect and event watch with fake`

**Owner:** Lane C.

---

### Phase 6 — DNS authoritative server

**RED:**

- A record hit NOERROR.
- Unknown name NXDOMAIN.
- Out-of-zone REFUSED.
- AAAA on existing A name → NOERROR empty.
- Concurrent SetRecords during query safe (race detector).
- Case-insensitive match.

**GREEN:** `internal/dns` using `miekg/dns`.

**Commit:** `feat: add authoritative stacklane.test dns server`

**Owner:** Lane D.

---

### Phase 7 — TCP proxy manager

**RED:**

- Listen on 127.x VIP:port (or 127.0.0.1 in unit test) proxies bytes both ways.
- Backend must be loopback or constructor errors.
- Reconcile removes stale listener.
- Cancel in-flight on shutdown.
- Half-close behavior unit test with local pipes/listeners.
- Max conns enforced.

**GREEN:** `internal/proxy`.

**Commit:** `feat: add tcp proxy manager with safe loopback dial`

**Owner:** Lane E.

---

### Phase 8 — Reconcile engine (fake docker)

**RED integration:**

- Two stacks full path: allocate, dns, proxy.
- Remove container → dns/proxy gone, lease kept.
- Restart: new engine loads store → same VIP.
- Bad container does not block good one.
- Debounce coalesces (fake clock if needed).

**GREEN:** `internal/reconcile`.

**Commit:** `feat: add reconcile engine wiring dns proxy and vip leases`

**Owner:** Lane F.

---

### Phase 9 — CLI serve/status/resolve

**RED:**

- `serve` starts with fake docker in test hook OR build tags.
- `status -o json` shows endpoints.
- `resolve` exit codes.
- Second serve lock fails.
- Does not log secrets (assert log buffer without env dump).

**GREEN:** `internal/app`, finish `cmd/stacklane`.

**Commit:** `feat: add serve status and resolve commands`

**Owner:** Lane F.

---

### Phase 10 — Real Docker E2E + README

**RED:**

- e2e test fails without stacks.
- Bring up compose fixtures → pass.

**GREEN:**

- `testdata/compose/**`
- README architecture, example, limitations, Linux/macOS notes, resolver **NOT implemented**.
- `make e2e`

**Commit:** `docs: add readme examples and real docker e2e`

**Owner:** Lane G + F.

---

### Phase 11 — Hardening pass

**RED/GREEN:**

- Race tests under load (parallel queries + reconcile).
- Corruption goldens complete.
- `git diff --check`
- Security checklist review ticket closed.

**Commit:** `test: harden race corruption and security regressions`

**Owner:** all lanes as needed.

---

## 19. README requirements (implementation content)

README must include:

1. What Stacklane is (VIP + DNS + proxy).
2. Architecture diagram (text/mermaid).
3. Compose example with `127.0.0.1::<port>` and labels.
4. Commands: serve/status/resolve.
5. Naming scheme.
6. Limitations (one endpoint/container, TCP only, no resolver installer, etc.).
7. Linux notes (`127/8` usually works).
8. macOS notes (lo0 alias may be required).
9. Future host resolver setup section clearly marked **`NOT implemented in MVP`**.
10. Security: Docker socket root-equivalent warning.
11. Link to `docs/plans/initial-mvp.md`.

---

## 20. Logging conventions

- Use stdlib `log/slog`.
- Levels: debug/info/warn/error.
- Default info.
- Include `container_id` (short), `stack_key`, `fqdn`, `vip` fields.
- Never: `env`, `Config`, auth headers, full label maps at info (debug may log validated stacklane keys only).

---

## 21. Open assumptions (resolved for implementers)

| Topic | Decision |
|-------|----------|
| State backend | Atomic JSON v1 |
| CLI parser | stdlib flags + subcommands |
| DNS library | miekg/dns |
| Admin API | HTTP over Unix socket only |
| Default DNS port | 15353 (unprivileged; avoids mDNS 5353) |
| Instance segment | Optional label; config default empty |
| Unhealthy containers | Still published in MVP |
| auto_alias VIP | Off by default |
| Go version | 1.22+ |
| Parallel endpoints per container | Not supported |

---

## 22. Implementation anti-cheating audit (for reviewers)

Reviewers must verify:

1. Proxy dial targets come from Docker inspect code path, not label strings.
2. E2E/fake tests assert **distinct** TCP payloads, not merely non-error.
3. VIP stability test actually restarts store/engine, not hard-coded map.
4. DNS tests speak real UDP/TCP DNS protocol to listen socket.
5. CI pins actions by full SHA.
6. No `SKIP` without build tag / explicit env gate.
7. No root-level `*.go`.
8. Compose examples use loopback publish form.
9. Grace expiry covered by fake clock or time injection.
10. Corrupt state tests use real load path of `serve`/store.

---

## 23. Quick reference — key contracts

### Labels (minimum enabled container)

```text
com.docker.compose.project=<any>
com.docker.compose.service=<any>
stacklane.enable=true
stacklane.project=<slug>
stacklane.endpoint=<slug>
stacklane.port=<1-65535>
# optional: stacklane.instance, stacklane.target_port, stacklane.protocol=tcp
```

### VIP policy

```text
pool: 127.77.0.0/16 (loopback-only CIDR)
alloc: durable per stack_key, ascending first-free
grace: 24h after last_active_at with zero endpoints
persist: ~/.stacklane/state.json atomic rename mode 0600
```

### DNS

```text
base: test
listen: 127.0.0.1:15353 (configurable)
A(stack) + A(endpoint) -> stack VIP
unknown in-zone: NXDOMAIN
out-of-zone: REFUSED
AAAA existing name: NOERROR empty
```

### Proxy

```text
listen: VIP:publicPort (tcp)
dial:  only 127.0.0.1:dockerHostPort
```

---

## 24. Completion rule

The initial functional MVP is complete only when:

1. All phases 0–11 commits land with green `make ci`.
2. Fake-Docker integration proves two-stack isolation (DNS + TCP).
3. Real Docker e2e proves the same when `E2E=1`.
4. README and security checklist items are satisfied.
5. Run report written later at `docs/reviews/initial-implementation-run.md` with command transcripts (not fabricated).

**This plan file alone is the plan-only deliverable; no production code accompanies the plan commit.**
