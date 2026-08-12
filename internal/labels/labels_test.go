package labels_test

import (
	"strings"
	"testing"

	"github.com/aleksclark/stacklane/internal/domain"
	"github.com/aleksclark/stacklane/internal/labels"
)

func baseLabels() map[string]string {
	return map[string]string{
		labels.ComposeProjectKey: "compose-proj",
		labels.ComposeServiceKey: "db",
		labels.EnableKey:         "true",
		labels.ProjectKey:        "curri",
		labels.InstanceKey:       "feature-a",
		labels.EndpointKey:       "postgres",
		labels.ProtocolKey:       "tcp",
		labels.PortKey:           "5432",
		labels.TargetPortKey:     "5432",
	}
}

func TestParse_ValidLabels_StackKeyAndFields(t *testing.T) {
	t.Parallel()
	meta, ok, err := labels.Parse(baseLabels(), "")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if !ok {
		t.Fatal("Parse ok=false, want true")
	}
	if meta.StackKey != domain.StackKey("curri/feature-a") {
		t.Errorf("StackKey = %q, want %q", meta.StackKey, "curri/feature-a")
	}
	if meta.ProjectSlug != "curri" || meta.InstanceSlug != "feature-a" || meta.EndpointName != "postgres" {
		t.Errorf("slugs = project=%q instance=%q endpoint=%q", meta.ProjectSlug, meta.InstanceSlug, meta.EndpointName)
	}
	if meta.Protocol != domain.ProtocolTCP {
		t.Errorf("Protocol = %q, want tcp", meta.Protocol)
	}
	if meta.PublicPort != 5432 || meta.TargetPort != 5432 {
		t.Errorf("ports public=%d target=%d", meta.PublicPort, meta.TargetPort)
	}
	if meta.ComposeProject != "compose-proj" || meta.ServiceName != "db" {
		t.Errorf("compose project=%q service=%q", meta.ComposeProject, meta.ServiceName)
	}
	if !meta.Enabled {
		t.Error("Enabled = false, want true")
	}

	// FQDN construction via domain helpers
	endpointFQDN, stackFQDN := domain.BuildFQDNs(meta.EndpointName, meta.ProjectSlug, meta.InstanceSlug, "stacklane.test")
	if endpointFQDN != "postgres.feature-a.curri.stacklane.test" {
		t.Errorf("endpointFQDN = %q", endpointFQDN)
	}
	if stackFQDN != "feature-a.curri.stacklane.test" {
		t.Errorf("stackFQDN = %q", stackFQDN)
	}
}

func TestParse_EnableTrueAndOne(t *testing.T) {
	t.Parallel()
	for _, enable := range []string{"true", "1"} {
		l := baseLabels()
		l[labels.EnableKey] = enable
		meta, ok, err := labels.Parse(l, "")
		if err != nil || !ok {
			t.Fatalf("enable=%q: ok=%v err=%v", enable, ok, err)
		}
		if !meta.Enabled {
			t.Fatalf("enable=%q: Enabled=false", enable)
		}
	}
}

func TestParse_EnableTRUE_Ignored(t *testing.T) {
	t.Parallel()
	l := baseLabels()
	l[labels.EnableKey] = "TRUE"
	_, ok, err := labels.Parse(l, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("enable=TRUE should be ignored (ok=false)")
	}
}

func TestParse_MissingComposeProject_Ignored(t *testing.T) {
	t.Parallel()
	l := baseLabels()
	delete(l, labels.ComposeProjectKey)
	_, ok, err := labels.Parse(l, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("missing compose project should be ignored")
	}
}

func TestParse_MissingComposeService_Ignored(t *testing.T) {
	t.Parallel()
	l := baseLabels()
	delete(l, labels.ComposeServiceKey)
	_, ok, err := labels.Parse(l, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("missing compose service should be ignored")
	}
}

func TestParse_UppercaseSlugRejected(t *testing.T) {
	t.Parallel()
	l := baseLabels()
	l[labels.ProjectKey] = "Feature-A"
	_, ok, err := labels.Parse(l, "")
	if !ok && err == nil {
		// could be either ignore or error; plan says enabled-ish but invalid → error
	}
	if err == nil {
		t.Fatal("uppercase slug should return error")
	}
	if ok {
		t.Fatal("uppercase slug should not be ok")
	}
}

func TestParse_OverlongLabelRejected(t *testing.T) {
	t.Parallel()
	l := baseLabels()
	l[labels.EndpointKey] = strings.Repeat("a", 129)
	_, ok, err := labels.Parse(l, "")
	if err == nil {
		t.Fatal("overlong label should return error")
	}
	if ok {
		t.Fatal("overlong label should not be ok")
	}
}

