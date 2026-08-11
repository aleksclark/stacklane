package config

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config holds runtime configuration for stacklane serve and related commands.
type Config struct {
	StateDir             string
	DockerHost           string
	VIPPool              string // CIDR
	VIPLeaseGrace        time.Duration
	DNSListen            string
	DNSAllowNonLoopback  bool // escape hatch; default false fails closed on non-loopback listen
	DNSBaseDomain        string
	DNSDefaultInstance   string
	DNSTTL               uint32
	ReconcileInterval    time.Duration
	ReconcileDebounce    time.Duration
	ProxyDialTimeout     time.Duration
	ProxyIdleTimeout     time.Duration
	ProxyShutdownTimeout time.Duration
	ProxyMaxConns        int
	LogLevel             string
	StateResetOnCorrupt  bool
	VIPAutoAlias         bool

	// ConfigPath is the optional path used during Load (not typically persisted).
	ConfigPath string
}

// fileConfig is the JSON shape for optional stacklane.json.
// Duration fields are strings (e.g. "24h", "15s").
type fileConfig struct {
	StateDir             *string `json:"state_dir"`
	DockerHost           *string `json:"docker_host"`
	VIPPool              *string `json:"vip_pool"`
	VIPLeaseGrace        *string `json:"vip_lease_grace"`
	DNSListen            *string `json:"dns_listen"`
	DNSAllowNonLoopback  *bool   `json:"dns_allow_non_loopback"`
	DNSBaseDomain        *string `json:"dns_base_domain"`
	DNSDefaultInstance   *string `json:"dns_default_instance"`
	DNSTTL               *uint32 `json:"dns_ttl"`
	ReconcileInterval    *string `json:"reconcile_interval"`
	ReconcileDebounce    *string `json:"reconcile_debounce"`
	ProxyDialTimeout     *string `json:"proxy_dial_timeout"`
	ProxyIdleTimeout     *string `json:"proxy_idle_timeout"`
	ProxyShutdownTimeout *string `json:"proxy_shutdown_timeout"`
	ProxyMaxConns        *int    `json:"proxy_max_conns"`
	LogLevel             *string `json:"log_level"`
	StateResetOnCorrupt  *bool   `json:"state_reset_on_corrupt"`
	VIPAutoAlias         *bool   `json:"vip_auto_alias"`
}

// Defaults returns the plan §12 default configuration with home-expanded state dir.
func Defaults() Config {
	home, err := os.UserHomeDir()
	stateDir := "~/.stacklane"
	if err == nil && home != "" {
		stateDir = filepath.Join(home, ".stacklane")
	}
	return Config{
		StateDir:             stateDir,
		DockerHost:           "",
		VIPPool:              "127.77.0.0/16",
		VIPLeaseGrace:        24 * time.Hour,
		DNSListen:            "127.0.0.1:5353",
		DNSBaseDomain:        "stacklane.test",
		DNSDefaultInstance:   "",
		DNSTTL:               5,
		ReconcileInterval:    15 * time.Second,
		ReconcileDebounce:    250 * time.Millisecond,
		ProxyDialTimeout:     3 * time.Second,
		ProxyIdleTimeout:     5 * time.Minute,
		ProxyShutdownTimeout: 5 * time.Second,
		ProxyMaxConns:        1024,
		LogLevel:             "info",
		StateResetOnCorrupt:  false,
		VIPAutoAlias:         false,
	}
}

