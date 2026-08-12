# AGENTS.md — Stacklane

Local developer daemon: per-stack loopback VIPs + authoritative `*.test` DNS + TCP proxy so parallel Docker Compose stacks can share standard ports without host-port conflicts.

Module: `github.com/aleksclark/stacklane` · Go `1.24.3` · default branch `master`

## Commands

```bash
make build      # bin/stacklane
make test       # go test ./...
make race       # go test -race ./...
make vet
make fmt        # gofmt -w cmd internal
make ci         # vet + race + build + gofmt check + git diff --check
make e2e        # E2E=1 go test -race -tags=e2e ./internal/app -count=1  (needs Docker)
make install    # ./scripts/install.sh (binary + systemd user unit + host DNS)
make uninstall  # reverse install artifacts (keeps ~/.stacklane)
```

Local install: `scripts/install.sh`

- Default (Linux): binary → `PREFIX/bin`, user unit `~/.config/systemd/user/stacklane.service`, host split-DNS via dummy `stacklane0` (`/etc/systemd/network/10-stacklane0.{netdev,network}`: `DNS=127.0.0.1:15353`, `Domains=~test`, `DNSDefaultRoute=no`) plus empty resolved drop-in `/etc/systemd/resolved.conf.d/50-stacklane.conf` (never put `Domains=~test` on Global next to public uplink DNS — NXDOMAIN poison).
- macOS: binary + `/etc/resolver/<base>`; no launchd.
- Flags: `--binary-only`, `--no-systemd`, `--no-dns`, `--no-start`, `--destdir`, `--uninstall`.
- Host DNS install refuses non-loopback `--dns-listen` hosts. Does not claim port 53.
- `DESTDIR` staging never enable/starts units or restarts networkd/resolved.

- `make ci` is the default quality gate; no Docker required.
- Real Docker E2E is **build-tagged** (`//go:build e2e`) and gated on `E2E=1`. Plain `go test ./...` skips it. gopls will not see that file unless `-tags=e2e` is set.
- Binary output: `bin/stacklane` (gitignored).
- No root-level `*.go` files — only `cmd/` and `internal/`.

## Layout

| Path | Role |
|------|------|
| `cmd/stacklane` | CLI entry: `serve`, `status`, `resolve`, `version`, `help` |
| `internal/app` | Daemon wiring (`Serve`), control-socket clients, E2E |
| `internal/config` | Defaults + JSON file + env + flags (precedence bottom→top) |
| `internal/reconcile` | Engine: events + periodic full reconcile; build desired; apply |
| `internal/dockerapi` | Thin Docker client interface + SDK adapter |
| `internal/dockerapi/fake` | In-memory client for unit/integration tests |
| `internal/domain` | `Stack`, `Endpoint`, `DesiredState`, FQDN/stack-key helpers |
| `internal/labels` | Label key constants + parse/validate |
| `internal/vip` | Loopback pool + durable lease allocator |
| `internal/state` | Atomic JSON VIP lease store (`state.json`) |
| `internal/dns` | Authoritative DNS (`miekg/dns`) |
| `internal/proxy` | TCP listeners on VIP → `127.0.0.1:hostPort` |
| `testdata/compose/` | Two-stack Compose fixtures for E2E/docs |
| `testdata/state/` | Corrupt/invalid JSON fixtures for store tests |
| `docs/plans/initial-mvp.md` | Full MVP plan (source of truth for product rules) |
| `docs/reviews/` | Implementation run reports |

## Runtime architecture

```
Docker events / list  →  reconcile.Engine  →  state.Store (leases first)
                                        ├→  dns.Controller.SetRecords
                                        └→  proxy.Manager.Reconcile
CLI status/resolve    →  Unix HTTP socket  <state-dir>/stacklane.sock
```

- `app.Serve` acquires exclusive `daemon.lock` in state dir, starts DNS + proxy + engine + control HTTP on the Unix socket.
- `status` / `resolve` are **read-only clients** of that socket (daemon must be running). They do not talk to Docker.
- Config merge order: **defaults < JSON config file < env < flags** (`config.Load`).
- State dir default `~/.stacklane` (`STACKLANE_STATE_DIR`); mode `0700`. Socket mode `0600`.

### Apply order (security-critical)

In `reconcile.apply`:

1. **`Store.Save` leases first**
2. Then DNS `SetRecords`
3. Then proxy `Reconcile`

If Save fails → **skip DNS/proxy advertise** for that cycle (do not claim VIPs that were not persisted).

### Fail-closed reconcile skips

`reconcileOnce` must **not** treat errors as empty inventory:

| Failure | Behavior |
|---------|----------|
| `Store.Load` error | Skip apply/save entirely (avoid wiping good `state.json`) |
| `Docker.ListRunning` error | Skip rebuild/apply/save; keep last-known-good DNS/proxy |
| Corrupt state at serve start | Fail closed unless `--state-reset-on-corrupt` |

