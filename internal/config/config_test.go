package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aleksclark/stacklane/internal/config"
)

func TestDefaults(t *testing.T) {
	t.Parallel()
	c := config.Defaults()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	wantState := filepath.Join(home, ".stacklane")
	if c.StateDir != wantState {
		t.Errorf("StateDir = %q, want %q", c.StateDir, wantState)
	}
	if c.VIPPool != "127.77.0.0/16" {
		t.Errorf("VIPPool = %q", c.VIPPool)
	}
	if c.VIPLeaseGrace != 24*time.Hour {
		t.Errorf("VIPLeaseGrace = %v", c.VIPLeaseGrace)
	}
	if c.DNSListen != "127.0.0.1:15353" {
		t.Errorf("DNSListen = %q", c.DNSListen)
	}
	if c.DNSBaseDomain != "test" {
		t.Errorf("DNSBaseDomain = %q", c.DNSBaseDomain)
	}
	if c.DNSDefaultInstance != "" {
		t.Errorf("DNSDefaultInstance = %q, want empty", c.DNSDefaultInstance)
	}
	if c.DNSTTL != 5 {
		t.Errorf("DNSTTL = %d", c.DNSTTL)
	}
	if c.ReconcileInterval != 15*time.Second {
		t.Errorf("ReconcileInterval = %v", c.ReconcileInterval)
	}
	if c.ReconcileDebounce != 250*time.Millisecond {
		t.Errorf("ReconcileDebounce = %v", c.ReconcileDebounce)
	}
	if c.ProxyDialTimeout != 3*time.Second {
		t.Errorf("ProxyDialTimeout = %v", c.ProxyDialTimeout)
	}
	if c.ProxyIdleTimeout != 5*time.Minute {
		t.Errorf("ProxyIdleTimeout = %v", c.ProxyIdleTimeout)
	}
	if c.ProxyShutdownTimeout != 5*time.Second {
		t.Errorf("ProxyShutdownTimeout = %v", c.ProxyShutdownTimeout)
	}
	if c.ProxyMaxConns != 1024 {
		t.Errorf("ProxyMaxConns = %d", c.ProxyMaxConns)
	}
	if c.LogLevel != "info" {
		t.Errorf("LogLevel = %q", c.LogLevel)
	}
	if c.StateResetOnCorrupt {
		t.Error("StateResetOnCorrupt want false")
	}
	if c.VIPAutoAlias {
		t.Error("VIPAutoAlias want false")
	}
	if c.DockerHost != "" {
		t.Errorf("DockerHost = %q, want empty (SDK default)", c.DockerHost)
	}
}

func TestDefaults_Validate(t *testing.T) {
	t.Parallel()
	c := config.Defaults()
	if err := c.Validate(); err != nil {
		t.Fatalf("Defaults().Validate(): %v", err)
	}
}

func TestValidate_InvalidCIDR(t *testing.T) {
	t.Parallel()
	c := config.Defaults()
	c.VIPPool = "not-a-cidr"
	if err := c.Validate(); err == nil {
		t.Fatal("want error for invalid CIDR")
	}
}

func TestValidate_NonLoopbackPool(t *testing.T) {
	t.Parallel()
	c := config.Defaults()
	c.VIPPool = "10.0.0.0/16"
	if err := c.Validate(); err == nil {
		t.Fatal("want error for non-loopback pool")
	}
}

func TestValidate_PrefixTooLarge(t *testing.T) {
	t.Parallel()
	// /15 is larger pool than /16 allowed max breadth
	c := config.Defaults()
	c.VIPPool = "127.0.0.0/15"
	if err := c.Validate(); err == nil {
		t.Fatal("want error for prefix < /16 (too large pool)")
	}
}

func TestValidate_PrefixTooSmall(t *testing.T) {
	t.Parallel()
	// /31 has fewer than 2 usable after exclusions potentially; /31 is outside /16-/30
	c := config.Defaults()
	c.VIPPool = "127.77.0.0/31"
	if err := c.Validate(); err == nil {
		t.Fatal("want error for prefix > /30")
	}
}

