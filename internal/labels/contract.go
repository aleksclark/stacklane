package labels

// Canonical Docker Compose and Stacklane label keys.
const (
	ComposeProjectKey = "com.docker.compose.project"
	ComposeServiceKey = "com.docker.compose.service"

	EnableKey     = "stacklane.enable"
	ProjectKey    = "stacklane.project"
	InstanceKey   = "stacklane.instance"
	EndpointKey   = "stacklane.endpoint"
	ProtocolKey   = "stacklane.protocol"
	PortKey       = "stacklane.port"
	TargetPortKey = "stacklane.target_port"
)

// MaxLabelValueBytes is the fail-closed cap for any single label value.
const MaxLabelValueBytes = 128

// MaxComposeProjectBytes is the fail-closed cap for compose project name retention.
const MaxComposeProjectBytes = 128