// Validate checks VIP pool, DNS, and timeout invariants.
func (c Config) Validate() error {
	if c.StateDir == "" {
		return fmt.Errorf("state_dir must be non-empty")
	}
	if err := validateVIPPool(c.VIPPool); err != nil {
		return err
	}
	if c.VIPLeaseGrace <= 0 {
		return fmt.Errorf("vip_lease_grace must be positive")
	}
	if c.DNSListen == "" {
		return fmt.Errorf("dns_listen must be non-empty")
	}
	udpAddr, err := net.ResolveUDPAddr("udp", c.DNSListen)
	if err != nil {
		return fmt.Errorf("dns_listen: %w", err)
	}
	if err := validateDNSListenHost(udpAddr, c.DNSAllowNonLoopback); err != nil {
		return err
	}
	if strings.TrimSpace(c.DNSBaseDomain) == "" {
		return fmt.Errorf("dns_base_domain must be non-empty")
	}
	if c.DNSDefaultInstance != "" {
		// soft check: instance slug rules applied by labels when used
		if strings.Contains(c.DNSDefaultInstance, ".") || strings.Contains(c.DNSDefaultInstance, "/") {
			return fmt.Errorf("dns_default_instance must be a single slug, got %q", c.DNSDefaultInstance)
		}
	}
	if c.DNSTTL == 0 {
		return fmt.Errorf("dns_ttl must be positive")
	}
	if c.ReconcileInterval <= 0 {
		return fmt.Errorf("reconcile_interval must be positive")
	}
	if c.ReconcileDebounce < 0 {
		return fmt.Errorf("reconcile_debounce must be non-negative")
	}
	if c.ProxyDialTimeout <= 0 {
		return fmt.Errorf("proxy_dial_timeout must be positive")
	}
	if c.ProxyIdleTimeout <= 0 {
		return fmt.Errorf("proxy_idle_timeout must be positive")
	}
	if c.ProxyShutdownTimeout <= 0 {
		return fmt.Errorf("proxy_shutdown_timeout must be positive")
	}
	if c.ProxyMaxConns <= 0 {
		return fmt.Errorf("proxy_max_conns must be positive")
	}
	if c.LogLevel == "" {
		return fmt.Errorf("log_level must be non-empty")
	}
	if c.VIPAutoAlias {
		return fmt.Errorf("vip_auto_alias is not implemented; leave false (manual lo0/alias setup if needed)")
	}
	return nil
}

// validateDNSListenHost requires loopback (or unspecified-with-allow) by default.
// Hostnames that resolve only at bind time are rejected unless they parse as loopback IPs.
func validateDNSListenHost(addr *net.UDPAddr, allowNonLoopback bool) error {
	if addr == nil {
		return fmt.Errorf("dns_listen: empty address")
	}
	ip := addr.IP
	if ip == nil {
		// ResolveUDPAddr without a host can leave IP nil for some forms; reject.
		return fmt.Errorf("dns_listen: host must be an explicit IP (got nil)")
	}
	if ip.IsUnspecified() || !ip.IsLoopback() {
		if allowNonLoopback {
			return nil
		}
		return fmt.Errorf("dns_listen must be loopback (got %s); set --dns-allow-non-loopback to override", addr.String())
	}
	return nil
}

func validateVIPPool(cidr string) error {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return fmt.Errorf("vip_pool: invalid CIDR %q: %w", cidr, err)
	}
	if !prefix.Addr().Is4() {
		return fmt.Errorf("vip_pool: must be IPv4 CIDR, got %q", cidr)
	}
	// Normalize to network prefix for iteration.
	prefix = prefix.Masked()
	bits := prefix.Bits()
	if bits < 16 || bits > 30 {
		return fmt.Errorf("vip_pool: prefix length must be between /16 and /30 inclusive, got /%d", bits)
	}

	// Every address in pool must be loopback (contained in 127.0.0.0/8).
	loopback, _ := netip.ParsePrefix("127.0.0.0/8")
	if !loopback.Contains(prefix.Addr()) {
		return fmt.Errorf("vip_pool: must be within 127.0.0.0/8 loopback, got %q", cidr)
	}
	// Last address of the prefix must also be loopback.
	last := lastAddr(prefix)
	if !last.IsLoopback() || !loopback.Contains(last) {
		return fmt.Errorf("vip_pool: range must be entirely loopback, got %q", cidr)
	}

	usable := countUsable(prefix)
	if usable < 2 {
		return fmt.Errorf("vip_pool: need at least 2 usable addresses, got %d in %s", usable, cidr)
	}
	return nil
}

func lastAddr(prefix netip.Prefix) netip.Addr {
	addr := prefix.Addr().As4()
	hostBits := 32 - prefix.Bits()
	// Set all host bits.
	var hostMask uint32
	if hostBits == 32 {
		hostMask = ^uint32(0)
	} else {
		hostMask = (uint32(1) << hostBits) - 1
	}
	ip := uint32(addr[0])<<24 | uint32(addr[1])<<16 | uint32(addr[2])<<8 | uint32(addr[3])
	ip |= hostMask
	return netip.AddrFrom4([4]byte{byte(ip >> 24), byte(ip >> 16), byte(ip >> 8), byte(ip)})
}

