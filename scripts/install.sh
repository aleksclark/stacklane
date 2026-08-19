#!/usr/bin/env bash
# Install stacklane from this source tree onto the local machine.
#
# Always builds/installs the binary. Optionally installs:
#   - systemd --user unit (Linux) to run `stacklane serve`
#   - host split-DNS so *.stacklane.test resolves via 127.0.0.1:5353
#     (systemd-resolved drop-in on Linux; /etc/resolver on macOS)
#
# Does NOT claim port 53, edit global resolv.conf, or install launchd (macOS
# service). VIP lo0 aliases on macOS remain a manual operator step.
#
# Usage:
#   ./scripts/install.sh                 # binary + systemd + dns (where supported)
#   ./scripts/install.sh --binary-only   # binary only
#   ./scripts/install.sh --uninstall     # reverse install (keeps ~/.stacklane)
#   PREFIX=/usr/local ./scripts/install.sh
set -euo pipefail

PREFIX="${PREFIX:-${HOME}/.local}"
DESTDIR="${DESTDIR:-}"
STATE_DIR="${STACKLANE_STATE_DIR:-${HOME}/.stacklane}"
DNS_LISTEN="${STACKLANE_DNS_LISTEN:-127.0.0.1:5353}"
DNS_BASE_DOMAIN="${STACKLANE_DNS_BASE_DOMAIN:-stacklane.test}"
UNIT_NAME="stacklane.service"

UNINSTALL=0
SKIP_BUILD=0
WITH_SYSTEMD=1
WITH_DNS=1
START_SERVICE=1
BINARY_ONLY=0

OS="$(uname -s)"

usage() {
  cat <<'EOF'
Usage: install.sh [options]

  Install stacklane from this repository: binary, optional systemd user unit,
  and optional host split-DNS for *.stacklane.test.

Options:
  --prefix DIR       Install prefix (default: ~/.local, or $PREFIX)
  --destdir DIR      Staging root prepended to install paths (no enable/start)
  --state-dir DIR    Daemon state dir (default: ~/.stacklane)
  --dns-listen ADDR  DNS listen addr written into unit/DNS config
                     (default: 127.0.0.1:5353)
  --dns-domain NAME  Base domain (default: stacklane.test)
  --binary-only      Install binary only (no systemd, no host DNS)
  --no-systemd       Skip systemd user unit
  --no-dns           Skip host DNS/resolver configuration
  --no-start         Install unit but do not enable/start it
  --systemd          Force systemd unit install (default on Linux)
  --dns              Force host DNS install (default on Linux/macOS)
  --uninstall        Remove binary, unit, and host DNS artifacts
  --skip-build       Use existing bin/stacklane instead of rebuilding
  -h, --help         Show this help

Environment:
  PREFIX, DESTDIR, GO
  STACKLANE_STATE_DIR, STACKLANE_DNS_LISTEN, STACKLANE_DNS_BASE_DOMAIN

Examples:
  ./scripts/install.sh
  ./scripts/install.sh --binary-only
  ./scripts/install.sh --uninstall
  PREFIX=/usr/local ./scripts/install.sh --no-start
EOF
}

log()  { printf '%s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

# If PREFIX already ends with /bin, treat it as the bindir itself.
bindir_from_prefix() {
  local p="$1"
  case "$p" in
    */bin|*/bin/) printf '%s\n' "${p%/}" ;;
    *)            printf '%s\n' "${p%/}/bin" ;;
  esac
}

expand_home() {
  local p="$1"
  case "$p" in
    "~/"*) printf '%s\n' "${HOME}/${p#~/}" ;;
    "~")   printf '%s\n' "${HOME}" ;;
    *)     printf '%s\n' "$p" ;;
  esac
}