Regression tests live in `internal/reconcile/*_fail_test.go` and `apply_order_test.go`. Do not “fix” flaky Docker by applying empty desired state.

## Interfaces to mock in tests

```go
dockerapi.Client   // ListRunning, Events, Close
state.Store        // Load, Save
dns.Controller     // SetRecords, ListenAddr, Shutdown
proxy.Manager      // Reconcile, Shutdown
vip.Allocator      // allocate/release against Snapshot
```

- Prefer `dockerapi/fake.Fake` (`SetContainers`, `Emit`) over real Docker.
- `app.ServeOpts{Docker, Logger, Now}` injects fakes into the full daemon path.
- Recording/tracing doubles in reconcile tests assert order and skip behavior.

## Product rules agents often miss

- **Publish form:** only `127.0.0.1` host bindings count. `0.0.0.0` / empty host IP → ignored.
- **Labels required:** `stacklane.enable`, `stacklane.project`, `stacklane.endpoint`, `stacklane.port`, plus Compose’s `com.docker.compose.project` / `service`. Optional: `instance`, `target_port`, `protocol` (TCP only).
- **One endpoint per container**; multi-replica conflicts → **smallest container ID wins**.
- **Stack key / VIP lease:** `project` or `project/instance` (`domain.MakeStackKey`). Prefer `project=org`, `instance=worktree`.
- **FQDNs:** `<endpoint>.<instance>.<project>.<base>` (instance optional); stack apex omits endpoint. `project`=org/product, `instance`=worktree. Base default `test` (never `.local`).
- **VIP pool:** IPv4 loopback only, prefix **/16–/30**, within `127.0.0.0/8`, default `127.77.0.0/16`. Allocator skips unusable `.0`/`.255` last octets.
- **Lease grace:** inactive stacks keep VIP for `vip_lease_grace` (default 24h) after last activity; DNS/proxy only while endpoints exist.
- **DNS listen:** default `127.0.0.1:15353` (avoids mDNS on 5353); loopback-only by default; non-loopback needs explicit `--dns-allow-non-loopback`.
- **Proxy:** backends must be `127.0.0.1`; bind VIP must be loopback and in pool (`IsAllowedVIP` / `VIPPool` on manager).
- **DNS records:** non-loopback A VIPs rejected by `SetRecords` (prior set unchanged).
- **`--vip-auto-alias`:** not implemented; enabling it **fails validation** (no silent no-op).
- **Host OS resolver install:** not implemented (document only).
- **Subcommand first:** `stacklane serve …`, not global flags before the verb for serve (status/resolve parse their own flags).

## Config surface (serve)

Env prefixes: `STACKLANE_*` plus `DOCKER_HOST`. Notable flags match README table: `--state-dir`, `--dns-listen`, `--dns-base-domain`, `--vip-pool`, `--config`, reconcile/proxy timeouts, `--log-level`, `--state-reset-on-corrupt`.

JSON config uses snake_case keys and **duration strings** (`"15s"`, `"24h"`). See `config.fileConfig`.

## Testing conventions

- Table-driven unit tests next to code (`*_test.go`).
- State fixtures under `testdata/state/`; Compose under `testdata/compose/stack-{a,b}/`.
- Prefer race detector for anything concurrent (DNS SetRecords vs query, proxy, engine).
- When changing fail-closed paths, extend the existing `*fail_test.go` / apply-order tests rather than only happy-path coverage.
- E2E uses unique `docker compose -p` names, temp state dir, free DNS port, and tears down compose projects in `defer`.

## Platform notes

- **Linux:** binding `127.77.x.x` usually works without lo aliases; DNS default `127.0.0.1:15353` (unprivileged; avoids mDNS on 5353).
- **macOS:** may need manual `lo0` aliases for VIP pool; auto-alias not implemented.
- **Windows:** unsupported for MVP.
- State temp writes use `O_NOFOLLOW` on Linux where applicable.

## Docs map

- Product/ops: `README.md`
- Spec/plan (detailed acceptance + non-goals): `docs/plans/initial-mvp.md`
- What shipped + security remediation history: `docs/reviews/initial-implementation-run.md`

Prefer the plan for behavioral edge cases; prefer the run report for known security fixes already landed.

## Don’ts

- Don’t add repository-root Go packages.
- Don’t apply empty desired state on Docker/list or state-load errors.
- Don’t advertise DNS/proxy before a successful lease Save.
- Don’t allow non-loopback proxy backends or non-loopback VIP binds.
- Don’t implement host resolver install / TLS / UDP / multi-endpoint-per-container in drive-by changes unless explicitly scoped.
- Don’t push to remote or edit `master` unless the user asks.
- Don’t invent PASS evidence; run `make ci` / `make e2e` for real gates.
)
