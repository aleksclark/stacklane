package labels

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/aleksclark/stacklane/internal/domain"
)

// Meta holds parsed Stacklane + Compose identity for one container endpoint.
type Meta struct {
	Enabled        bool
	ComposeProject string
	ServiceName    string
	ProjectSlug    string
	InstanceSlug   string
	EndpointName   string
	Protocol       domain.Protocol
	PublicPort     uint16
	TargetPort     uint16
	StackKey       domain.StackKey
}

// Parse interprets container labels into Meta.
//
// ok=false with err=nil means the container should be ignored (not enabled / missing
// compose identity). err!=nil means the container looked enabled-ish but had invalid
// values — callers should skip and warn.
func Parse(labels map[string]string, defaultInstance string) (meta Meta, ok bool, err error) {
	if labels == nil {
		return Meta{}, false, nil
	}

	// Cap scan of stacklane.* keys (forward-compat ignore of unknowns).
	stacklaneCount := 0
	for k := range labels {
		if strings.HasPrefix(k, "stacklane.") {
			stacklaneCount++
			if stacklaneCount > 64 {
				return Meta{}, false, fmt.Errorf("too many stacklane.* labels (max 64)")
			}
		}
	}

	enable, hasEnable := labels[EnableKey]
	if !hasEnable {
		return Meta{}, false, nil
	}
	if err := checkLabelValueLen(EnableKey, enable); err != nil {
		return Meta{}, false, err
	}
	// Exact match only: "1" or "true". TRUE/yes/etc → ignore.
	if enable != "1" && enable != "true" {
		return Meta{}, false, nil
	}

	composeProject, hasProject := labels[ComposeProjectKey]
	composeService, hasService := labels[ComposeServiceKey]
	if !hasProject || composeProject == "" || !hasService || composeService == "" {
		return Meta{}, false, nil
	}
	if err := checkLabelValueLen(ComposeProjectKey, composeProject); err != nil {
		return Meta{}, false, err
	}
	if len(composeProject) > MaxComposeProjectBytes {
		return Meta{}, false, fmt.Errorf("%s exceeds %d bytes", ComposeProjectKey, MaxComposeProjectBytes)
	}
	if err := checkLabelValueLen(ComposeServiceKey, composeService); err != nil {
		return Meta{}, false, err
	}

	projectSlug, err := requireSlug(labels, ProjectKey)
	if err != nil {
		return Meta{}, false, err
	}
	endpointName, err := requireSlug(labels, EndpointKey)
	if err != nil {
		return Meta{}, false, err
	}

	instanceSlug := ""
	if v, ok := labels[InstanceKey]; ok && v != "" {
		if err := checkLabelValueLen(InstanceKey, v); err != nil {
			return Meta{}, false, err
		}
		if err := domain.ValidateSlug(v); err != nil {
			return Meta{}, false, fmt.Errorf("%s: %w", InstanceKey, err)
		}
		instanceSlug = v
	} else if defaultInstance != "" {
		if err := domain.ValidateSlug(defaultInstance); err != nil {
			return Meta{}, false, fmt.Errorf("default instance: %w", err)
		}
		instanceSlug = defaultInstance
	}

	protocol := domain.ProtocolTCP
	if v, ok := labels[ProtocolKey]; ok && v != "" {
		if err := checkLabelValueLen(ProtocolKey, v); err != nil {
			return Meta{}, false, err
		}
		if v != string(domain.ProtocolTCP) {
			return Meta{}, false, fmt.Errorf("%s: unsupported protocol %q (only tcp allowed)", ProtocolKey, v)
		}
		protocol = domain.ProtocolTCP
	}

	publicPort, err := requirePort(labels, PortKey)
	if err != nil {
		return Meta{}, false, err
	}

	targetPort := publicPort
	if v, ok := labels[TargetPortKey]; ok && v != "" {
		p, err := parsePortValue(TargetPortKey, v)
		if err != nil {
			return Meta{}, false, err
		}
		targetPort = p
	}

	return Meta{
		Enabled:        true,
		ComposeProject: composeProject,
		ServiceName:    composeService,
		ProjectSlug:    projectSlug,
		InstanceSlug:   instanceSlug,
		EndpointName:   endpointName,
		Protocol:       protocol,
		PublicPort:     publicPort,
		TargetPort:     targetPort,
		StackKey:       domain.MakeStackKey(projectSlug, instanceSlug),
	}, true, nil
}

func requireSlug(labels map[string]string, key string) (string, error) {
	v, ok := labels[key]
	if !ok || v == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	if err := checkLabelValueLen(key, v); err != nil {
		return "", err
	}
	if err := domain.ValidateSlug(v); err != nil {
		return "", fmt.Errorf("%s: %w", key, err)
	}
	return v, nil
}

func requirePort(labels map[string]string, key string) (uint16, error) {
	v, ok := labels[key]
	if !ok || v == "" {
		return 0, fmt.Errorf("%s is required", key)
	}
	return parsePortValue(key, v)
}

func parsePortValue(key, v string) (uint16, error) {
	if err := checkLabelValueLen(key, v); err != nil {
		return 0, err
	}
	// Reject leading +/spaces and non-numeric strictly.
	if v != strings.TrimSpace(v) {
		return 0, fmt.Errorf("%s: invalid port %q", key, v)
	}
	if v == "" || v[0] == '+' || v[0] == '-' {
		return 0, fmt.Errorf("%s: invalid port %q", key, v)
	}
	for _, r := range v {
		if !unicode.IsDigit(r) {
			return 0, fmt.Errorf("%s: invalid port %q", key, v)
		}
	}
	n, err := strconv.ParseUint(v, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid port %q", key, v)
	}
	if n == 0 {
		return 0, fmt.Errorf("%s: port 0 is not allowed", key)
	}
	p := uint16(n)
	if err := domain.ValidatePort(p); err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return p, nil
}

func checkLabelValueLen(key, v string) error {
	if len(v) > MaxLabelValueBytes {
		return fmt.Errorf("%s exceeds %d bytes", key, MaxLabelValueBytes)
	}
	return nil
}