# Run a command as root when needed. Uses sudo if not already root.
# In DESTDIR mode, never escalates (files go under DESTDIR).
run_root() {
  if [[ -n "$DESTDIR" ]]; then
    "$@"
    return
  fi
  if [[ "$(id -u)" -eq 0 ]]; then
    "$@"
    return
  fi
  if command -v sudo >/dev/null 2>&1; then
    sudo "$@"
    return
  fi
  die "root privileges required for: $*"
}

write_file() {
  # write_file MODE PATH heredoc-via-stdin
  local mode="$1" path="$2"
  local dir
  dir="$(dirname "$path")"
  if [[ -n "$DESTDIR" ]] || [[ -w "$dir" ]] 2>/dev/null || [[ -w "$(dirname "$dir")" ]] 2>/dev/null; then
    mkdir -p "$dir"
    local tmp="${path}.tmp.$$"
    cat >"$tmp"
    chmod "$mode" "$tmp"
    mv -f "$tmp" "$path"
    return
  fi
  # Need root to create parent / write path
  run_root mkdir -p "$dir"
  local tmp
  tmp="$(mktemp)"
  cat >"$tmp"
  chmod "$mode" "$tmp"
  run_root cp "$tmp" "$path"
  run_root chmod "$mode" "$path"
  rm -f "$tmp"
}

remove_file() {
  local path="$1"
  if [[ -e "$path" || -L "$path" ]]; then
    if [[ -n "$DESTDIR" ]] || [[ -w "$(dirname "$path")" ]]; then
      rm -f "$path"
    else
      run_root rm -f "$path"
    fi
    log "removed $path"
  fi
}

parse_dns_listen() {
  # Sets DNS_HOST DNS_PORT from DNS_LISTEN (host:port)
  local addr="$1"
  if [[ "$addr" != *:* ]]; then
    die "dns-listen must be host:port, got: $addr"
  fi
  DNS_HOST="${addr%:*}"
  DNS_PORT="${addr##*:}"
  [[ -n "$DNS_HOST" && -n "$DNS_PORT" ]] || die "invalid dns-listen: $addr"
  case "$DNS_PORT" in
    *[!0-9]*) die "dns-listen port must be numeric: $addr" ;;
  esac
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --prefix)
      PREFIX="${2:?--prefix requires a directory}"; shift 2 ;;
    --destdir)
      DESTDIR="${2:?--destdir requires a directory}"; shift 2 ;;
    --state-dir)
      STATE_DIR="${2:?--state-dir requires a directory}"; shift 2 ;;
    --dns-listen)
      DNS_LISTEN="${2:?--dns-listen requires host:port}"; shift 2 ;;
    --dns-domain)
      DNS_BASE_DOMAIN="${2:?--dns-domain requires a name}"; shift 2 ;;
    --binary-only)
      BINARY_ONLY=1; WITH_SYSTEMD=0; WITH_DNS=0; START_SERVICE=0; shift ;;
    --no-systemd)
      WITH_SYSTEMD=0; shift ;;
    --no-dns)
      WITH_DNS=0; shift ;;
    --no-start)
      START_SERVICE=0; shift ;;
    --systemd)
      WITH_SYSTEMD=1; shift ;;
    --dns)
      WITH_DNS=1; shift ;;
    --uninstall)
      UNINSTALL=1; shift ;;
    --skip-build)
      SKIP_BUILD=1; shift ;;
    -h|--help)
      usage; exit 0 ;;
    *)
      die "unknown option: $1 (try --help)" ;;
  esac
done

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PREFIX="$(expand_home "$PREFIX")"
STATE_DIR="$(expand_home "$STATE_DIR")"
BINDIR="$(bindir_from_prefix "$PREFIX")"
INSTALL_BIN="${DESTDIR}${BINDIR}/stacklane"
# Runtime path (no DESTDIR) used inside unit files and user-facing messages
RUNTIME_BIN="${BINDIR}/stacklane"
GO_BIN="${GO:-go}"

USER_UNIT_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
UNIT_PATH="${DESTDIR}${USER_UNIT_DIR}/${UNIT_NAME}"