// countUsable approximates usable VIP count: host addresses minus network,
// and skipping .0/.255 in each /24-style last octet when applicable.
func countUsable(prefix netip.Prefix) int {
	hostBits := 32 - prefix.Bits()
	if hostBits <= 0 {
		return 0
	}
	total := 1 << hostBits
	// Iterate for small pools; for large (/16) compute analytically.
	if hostBits > 16 {
		// Should not happen due to /16 min, but be safe.
		return total
	}
	count := 0
	base := prefix.Addr().As4()
	baseU := uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])
	for i := 0; i < total; i++ {
		ip := baseU + uint32(i)
		b0 := byte(ip >> 24)
		b1 := byte(ip >> 16)
		b2 := byte(ip >> 8)
		b3 := byte(ip)
		a := netip.AddrFrom4([4]byte{b0, b1, b2, b3})
		if !prefix.Contains(a) {
			continue
		}
		if !a.IsLoopback() {
			continue
		}
		// Skip network address (first) and typical .0/.255 last-octet exclusions.
		if i == 0 {
			continue // network
		}
		if b3 == 0 || b3 == 255 {
			continue
		}
		// Also skip broadcast (last) if distinct.
		if i == total-1 && hostBits <= 30 {
			// broadcast often ends with non-0/255 for odd sizes; still skip as unusable
			// but .0/.255 already covered common cases. For /30 etc. last may be usable-ish;
			// plan: skip network; exclude .0 and .255 last octet.
			// Keep broadcast if last octet not 0/255 — still count it? Plan says skip network
			// and .0/.255. Count broadcast if last octet ok.
		}
		count++
	}
	return count
}

// Load merges defaults < JSON config file < env < flags.
// getenv should typically be os.Getenv; pass a stub in tests.
// args are flag-style arguments (without program name), e.g. os.Args[1:].
func Load(args []string, getenv func(string) string) (Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	cfg := Defaults()

	// First pass: discover --config / -config path without consuming other flags.
	configPath := getenv("STACKLANE_CONFIG")
	fsDiscover := flag.NewFlagSet("stacklane-config-discover", flag.ContinueOnError)
	fsDiscover.SetOutput(discard{})
	cfgFlag := fsDiscover.String("config", configPath, "optional config file path")
	_ = fsDiscover.Parse(filterKnown(args, map[string]bool{"config": true}))
	if *cfgFlag != "" {
		configPath = *cfgFlag
	}
	cfg.ConfigPath = configPath

	if configPath != "" {
		if err := applyFile(&cfg, configPath); err != nil {
			return Config{}, err
		}
	}

	applyEnv(&cfg, getenv)

	if err := applyFlags(&cfg, args); err != nil {
		return Config{}, err
	}

	// Expand ~/ in state dir if still present.
	if strings.HasPrefix(cfg.StateDir, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			cfg.StateDir = filepath.Join(home, cfg.StateDir[2:])
		}
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func applyFile(cfg *Config, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("config file: %w", err)
	}
	var fc fileConfig
	if err := json.Unmarshal(data, &fc); err != nil {
		return fmt.Errorf("config file: %w", err)
	}
	if fc.StateDir != nil {
		cfg.StateDir = *fc.StateDir
	}
	if fc.DockerHost != nil {
		cfg.DockerHost = *fc.DockerHost
	}
	if fc.VIPPool != nil {
		cfg.VIPPool = *fc.VIPPool
	}
	if fc.VIPLeaseGrace != nil {
		d, err := time.ParseDuration(*fc.VIPLeaseGrace)
		if err != nil {
			return fmt.Errorf("config vip_lease_grace: %w", err)
		}
		cfg.VIPLeaseGrace = d
	}
	if fc.DNSListen != nil {
		cfg.DNSListen = *fc.DNSListen
	}
	if fc.DNSAllowNonLoopback != nil {
		cfg.DNSAllowNonLoopback = *fc.DNSAllowNonLoopback
	}
	if fc.DNSBaseDomain != nil {
		cfg.DNSBaseDomain = *fc.DNSBaseDomain
	}
	if fc.DNSDefaultInstance != nil {
		cfg.DNSDefaultInstance = *fc.DNSDefaultInstance
	}
	if fc.DNSTTL != nil {
		cfg.DNSTTL = *fc.DNSTTL
	}
	if fc.ReconcileInterval != nil {
		d, err := time.ParseDuration(*fc.ReconcileInterval)
		if err != nil {
			return fmt.Errorf("config reconcile_interval: %w", err)
		}
		cfg.ReconcileInterval = d
	}
	if fc.ReconcileDebounce != nil {
		d, err := time.ParseDuration(*fc.ReconcileDebounce)
		if err != nil {
			return fmt.Errorf("config reconcile_debounce: %w", err)
		}
		cfg.ReconcileDebounce = d
	}
	if fc.ProxyDialTimeout != nil {
		d, err := time.ParseDuration(*fc.ProxyDialTimeout)
		if err != nil {
			return fmt.Errorf("config proxy_dial_timeout: %w", err)
		}
		cfg.ProxyDialTimeout = d
	}
	if fc.ProxyIdleTimeout != nil {
		d, err := time.ParseDuration(*fc.ProxyIdleTimeout)
		if err != nil {
			return fmt.Errorf("config proxy_idle_timeout: %w", err)
		}
		cfg.ProxyIdleTimeout = d
	}
	if fc.ProxyShutdownTimeout != nil {
		d, err := time.ParseDuration(*fc.ProxyShutdownTimeout)
		if err != nil {
			return fmt.Errorf("config proxy_shutdown_timeout: %w", err)
		}
		cfg.ProxyShutdownTimeout = d
	}
	if fc.ProxyMaxConns != nil {
		cfg.ProxyMaxConns = *fc.ProxyMaxConns
	}
	if fc.LogLevel != nil {
		cfg.LogLevel = *fc.LogLevel
	}
	if fc.StateResetOnCorrupt != nil {
		cfg.StateResetOnCorrupt = *fc.StateResetOnCorrupt
	}
	if fc.VIPAutoAlias != nil {
		cfg.VIPAutoAlias = *fc.VIPAutoAlias
	}
	return nil
}

