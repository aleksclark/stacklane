# Initial MVP Implementation Run Report

**Branch:** `feat/initial-mvp`  
**Final HEAD:** `c144dd3b69f16ce78aed3099f7d4c3f24c6e7a55`  
**Base master:** `5c68ab515bf63b2d568fa010dd188c7174f7139c`  
**Date:** 2026-08-11  
**Module:** `github.com/aleksclark/stacklane`

## Outcome

Functional MVP delivered: `stacklane serve` / `status` / `resolve` with Docker event+periodic reconcile, durable VIP leases, authoritative `stacklane.test` DNS, and TCP proxy VIP→loopback-only backends.

## Commit list (from base master)

```
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

## Gate evidence (real)

### `make ci` — PASS

```
go vet ./...
go test -race ./...
# all packages ok (cmd has no test files)
go build -o bin/stacklane ./cmd/stacklane
git diff --check
test -z "$(gofmt -l cmd internal)"
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

### Real Docker E2E — PASS

Docker available (`docker info` OK).

```
$ make e2e
E2E=1 go test -race -tags=e2e ./internal/app -count=1
ok  	github.com/aleksclark/stacklane/internal/app	2.962s
```

Scenario: two compose projects (`testdata/compose/stack-a`, `stack-b`) with `hashicorp/http-echo`, labels `alpha`/`beta` + `curri` + `app:8080`, publish `127.0.0.1::8080`. Asserted distinct VIPs, DNS A records, and HTTP bodies A≠B via VIP:8080.

## Residual gaps / notes

- Host resolver installer intentionally **NOT implemented** (documented in README).
- `vip.auto_alias` remains off by default (macOS may need manual lo0 alias).
- No corrupt-state serve-path integration test beyond store goldens (store fail-closed covered).
- No explicit log-redaction unit assertion for secrets (logging uses slog; inspect dumps not logged at info).
- Worktree left clean on `feat/initial-mvp`; **not pushed**; **no PR**.

## Security checklist (MVP)

- Proxies bind only allocated VIPs; dial only inspected `127.0.0.1`
- DNS default loopback; labels cannot set destinations
- State file 0600 / dir 0700; daemon flock; Unix control socket only
- Docker socket documented as root-equivalent in README