# Host DNS artifact paths (DESTDIR-prefixed when staging)
RESOLVED_DROPIN_REL="/etc/systemd/resolved.conf.d/50-stacklane.conf"
MAC_RESOLVER_REL="/etc/resolver/${DNS_BASE_DOMAIN}"
RESOLVED_DROPIN="${DESTDIR}${RESOLVED_DROPIN_REL}"
MAC_RESOLVER="${DESTDIR}${MAC_RESOLVER_REL}"

case "$OS" in
  Linux) ;;
  Darwin) ;;
  MINGW*|MSYS*|CYGWIN*|Windows_NT)
    die "Windows is not supported for MVP" ;;
  *)
    warn "unsupported OS '$OS'; binary install only"
    WITH_SYSTEMD=0
    WITH_DNS=0
    START_SERVICE=0
    ;;
esac

if [[ "$BINARY_ONLY" -eq 1 ]]; then
  WITH_SYSTEMD=0
  WITH_DNS=0
  START_SERVICE=0
fi

# systemd user units: Linux only
if [[ "$OS" != "Linux" ]]; then
  WITH_SYSTEMD=0
fi

# Staging never enables/starts services
if [[ -n "$DESTDIR" ]]; then
  START_SERVICE=0
fi

parse_dns_listen "$DNS_LISTEN"

# --- uninstall ---------------------------------------------------------------

systemd_user() {
  # systemctl --user wrapper; no-op if unavailable
  if ! command -v systemctl >/dev/null 2>&1; then
    return 1
  fi
  systemctl --user "$@"
}

uninstall_systemd() {
  if [[ "$OS" != "Linux" ]]; then
    return 0
  fi
  if [[ -z "$DESTDIR" ]] && command -v systemctl >/dev/null 2>&1; then
    systemd_user disable --now "$UNIT_NAME" 2>/dev/null || true
    systemd_user daemon-reload 2>/dev/null || true
  fi
  remove_file "$UNIT_PATH"
}

uninstall_dns() {
  case "$OS" in
    Linux)
      if [[ -e "$RESOLVED_DROPIN" || -L "$RESOLVED_DROPIN" ]]; then
        remove_file "$RESOLVED_DROPIN"
        if [[ -z "$DESTDIR" ]] && command -v systemctl >/dev/null 2>&1; then
          run_root systemctl restart systemd-resolved 2>/dev/null || \
            warn "could not restart systemd-resolved; reboot or restart it manually"
        fi
      fi
      ;;
    Darwin)
      remove_file "$MAC_RESOLVER"
      # remove empty /etc/resolver if we emptied it (best effort)
      if [[ -z "$DESTDIR" && -d /etc/resolver ]]; then
        if [[ -z "$(ls -A /etc/resolver 2>/dev/null || true)" ]]; then
          run_root rmdir /etc/resolver 2>/dev/null || true
        fi
      fi
      ;;
  esac
}

uninstall() {
  # Honor the same selectors as install: default removes everything this
  # script manages; --binary-only / --no-systemd / --no-dns narrow scope.
  if [[ "$WITH_SYSTEMD" -eq 1 ]]; then
    uninstall_systemd
  fi
  if [[ "$WITH_DNS" -eq 1 ]]; then
    uninstall_dns
  fi
  if [[ -e "$INSTALL_BIN" || -L "$INSTALL_BIN" ]]; then
    rm -f "$INSTALL_BIN"
    log "removed $INSTALL_BIN"
  else
    log "nothing to remove at $INSTALL_BIN"
  fi
  log "note: state dir ($STATE_DIR) was left untouched"
}

if [[ "$UNINSTALL" -eq 1 ]]; then
  uninstall
  exit 0
fi

# --- build + binary ----------------------------------------------------------

