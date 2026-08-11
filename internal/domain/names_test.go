package domain_test

import (
	"strings"
	"testing"

	"github.com/aleksclark/stacklane/internal/domain"
)

func TestMakeStackKey_WithInstance(t *testing.T) {
	t.Parallel()
	got := domain.MakeStackKey("feature-a", "curri")
	if got != domain.StackKey("feature-a/curri") {
		t.Fatalf("MakeStackKey = %q, want %q", got, "feature-a/curri")
	}
}

func TestMakeStackKey_WithoutInstance(t *testing.T) {
	t.Parallel()
	got := domain.MakeStackKey("feature-a", "")
	if got != domain.StackKey("feature-a") {
		t.Fatalf("MakeStackKey = %q, want %q", got, "feature-a")
	}
}

func TestBuildFQDNs_WithInstance(t *testing.T) {
	t.Parallel()
	endpointFQDN, stackFQDN := domain.BuildFQDNs("postgres", "feature-a", "curri", "stacklane.test")
	if endpointFQDN != "postgres.feature-a.curri.stacklane.test" {
		t.Fatalf("endpointFQDN = %q, want %q", endpointFQDN, "postgres.feature-a.curri.stacklane.test")
	}
	if stackFQDN != "feature-a.curri.stacklane.test" {
		t.Fatalf("stackFQDN = %q, want %q", stackFQDN, "feature-a.curri.stacklane.test")
	}
}

func TestBuildFQDNs_WithoutInstance(t *testing.T) {
	t.Parallel()
	endpointFQDN, stackFQDN := domain.BuildFQDNs("postgres", "feature-a", "", "stacklane.test")
	if endpointFQDN != "postgres.feature-a.stacklane.test" {
		t.Fatalf("endpointFQDN = %q, want %q", endpointFQDN, "postgres.feature-a.stacklane.test")
	}
	if stackFQDN != "feature-a.stacklane.test" {
		t.Fatalf("stackFQDN = %q, want %q", stackFQDN, "feature-a.stacklane.test")
	}
}

func TestEndpointMapKey(t *testing.T) {
	t.Parallel()
	got := domain.EndpointMapKey("postgres.feature-a.curri.stacklane.test", 5432, domain.ProtocolTCP)
	want := "postgres.feature-a.curri.stacklane.test|5432|tcp"
	if got != want {
		t.Fatalf("EndpointMapKey = %q, want %q", got, want)
	}
}

func TestValidateSlug_Valid(t *testing.T) {
	t.Parallel()
	valid := []string{
		"a",
		"0",
		"feature-a",
		"postgres",
		"ab",
		"a1b2c3",
		"a" + strings.Repeat("b", 61) + "c", // 63 chars
	}
	for _, s := range valid {
		if err := domain.ValidateSlug(s); err != nil {
			t.Errorf("ValidateSlug(%q) unexpected error: %v", s, err)
		}
	}
}

func TestValidateSlug_Invalid(t *testing.T) {
	t.Parallel()
	invalid := []string{
		"",
		"Feature",             // uppercase
		"feature_a",           // underscore
		"feature.a",           // dot
		"-feature",            // leading hyphen
		"feature-",            // trailing hyphen
		"FEATURE-A",           // uppercase
		strings.Repeat("a", 64), // too long
	}
	for _, s := range invalid {
		if err := domain.ValidateSlug(s); err == nil {
			t.Errorf("ValidateSlug(%q) want error, got nil", s)
		}
	}
}

func TestValidatePort(t *testing.T) {
	t.Parallel()
	if err := domain.ValidatePort(1); err != nil {
		t.Fatalf("ValidatePort(1): %v", err)
	}
	if err := domain.ValidatePort(65535); err != nil {
		t.Fatalf("ValidatePort(65535): %v", err)
	}
	if err := domain.ValidatePort(0); err == nil {
		t.Fatal("ValidatePort(0) want error")
	}
}