func TestValidate_EmptyBaseDomain(t *testing.T) {
	t.Parallel()
	c := config.Defaults()
	c.DNSBaseDomain = ""
	if err := c.Validate(); err == nil {
		t.Fatal("want error for empty base domain")
	}
}

func TestValidate_ZeroTTL(t *testing.T) {
	t.Parallel()
	c := config.Defaults()
	c.DNSTTL = 0
	if err := c.Validate(); err == nil {
		t.Fatal("want error for zero DNS TTL")
	}
}

func TestValidate_ZeroReconcileInterval(t *testing.T) {
	t.Parallel()
	c := config.Defaults()
	c.ReconcileInterval = 0
	if err := c.Validate(); err == nil {
		t.Fatal("want error for zero reconcile interval")
	}
}

func TestLoad_FlagsOverrideEnvOverrideFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "stacklane.json")
	content := `{
  "vip_pool": "127.88.0.0/16",
  "dns_base_domain": "from-file.test",
  "dns_default_instance": "fileinst",
  "dns_ttl": 9,
  "log_level": "debug"
}`
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	getenv := func(k string) string {
		switch k {
		case "STACKLANE_VIP_POOL":
			return "127.99.0.0/16"
		case "STACKLANE_DNS_BASE_DOMAIN":
			return "from-env.test"
		case "STACKLANE_LOG_LEVEL":
			return "warn"
		default:
			return ""
		}
	}

	args := []string{
		"--config", cfgPath,
		"--dns-base-domain", "from-flag.test",
		"--vip-pool", "127.77.1.0/24",
	}

	c, err := config.Load(args, getenv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// flags win over env over file
	if c.VIPPool != "127.77.1.0/24" {
		t.Errorf("VIPPool = %q, want flag value 127.77.1.0/24", c.VIPPool)
	}
	if c.DNSBaseDomain != "from-flag.test" {
		t.Errorf("DNSBaseDomain = %q, want from-flag.test", c.DNSBaseDomain)
	}
	// env wins over file where flag not set
	if c.LogLevel != "warn" {
		t.Errorf("LogLevel = %q, want warn (from env)", c.LogLevel)
	}
	// file value where neither env nor flag set
	if c.DNSDefaultInstance != "fileinst" {
		t.Errorf("DNSDefaultInstance = %q, want fileinst", c.DNSDefaultInstance)
	}
	if c.DNSTTL != 9 {
		t.Errorf("DNSTTL = %d, want 9 from file", c.DNSTTL)
	}
}