if [[ "$SKIP_BUILD" -eq 0 ]]; then
  command -v "$GO_BIN" >/dev/null 2>&1 || die "Go toolchain not found (looked for '$GO_BIN'). Install Go 1.26.6+ or set GO=."
  log "building stacklane..."
  (
    cd "$ROOT"
    mkdir -p bin
    "$GO_BIN" build -o bin/stacklane ./cmd/stacklane
  )
else
  [[ -f "$ROOT/bin/stacklane" ]] || die "--skip-build set but $ROOT/bin/stacklane missing; run make build first"
fi

SRC="$ROOT/bin/stacklane"
[[ -f "$SRC" ]] || die "build did not produce $SRC"

mkdir -p "${DESTDIR}${BINDIR}"
tmp="${INSTALL_BIN}.tmp.$$"
cp "$SRC" "$tmp"
chmod 755 "$tmp"
mv -f "$tmp" "$INSTALL_BIN"

ver="$("$INSTALL_BIN" version 2>/dev/null || true)"
log "installed $INSTALL_BIN${ver:+ ($ver)}"

case ":${PATH}:" in
  *":${BINDIR}:"*) ;;
  *)
    log "note: ${BINDIR} is not on PATH; add it, e.g.:"
    log "  export PATH=\"${BINDIR}:\$PATH\""
    ;;
esac

# --- systemd user unit (Linux) -----------------------------------------------

install_systemd() {
  if [[ "$WITH_SYSTEMD" -ne 1 ]]; then
    return 0
  fi
  if [[ -z "$DESTDIR" ]] && ! command -v systemctl >/dev/null 2>&1; then
    warn "systemctl not found; skipping systemd unit"
    return 0
  fi

  mkdir -p "${DESTDIR}${USER_UNIT_DIR}"
  # Use absolute runtime binary path. %h is home in systemd.
  # Pass explicit state-dir and dns-listen so unit matches install choices.
  write_file 644 "$UNIT_PATH" <<EOF
[Unit]
Description=Stacklane local VIP DNS and TCP proxy
Documentation=https://github.com/aleksclark/stacklane
After=network-online.target
Wants=network-online.target
# Docker may be a system unit; user units cannot robustly After= it.
# stacklane retries via periodic reconcile if Docker is briefly unavailable.

[Service]
Type=simple
ExecStart=${RUNTIME_BIN} serve --state-dir ${STATE_DIR} --dns-listen ${DNS_LISTEN} --dns-base-domain ${DNS_BASE_DOMAIN}
Restart=on-failure
RestartSec=2
# Hardening (developer daemon still needs Docker socket + loopback binds)
NoNewPrivileges=true
PrivateTmp=true

[Install]
WantedBy=default.target
EOF
  log "installed systemd user unit $UNIT_PATH"

  if [[ "$START_SERVICE" -eq 1 ]]; then
    systemd_user daemon-reload || die "systemctl --user daemon-reload failed"
    # Linger so the user service can run without an interactive login (best effort)
    if command -v loginctl >/dev/null 2>&1; then
      if ! loginctl show-user "$USER" -p Linger 2>/dev/null | grep -q 'Linger=yes'; then
        if [[ "$(id -u)" -eq 0 ]]; then
          loginctl enable-linger "$USER" 2>/dev/null || warn "could not enable linger for $USER"
        elif command -v sudo >/dev/null 2>&1; then
          sudo loginctl enable-linger "$USER" 2>/dev/null || \
            warn "enable lingering for background serve without login: sudo loginctl enable-linger $USER"
        else
          warn "enable lingering for background serve without login: loginctl enable-linger $USER"
        fi
      fi
    fi
    systemd_user enable --now "$UNIT_NAME" || die "failed to enable/start $UNIT_NAME"
    log "enabled and started user unit $UNIT_NAME"
  else
    log "unit installed but not started (use: systemctl --user enable --now $UNIT_NAME)"
  fi
}

# --- host DNS ----------------------------------------------------------------