func applyEnv(cfg *Config, getenv func(string) string) {
	if v := getenv("STACKLANE_STATE_DIR"); v != "" {
		cfg.StateDir = v
	}
	if v := getenv("DOCKER_HOST"); v != "" {
		cfg.DockerHost = v
	}
	if v := getenv("STACKLANE_VIP_POOL"); v != "" {
		cfg.VIPPool = v
	}
	if v := getenv("STACKLANE_VIP_LEASE_GRACE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.VIPLeaseGrace = d
		}
	}
	if v := getenv("STACKLANE_DNS_LISTEN"); v != "" {
		cfg.DNSListen = v
	}
	if v := getenv("STACKLANE_DNS_ALLOW_NON_LOOPBACK"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			cfg.DNSAllowNonLoopback = true
		case "0", "false", "no", "off":
			cfg.DNSAllowNonLoopback = false
		}
	}
	if v := getenv("STACKLANE_DNS_BASE_DOMAIN"); v != "" {
		cfg.DNSBaseDomain = v
	}
	if v := getenv("STACKLANE_DNS_DEFAULT_INSTANCE"); v != "" {
		cfg.DNSDefaultInstance = v
	}
	if v := getenv("STACKLANE_DNS_TTL"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 32); err == nil {
			cfg.DNSTTL = uint32(n)
		}
	}
	if v := getenv("STACKLANE_RECONCILE_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.ReconcileInterval = d
		}
	}
	if v := getenv("STACKLANE_RECONCILE_DEBOUNCE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.ReconcileDebounce = d
		}
	}
	if v := getenv("STACKLANE_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
}

