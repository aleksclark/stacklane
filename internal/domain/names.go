package domain

import "strconv"

// MakeStackKey builds the durable stack identity from project and optional instance.
// If instance is empty, the key is just the project slug.
// Convention: project is the stable product/org slug; instance is the worktree/clone.
func MakeStackKey(project, instance string) StackKey {
	if instance == "" {
		return StackKey(project)
	}
	return StackKey(project + "/" + instance)
}

// BuildFQDNs constructs endpoint and stack FQDNs under baseDomain.
// Hierarchy (left = most specific):
//
//	{endpoint}.{instance}.{project}.{base}   when instance is set
//	{endpoint}.{project}.{base}              when instance is empty
//
// Stack apex omits the endpoint label:
//
//	{instance}.{project}.{base}  or  {project}.{base}
//
// Example: endpoint=postgres, instance=aleks-stacklane-test, project=curri, base=test
//
//	→ postgres.aleks-stacklane-test.curri.test
func BuildFQDNs(endpoint, project, instance, baseDomain string) (endpointFQDN, stackFQDN string) {
	if instance != "" {
		endpointFQDN = endpoint + "." + instance + "." + project + "." + baseDomain
		stackFQDN = instance + "." + project + "." + baseDomain
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