install_dns_linux_resolved() {
  write_file 644 "$RESOLVED_DROPIN" <<EOF
# Managed by stacklane scripts/install.sh — split DNS for ${DNS_BASE_DOMAIN}
# Routes only ~${DNS_BASE_DOMAIN} to the stacklane daemon (default ${DNS_LISTEN}).
# Remove this file and restart systemd-resolved to undo.
[Resolve]
DNS=${DNS_HOST}:${DNS_PORT}
Domains=~${DNS_BASE_DOMAIN}
EOF
  log "installed systemd-resolved drop-in $RESOLVED_DROPIN"

  if [[ -z "$DESTDIR" ]]; then
    if command -v systemctl >/dev/null 2>&1; then
      if run_root systemctl restart systemd-resolved 2>/dev/null; then
        log "restarted systemd-resolved"
      else
        warn "could not restart systemd-resolved; run: sudo systemctl restart systemd-resolved"
      fi
    fi
    # Helpful verification hint
    if command -v resolvectl >/dev/null 2>&1; then
      log "verify with: resolvectl query app.example.${DNS_BASE_DOMAIN}   # after stacklane serve is up"
    fi
  fi
}

install_dns_macos() {
  write_file 644 "$MAC_RESOLVER" <<EOF
# Managed by stacklane scripts/install.sh
nameserver ${DNS_HOST}
port ${DNS_PORT}
EOF
  log "installed macOS resolver $MAC_RESOLVER"
  log "verify with: dscacheutil -q host -a name app.example.${DNS_BASE_DOMAIN}"
}

install_dns() {
  if [[ "$WITH_DNS" -ne 1 ]]; then
    return 0
  fi

  # Refuse non-loopback DNS targets unless DESTDIR staging (operator knows best)
  case "$DNS_HOST" in
    127.*|::1) ;;
    *)
      die "refusing host DNS config for non-loopback dns-listen host '$DNS_HOST' (fail-closed)"
      ;;
  esac

  case "$OS" in
    Linux)
      # Prefer systemd-resolved when present or when staging under DESTDIR
      if [[ -n "$DESTDIR" ]] || \
         [[ -d /etc/systemd/resolved.conf.d ]] || \
         [[ -L /etc/resolv.conf && "$(readlink -f /etc/resolv.conf 2>/dev/null || true)" == *systemd* ]] || \
         command -v resolvectl >/dev/null 2>&1; then
        install_dns_linux_resolved
      else
        warn "systemd-resolved not detected; host DNS not configured"
        warn "point tools at ${DNS_LISTEN} or use: stacklane resolve <name>"
        warn "manual resolved drop-in path: ${RESOLVED_DROPIN_REL}"
      fi
      ;;
    Darwin)
      install_dns_macos
      warn "macOS VIP binds may need lo0 aliases, e.g.: sudo ifconfig lo0 alias 127.77.0.1 netmask 255.255.0.0"
      ;;
  esac
}

install_systemd
install_dns

# --- summary -----------------------------------------------------------------

log
log "done."
if [[ "$WITH_SYSTEMD" -eq 1 && "$START_SERVICE" -eq 1 && -z "$DESTDIR" ]]; then
  log "  stacklane serve is managed by: systemctl --user status ${UNIT_NAME}"
  log "  logs: journalctl --user -u ${UNIT_NAME} -f"
else
  log "  start daemon: ${RUNTIME_BIN} serve"
fi
log "  status:       stacklane status -o json"
log "  resolve:      stacklane resolve app.example.${DNS_BASE_DOMAIN}"
if [[ "$WITH_DNS" -eq 1 ]]; then
  log "  host DNS:     *.${DNS_BASE_DOMAIN} -> ${DNS_LISTEN} (daemon must be running)"
else
  log "  host DNS:     not configured; dig @${DNS_HOST} -p ${DNS_PORT} <name>"
fi
log
log "uninstall: $0 --uninstall"