func applyFlags(cfg *Config, args []string) error {
	fs := flag.NewFlagSet("stacklane", flag.ContinueOnError)
	fs.SetOutput(discard{})

	stateDir := fs.String("state-dir", cfg.StateDir, "state directory")
	configPath := fs.String("config", cfg.ConfigPath, "optional config file")
	dockerHost := fs.String("docker-host", cfg.DockerHost, "docker host")
	vipPool := fs.String("vip-pool", cfg.VIPPool, "VIP pool CIDR")
	vipLeaseGrace := fs.Duration("vip-lease-grace", cfg.VIPLeaseGrace, "VIP lease grace")
	dnsListen := fs.String("dns-listen", cfg.DNSListen, "DNS listen address")
	dnsAllowNonLoopback := fs.Bool("dns-allow-non-loopback", cfg.DNSAllowNonLoopback, "allow non-loopback DNS listen (fail-open escape hatch)")
	dnsBaseDomain := fs.String("dns-base-domain", cfg.DNSBaseDomain, "DNS base domain")
	dnsDefaultInstance := fs.String("dns-default-instance", cfg.DNSDefaultInstance, "default instance slug")
	dnsTTL := fs.Uint("dns-ttl", uint(cfg.DNSTTL), "DNS TTL seconds")
	reconcileInterval := fs.Duration("reconcile-interval", cfg.ReconcileInterval, "reconcile interval")
	reconcileDebounce := fs.Duration("reconcile-debounce", cfg.ReconcileDebounce, "reconcile debounce")
	proxyDialTimeout := fs.Duration("proxy-dial-timeout", cfg.ProxyDialTimeout, "proxy dial timeout")
	proxyIdleTimeout := fs.Duration("proxy-idle-timeout", cfg.ProxyIdleTimeout, "proxy idle timeout")
	proxyShutdownTimeout := fs.Duration("proxy-shutdown-timeout", cfg.ProxyShutdownTimeout, "proxy shutdown timeout")
	proxyMaxConns := fs.Int("proxy-max-conns", cfg.ProxyMaxConns, "proxy max connections")
	logLevel := fs.String("log-level", cfg.LogLevel, "log level")
	stateReset := fs.Bool("state-reset-on-corrupt", cfg.StateResetOnCorrupt, "reset state on corrupt")
	vipAutoAlias := fs.Bool("vip-auto-alias", cfg.VIPAutoAlias, "auto-add VIP aliases (not implemented)")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("flags: %w", err)
	}

	// Detect which flags were explicitly set so we don't "override" with defaults
	// when the flag wasn't passed. flag.FlagSet always has values after Parse;
	// we compare Visit.
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) {
		set[f.Name] = true
	})

	if set["state-dir"] {
		cfg.StateDir = *stateDir
	}
	if set["config"] {
		cfg.ConfigPath = *configPath
	}
	if set["docker-host"] {
		cfg.DockerHost = *dockerHost
	}
	if set["vip-pool"] {
		cfg.VIPPool = *vipPool
	}
	if set["vip-lease-grace"] {
		cfg.VIPLeaseGrace = *vipLeaseGrace
	}
	if set["dns-listen"] {
		cfg.DNSListen = *dnsListen
	}
	if set["dns-allow-non-loopback"] {
		cfg.DNSAllowNonLoopback = *dnsAllowNonLoopback
	}
	if set["dns-base-domain"] {
		cfg.DNSBaseDomain = *dnsBaseDomain
	}
	if set["dns-default-instance"] {
		cfg.DNSDefaultInstance = *dnsDefaultInstance
	}
	if set["dns-ttl"] {
		cfg.DNSTTL = uint32(*dnsTTL)
	}
	if set["reconcile-interval"] {
		cfg.ReconcileInterval = *reconcileInterval
	}
	if set["reconcile-debounce"] {
		cfg.ReconcileDebounce = *reconcileDebounce
	}
	if set["proxy-dial-timeout"] {
		cfg.ProxyDialTimeout = *proxyDialTimeout
	}
	if set["proxy-idle-timeout"] {
		cfg.ProxyIdleTimeout = *proxyIdleTimeout
	}
	if set["proxy-shutdown-timeout"] {
		cfg.ProxyShutdownTimeout = *proxyShutdownTimeout
	}
	if set["proxy-max-conns"] {
		cfg.ProxyMaxConns = *proxyMaxConns
	}
	if set["log-level"] {
		cfg.LogLevel = *logLevel
	}
	if set["state-reset-on-corrupt"] {
		cfg.StateResetOnCorrupt = *stateReset
	}
	if set["vip-auto-alias"] {
		cfg.VIPAutoAlias = *vipAutoAlias
	}
	return nil
}

// filterKnown keeps only flags whose names are in allow (for config path discovery).
func filterKnown(args []string, allow map[string]bool) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			continue
		}
		name := strings.TrimLeft(a, "-")
		val := ""
		if idx := strings.IndexByte(name, '='); idx >= 0 {
			val = name[idx+1:]
			name = name[:idx]
		}
		if !allow[name] {
			// skip this flag and its separate value if present
			if val == "" && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				// might be value for unknown flag — skip both only if it looks like a value
				// For discover pass we only care about --config; leave others out entirely.
				// If unknown flag takes a value, skip next token when it doesn't start with -
				i++
			}
			continue
		}
		out = append(out, a)
		if val == "" && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			i++
			out = append(out, args[i])
		}
	}
	return out
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