func TestParse_OverlongComposeProjectRejected(t *testing.T) {
	t.Parallel()
	l := baseLabels()
	l[labels.ComposeProjectKey] = strings.Repeat("p", 129)
	_, ok, err := labels.Parse(l, "")
	if err == nil {
		t.Fatal("overlong compose project should return error")
	}
	if ok {
		t.Fatal("overlong compose project should not be ok")
	}
}

func TestParse_InvalidPortZeroRejected(t *testing.T) {
	t.Parallel()
	l := baseLabels()
	l[labels.PortKey] = "0"
	_, ok, err := labels.Parse(l, "")
	if err == nil {
		t.Fatal("port 0 should return error")
	}
	if ok {
		t.Fatal("port 0 should not be ok")
	}
}

func TestParse_InvalidPortLeadingPlusRejected(t *testing.T) {
	t.Parallel()
	l := baseLabels()
	l[labels.PortKey] = "+5432"
	_, ok, err := labels.Parse(l, "")
	if err == nil {
		t.Fatal("leading + port should return error")
	}
	if ok {
		t.Fatal("leading + port should not be ok")
	}
}

func TestParse_DefaultInstanceApplied(t *testing.T) {
	t.Parallel()
	l := baseLabels()
	delete(l, labels.InstanceKey)
	// default instance is the worktree/clone slug when the label is omitted
	meta, ok, err := labels.Parse(l, "feature-a")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if meta.InstanceSlug != "feature-a" {
		t.Fatalf("InstanceSlug = %q, want feature-a", meta.InstanceSlug)
	}
	if meta.StackKey != domain.StackKey("curri/feature-a") {
		t.Fatalf("StackKey = %q, want curri/feature-a", meta.StackKey)
	}
}

func TestParse_NoInstanceNoDefault(t *testing.T) {
	t.Parallel()
	l := baseLabels()
	delete(l, labels.InstanceKey)
	meta, ok, err := labels.Parse(l, "")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if meta.InstanceSlug != "" {
		t.Fatalf("InstanceSlug = %q, want empty", meta.InstanceSlug)
	}
	if meta.StackKey != domain.StackKey("curri") {
		t.Fatalf("StackKey = %q, want curri", meta.StackKey)
	}
}

func TestParse_ProtocolUDPRejected(t *testing.T) {
	t.Parallel()
	l := baseLabels()
	l[labels.ProtocolKey] = "udp"
	_, ok, err := labels.Parse(l, "")
	if err == nil {
		t.Fatal("udp protocol should return error")
	}
	if ok {
		t.Fatal("udp protocol should not be ok")
	}
}

func TestParse_ProtocolDefaultsToTCP(t *testing.T) {
	t.Parallel()
	l := baseLabels()
	delete(l, labels.ProtocolKey)
	meta, ok, err := labels.Parse(l, "")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if meta.Protocol != domain.ProtocolTCP {
		t.Fatalf("Protocol = %q, want tcp", meta.Protocol)
	}
}

func TestParse_TargetPortDefaultsToPublic(t *testing.T) {
	t.Parallel()
	l := baseLabels()
	delete(l, labels.TargetPortKey)
	meta, ok, err := labels.Parse(l, "")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if meta.TargetPort != meta.PublicPort || meta.TargetPort != 5432 {
		t.Fatalf("TargetPort = %d, want 5432", meta.TargetPort)
	}
}

func TestParse_MissingEnable_Ignored(t *testing.T) {
	t.Parallel()
	l := baseLabels()
	delete(l, labels.EnableKey)
	_, ok, err := labels.Parse(l, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("missing enable should be ignored")
	}
}

func TestContractKeyConstants(t *testing.T) {
	t.Parallel()
	checks := map[string]string{
		labels.ComposeProjectKey: "com.docker.compose.project",
		labels.ComposeServiceKey: "com.docker.compose.service",
		labels.EnableKey:         "stacklane.enable",
		labels.ProjectKey:        "stacklane.project",
		labels.InstanceKey:       "stacklane.instance",
		labels.EndpointKey:       "stacklane.endpoint",
		labels.ProtocolKey:       "stacklane.protocol",
		labels.PortKey:           "stacklane.port",
		labels.TargetPortKey:     "stacklane.target_port",
	}
	for got, want := range checks {
		if got != want {
			t.Errorf("const = %q, want %q", got, want)
		}
	}
}
