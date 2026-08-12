package domain_test

import (
	"strings"
	"testing"

	"github.com/aleksclark/stacklane/internal/domain"
)

func TestMakeStackKey_WithInstance(t *testing.T) {
	t.Parallel()
	got := domain.MakeStackKey("curri", "feature-a")
	if got != domain.StackKey("curri/feature-a") {
		t.Fatalf("MakeStackKey = %q, want %q", got, "curri/feature-a")
	}
}

func TestMakeStackKey_WithoutInstance(t *testing.T) {
	t.Parallel()
	got := domain.MakeStackKey("curri", "")
	if got != domain.StackKey("curri") {
		t.Fatalf("MakeStackKey = %q, want %q", got, "curri")
	}
}

func TestBuildFQDNs_WithInstance(t *testing.T) {
	t.Parallel()
	// hierarchy: endpoint.instance.project.base
	endpointFQDN, stackFQDN := domain.BuildFQDNs("postgres", "curri", "feature-a", "stacklane.test")
	if endpointFQDN != "postgres.feature-a.curri.stacklane.test" {
		t.Fatalf("endpointFQDN = %q, want %q", endpointFQDN, "postgres.feature-a.curri.stacklane.test")
	}
	if stackFQDN != "feature-a.curri.stacklane.test" {
		t.Fatalf("stackFQDN = %q, want %q", stackFQDN, "feature-a.curri.stacklane.test")
	}
}

func TestBuildFQDNs_WithoutInstance(t *testing.T) {
	t.Parallel()
	endpointFQDN, stackFQDN := domain.BuildFQDNs("postgres", "curri", "", "stacklane.test")
	if endpointFQDN != "postgres.curri.stacklane.test" {
		t.Fatalf("endpointFQDN = %q, want %q", endpointFQDN, "postgres.curri.stacklane.test")
	}
	if stackFQDN != "curri.stacklane.test" {
		t.Fatalf("stackFQDN = %q, want %q", stackFQDN, "curri.stacklane.test")
	}
}

func TestBuildFQDNs_WorktreeHierarchy(t *testing.T) {
	t.Parallel()
	endpointFQDN, stackFQDN := domain.BuildFQDNs("postgres", "curri", "aleks-stacklane-test", "test")
	if endpointFQDN != "postgres.aleks-stacklane-test.curri.test" {
		t.Fatalf("endpointFQDN = %q, want %q", endpointFQDN, "postgres.aleks-stacklane-test.curri.test")
	}
	if stackFQDN != "aleks-stacklane-test.curri.test" {
		t.Fatalf("stackFQDN = %q, want %q", stackFQDN, "aleks-stacklane-test.curri.test")
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
		"Feature",               // uppercase
		"feature_a",             // underscore
		"feature.a",             // dot
		"-feature",              // leading hyphen
		"feature-",              // trailing hyphen
		"FEATURE-A",             // uppercase
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
