# Initial MVP Implementation Run Report

- **Worktree:** `/home/aleks/work/projects/stacklane/worktrees/initial-mvp`
- **Branch:** `feat/initial-mvp`
- **Final HEAD:** `d71a45`
- **Approved code tip (pre-docs-bind):** `07a7b54e3d0416e5f1914eef3f6997b58eab142b`
- **Base master:** `5c68ab515bf63b2d568fa010dd188c7174f7139c`
- **Date:** 2026-08-11
- **Module:** `github.com/aleksclark/stacklane`
- **Independent review verdict:** **APPROVED** @ `d71a45`

## Outcome

Functional MVP delivered: `stacklane serve` / `status` / `resolve` with Docker event+periodic reconcile, durable VIP leases, authoritative `stacklane.test` DNS, and TCP proxy VIP→loopback-only backends.

Security/integration review: first **CHANGES_REQUIRED** @ `fb4ff56`; Important findings remediated with TDD; final **VERDICT: APPROVED** @ `d71a45`.

## Commit list (from base master)

```
d71a45 docs: bind final review verdict in run report
07a7b54 docs: refresh run report after security remediation
b6ded0e fix: require loopback vip and dns listen defaults
dc19fbe fix: skip reconcile save when state load fails
4fe3e97 fix: persist vip leases before advertising dns and proxy
fb4ff56 docs: fix run report whitespace and tip SHA
0a3124f docs: add initial implementation run report with gate evidence
c144dd3 docs: add readme examples and real docker e2e
f00fa61 chore: add makefile and pinned github actions ci
5eb7c07 feat: add serve status and resolve commands
26e3452 feat: add reconcile engine wiring dns proxy and vip leases
9981bfb feat: add tcp proxy manager with safe loopback dial
3345357 feat: add authoritative stacklane.test dns server
90b48df feat: add crash-safe vip lease state store
25033d3 feat: add docker inspect and event watch with fake
b9cc702 feat: add deterministic loopback vip allocator
89920e7 feat: add config loading and validation
e563263 feat: add label contract and dns name construction
580ebf9 chore: bootstrap go module, interfaces, and version
4e6eca5 docs: add initial MVP implementation plan
```

## Package tree

```
cmd/stacklane/
internal/app/          # serve, status, resolve, e2e
internal/config/
internal/dns/
internal/dockerapi/ + fake/
internal/domain/
internal/labels/
internal/proxy/
internal/reconcile/
internal/state/
internal/version/
internal/vip/
testdata/compose/stack-{a,b}/
testdata/state/
```

No repository-root `*.go` files.

## Independent review

| Stage | SHA | Verdict |
|---|---|---|
| First independent review | `fb4ff56` | **CHANGES_REQUIRED** |
| Remediation | `4fe3e97`, `dc19fbe`, `b6ded0e` | Important findings fixed (TDD) |
| Post-remediation docs | `07a7b54` | Run report refreshed |
| Final bind (this commit) | `d71a45` | **APPROVED** |

### Security review remediation (Important)

Code tip for remediations: `b6ded0e5e90bfbdf53d1ef6e77f16908c801f0b9` (within `07a7b54` tip lineage).

| Finding | Fix | Regression tests |
|---|---|---|
| Persist VIP leases before DNS/proxy advertise | `apply`: Save → SetRecords → Reconcile; Save fail skips advertise | `TestApply_PersistsLeasesBeforeDNSAndProxy`, `TestApply_SaveFailureDoesNotAdvertise` |
| Runtime Store.Load failure must not wipe leases | `reconcileOnce`: Load err → skip apply/save (no empty snap Save) | `TestReconcile_LoadErrorDoesNotSaveEmptyOrClobber` |
| Proxy bind VIP must be loopback (+ pool) | `validateEndpoint`: `VIP.IsLoopback()`; optional `VIPPool` / `IsAllowedVIP`; serve wires pool | `TestProxy_RejectNonLoopbackVIP`, `TestProxy_RejectVIPOutsidePool`, `TestProxy_RejectWithIsAllowedVIPCallback` |
| DNS listen loopback fail-closed | Validate host is loopback unless `--dns-allow-non-loopback` | `TestValidate_RejectsNonLoopbackDNSListen`, `TestLoad_DNSAllowNonLoopbackFlag`, `TestValidate_AllowsNonLoopbackDNSListenWithExplicitFlag` |

