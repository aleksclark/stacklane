package domain

import "strconv"

// MakeStackKey builds the durable stack identity from project and optional instance.
// If instance is empty, the key is just the project slug.
func MakeStackKey(project, instance string) StackKey {
	if instance == "" {
		return StackKey(project)
	}
	return StackKey(project + "/" + instance)
}

// BuildFQDNs constructs endpoint and stack FQDNs under baseDomain.
// When instance is empty, the instance label segment is omitted.
func BuildFQDNs(endpoint, project, instance, baseDomain string) (endpointFQDN, stackFQDN string) {
	if instance != "" {
		endpointFQDN = endpoint + "." + project + "." + instance + "." + baseDomain
		stackFQDN = project + "." + instance + "." + baseDomain
		return endpointFQDN, stackFQDN
	}
	endpointFQDN = endpoint + "." + project + "." + baseDomain
	stackFQDN = project + "." + baseDomain
	return endpointFQDN, stackFQDN
}

// EndpointMapKey builds the DesiredState.Endpoints map key:
// FQDN + "|" + port + "|" + protocol.
func EndpointMapKey(fqdn string, port uint16, proto Protocol) string {
	return fqdn + "|" + strconv.FormatUint(uint64(port), 10) + "|" + string(proto)
}
