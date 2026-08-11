package dockerapi

import (
	"context"
	"time"
)

// PortBinding describes a published container port mapping.
type PortBinding struct {
	HostIP        string
	HostPort      uint16
	ContainerPort uint16
	Protocol      string // tcp
}

// Container is a running container snapshot relevant to stacklane.
type Container struct {
	ID     string
	Name   string
	Labels map[string]string
	Ports  []PortBinding
}

// Event is a Docker container lifecycle event.
type Event struct {
	Type    string // container
	Action  string // start, die, etc
	ActorID string
	Time    time.Time
}

// Client watches and lists Docker containers.
type Client interface {
	ListRunning(ctx context.Context) ([]Container, error)
	Events(ctx context.Context) (<-chan Event, <-chan error)
	Close() error
}