Minors also landed in the same remediation pass:

- State temp write uses `O_NOFOLLOW` (Linux)
- `vip_auto_alias=true` fails validation as not implemented (no silent no-op)

## Gate evidence (real, re-run at final bind)

### `make ci` — PASS

```
go vet ./...
go test -race ./...
?   	github.com/aleksclark/stacklane/cmd/stacklane	[no test files]
ok  	github.com/aleksclark/stacklane/internal/app	(cached)
ok  	github.com/aleksclark/stacklane/internal/config	(cached)
ok  	github.com/aleksclark/stacklane/internal/dns	(cached)
ok  	github.com/aleksclark/stacklane/internal/dockerapi	(cached)
ok  	github.com/aleksclark/stacklane/internal/dockerapi/fake	(cached)
ok  	github.com/aleksclark/stacklane/internal/domain	(cached)
ok  	github.com/aleksclark/stacklane/internal/labels	(cached)
ok  	github.com/aleksclark/stacklane/internal/proxy	(cached)
ok  	github.com/aleksclark/stacklane/internal/reconcile	(cached)
ok  	github.com/aleksclark/stacklane/internal/state	(cached)
ok  	github.com/aleksclark/stacklane/internal/version	(cached)
ok  	github.com/aleksclark/stacklane/internal/vip	(cached)
go build -o bin/stacklane ./cmd/stacklane
git diff --check
test -z "$(gofmt -l cmd internal)"
# exit 0
```

### Fake-Docker integration — PASS

Covered in `internal/reconcile` and `internal/app` unit/integration tests under default `go test -race ./...`:

- Two stacks, same public port, distinct VIP/DNS/proxy payloads
- Container stop → DNS/proxy removed; lease retained within grace
- Grace expiry → VIP reallocatable
- Invalid labels / non-loopback bindings ignored
- Engine restart reloads store → stable VIP
- FQDN conflict → lexicographically smaller container ID wins
- status JSON + resolve exit codes
- Second serve daemon lock fails
- Lease Save before DNS/proxy advertise; Save fail does not advertise
- Mid-run state Load failure does not Save empty / clobber leases
- Non-loopback VIP rejected; out-of-pool VIP rejected
- Non-loopback DNS listen rejected without allow flag

### Real Docker E2E — PASS

Docker available (`docker info` OK).

```
$ make e2e
E2E=1 go test -race -tags=e2e ./internal/app -count=1
ok  	github.com/aleksclark/stacklane/internal/app	2.905s
# exit 0
```

Scenario: two compose projects (`testdata/compose/stack-a`, `stack-b`) with `hashicorp/http-echo`, labels `alpha`/`beta` + `curri` + `app:8080`, publish `127.0.0.1::8080`. Asserted distinct VIPs, DNS A records, and HTTP bodies A≠B via VIP:8080.

### `git diff --check master...HEAD` — PASS

```
$ git diff --check master...HEAD
# exit 0 (no whitespace errors)
```

## Residual limitations

- Host resolver installer intentionally **NOT implemented** (documented in README).
- `vip.auto_alias` rejected when true (not implemented); macOS may need manual lo0 alias.
- No TLS termination, UDP proxy, service mesh, or packaging in this MVP.
- No corrupt-state serve-path integration test beyond store goldens (store fail-closed covered; mid-run Load fail skip covered).
- No explicit log-redaction unit assertion for secrets (logging uses slog; inspect dumps not logged at info).

## Delivery status

- Branch `feat/initial-mvp` pushed to `origin`
- PR opened targeting `master` (unmerged)
- Main repo `master` left untouched at `5c68ab515bf63b2d568fa010dd188c7174f7139c`

## Security checklist (MVP)

- Proxies bind only loopback allocated VIPs (pool-checked at serve); dial only inspected `127.0.0.1`
- DNS listen default loopback fail-closed; explicit `--dns-allow-non-loopback` escape hatch
- Leases persisted before DNS/proxy advertise; Load failure does not wipe durable state
- State file 0600 / dir 0700; temp open `O_NOFOLLOW`; daemon flock; Unix control socket only
- Docker socket documented as root-equivalent in README
