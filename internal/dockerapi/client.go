package dockerapi

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	mobyclient "github.com/moby/moby/client"
)

// watchedContainerActions are Docker container lifecycle actions that should
// trigger reconcile (plan §11.1). health_status may arrive as a prefix.
var watchedContainerActions = map[string]struct{}{
	"start":         {},
	"die":           {},
	"destroy":       {},
	"stop":          {},
	"pause":         {},
	"unpause":       {},
	"health_status": {},
	"rename":        {},
}

// sdkAPI is the subset of the official Docker client used by sdkClient.
type sdkAPI interface {
	ContainerList(ctx context.Context, options mobyclient.ContainerListOptions) (mobyclient.ContainerListResult, error)
	Events(ctx context.Context, options mobyclient.EventsListOptions) mobyclient.EventsResult
	Close() error
}

// sdkClient adapts the official Docker/Moby client to dockerapi.Client.
type sdkClient struct {
	api sdkAPI
}

// NewClient connects to Docker at dockerHost (e.g. unix:///var/run/docker.sock).
// Empty dockerHost uses the SDK default / environment.
func NewClient(dockerHost string) (Client, error) {
	opts := []mobyclient.Opt{}
	if dockerHost != "" {
		opts = append(opts, mobyclient.WithHost(dockerHost))
	} else {
		opts = append(opts, mobyclient.FromEnv)
	}
	cli, err := mobyclient.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &sdkClient{api: cli}, nil
}

// ListRunning returns currently running containers with ID, Name, Labels, Ports.
func (c *sdkClient) ListRunning(ctx context.Context) ([]Container, error) {
	res, err := c.api.ContainerList(ctx, mobyclient.ContainerListOptions{
		All: false, // running only
	})
	if err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(res.Items))
	for _, item := range res.Items {
		out = append(out, mapSummary(item))
	}
	return out, nil
}

// Events streams container lifecycle events relevant to reconcile.
func (c *sdkClient) Events(ctx context.Context) (<-chan Event, <-chan error) {
	outEvents := make(chan Event, 32)
	outErrs := make(chan error, 1)

	filters := make(mobyclient.Filters).Add("type", string(events.ContainerEventType))
	res := c.api.Events(ctx, mobyclient.EventsListOptions{Filters: filters})

	go func() {
		defer close(outEvents)
		defer close(outErrs)
		for {
			select {
			case <-ctx.Done():
				// Drain SDK error if present so its goroutine can exit cleanly.
				select {
				case <-res.Err:
				default:
				}
				return
			case err, ok := <-res.Err:
				if ok && err != nil {
					// Context cancel is normal shutdown, not a stream failure.
					if ctx.Err() != nil {
						return
					}
					outErrs <- err
				}
				return
			case msg, ok := <-res.Messages:
				if !ok {
					return
				}
				if msg.Type != events.ContainerEventType {
					continue
				}
				action := normalizeAction(string(msg.Action))
				if _, want := watchedContainerActions[action]; !want {
					continue
				}
				ev := Event{
					Type:    string(msg.Type),
					Action:  action,
					ActorID: msg.Actor.ID,
					Time:    eventTime(msg),
				}
				select {
				case outEvents <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return outEvents, outErrs
}

// Close closes the underlying SDK client.
func (c *sdkClient) Close() error {
	return c.api.Close()
}

func mapSummary(s container.Summary) Container {
	name := ""
	if len(s.Names) > 0 {
		name = strings.TrimPrefix(s.Names[0], "/")
	}
	labels := s.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	return Container{
		ID:     s.ID,
		Name:   name,
		Labels: labels,
		Ports:  mapPortSummaries(s.Ports),
	}
}

func mapPortSummaries(ports []container.PortSummary) []PortBinding {
	if len(ports) == 0 {
		return nil
	}
	out := make([]PortBinding, 0, len(ports))
	for _, p := range ports {
		hostIP := ""
		if p.IP.IsValid() {
			hostIP = p.IP.String()
		}
		out = append(out, PortBinding{
			HostIP:        hostIP,
			HostPort:      p.PublicPort,
			ContainerPort: p.PrivatePort,
			Protocol:      strings.ToLower(p.Type),
		})
	}
	return out
}

func normalizeAction(action string) string {
	// health_status events may be "health_status: healthy" etc.
	if strings.HasPrefix(action, "health_status") {
		return "health_status"
	}
	return action
}

func eventTime(msg events.Message) time.Time {
	if msg.TimeNano > 0 {
		return time.Unix(0, msg.TimeNano)
	}
	if msg.Time > 0 {
		return time.Unix(msg.Time, 0)
	}
	return time.Time{}
}