func TestLoad_EnvOnly(t *testing.T) {
	getenv := func(k string) string {
		switch k {
		case "STACKLANE_DNS_DEFAULT_INSTANCE":
			return "curri"
		case "STACKLANE_RECONCILE_INTERVAL":
			return "30s"
		case "DOCKER_HOST":
			return "unix:///tmp/docker.sock"
		default:
			return ""
		}
	}
	c, err := config.Load(nil, getenv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DNSDefaultInstance != "curri" {
		t.Errorf("DNSDefaultInstance = %q", c.DNSDefaultInstance)
	}
	if c.ReconcileInterval != 30*time.Second {
		t.Errorf("ReconcileInterval = %v", c.ReconcileInterval)
	}
	if c.DockerHost != "unix:///tmp/docker.sock" {
		t.Errorf("DockerHost = %q", c.DockerHost)
	}
}

func TestLoad_InvalidFileCIDRFails(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "stacklane.json")
	if err := os.WriteFile(cfgPath, []byte(`{"vip_pool":"10.0.0.0/8"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := config.Load([]string{"--config", cfgPath}, func(string) string { return "" })
	if err == nil {
		t.Fatal("want validation error for non-loopback pool in file")
	}
}

func TestLoad_DurationFlags(t *testing.T) {
	args := []string{
		"--vip-lease-grace", "1h",
		"--reconcile-debounce", "100ms",
		"--proxy-dial-timeout", "2s",
		"--proxy-idle-timeout", "1m",
		"--proxy-shutdown-timeout", "10s",
		"--proxy-max-conns", "64",
		"--dns-ttl", "10",
		"--dns-listen", "127.0.0.1:5354",
		"--state-dir", "/tmp/stacklane-test",
		"--state-reset-on-corrupt",
	}
	c, err := config.Load(args, func(string) string { return "" })
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.VIPLeaseGrace != time.Hour {
		t.Errorf("VIPLeaseGrace = %v", c.VIPLeaseGrace)
	}
	if c.ReconcileDebounce != 100*time.Millisecond {
		t.Errorf("ReconcileDebounce = %v", c.ReconcileDebounce)
	}
	if c.ProxyDialTimeout != 2*time.Second {
		t.Errorf("ProxyDialTimeout = %v", c.ProxyDialTimeout)
	}
	if c.ProxyIdleTimeout != time.Minute {
		t.Errorf("ProxyIdleTimeout = %v", c.ProxyIdleTimeout)
	}
	if c.ProxyShutdownTimeout != 10*time.Second {
		t.Errorf("ProxyShutdownTimeout = %v", c.ProxyShutdownTimeout)
	}
	if c.ProxyMaxConns != 64 {
		t.Errorf("ProxyMaxConns = %d", c.ProxyMaxConns)
	}
	if c.DNSTTL != 10 {
		t.Errorf("DNSTTL = %d", c.DNSTTL)
	}
	if c.DNSListen != "127.0.0.1:5354" {
		t.Errorf("DNSListen = %q", c.DNSListen)
	}
	if c.StateDir != "/tmp/stacklane-test" {
		t.Errorf("StateDir = %q", c.StateDir)
	}
	if !c.StateResetOnCorrupt {
		t.Error("StateResetOnCorrupt want true")
	}
}

func TestValidate_RejectsNonLoopbackDNSListen(t *testing.T) {
	t.Parallel()
	c := config.Defaults()
	c.DNSListen = "0.0.0.0:5353"
	if err := c.Validate(); err == nil {
		t.Fatal("want error for non-loopback dns_listen 0.0.0.0")
	}
	c.DNSListen = "192.168.1.10:5353"
	if err := c.Validate(); err == nil {
		t.Fatal("want error for non-loopback dns_listen LAN IP")
	}
	c.DNSListen = "[::]:5353"
	if err := c.Validate(); err == nil {
		t.Fatal("want error for unspecified IPv6 dns_listen")
	}
}

func TestValidate_AllowsNonLoopbackDNSListenWithExplicitFlag(t *testing.T) {
	t.Parallel()
	c := config.Defaults()
	c.DNSListen = "0.0.0.0:5353"
	c.DNSAllowNonLoopback = true
	if err := c.Validate(); err != nil {
		t.Fatalf("allow flag should permit non-loopback dns_listen: %v", err)
	}
}

func TestLoad_DNSAllowNonLoopbackFlag(t *testing.T) {
	// Without flag, 0.0.0.0 fails closed.
	_, err := config.Load([]string{"--dns-listen", "0.0.0.0:5353"}, func(string) string { return "" })
	if err == nil {
		t.Fatal("want Load error for 0.0.0.0 without allow flag")
	}
	c, err := config.Load([]string{
		"--dns-listen", "0.0.0.0:5353",
		"--dns-allow-non-loopback",
	}, func(string) string { return "" })
	if err != nil {
		t.Fatalf("Load with allow flag: %v", err)
	}
	if !c.DNSAllowNonLoopback {
		t.Fatal("DNSAllowNonLoopback want true")
	}
	if c.DNSListen != "0.0.0.0:5353" {
		t.Fatalf("DNSListen = %q", c.DNSListen)
	}
}

func TestValidate_VIPAutoAliasNotImplemented(t *testing.T) {
	t.Parallel()
	c := config.Defaults()
	c.VIPAutoAlias = true
	err := c.Validate()
	if err == nil {
		t.Fatal("want error when vip_auto_alias=true (not implemented)")
	}
}
