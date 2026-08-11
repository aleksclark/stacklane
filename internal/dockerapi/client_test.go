package dockerapi

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	mobyclient "github.com/moby/moby/client"
)

func TestMapPortSummaries_PreservesHostIPAndPorts(t *testing.T) {
	ip := netip.MustParseAddr("127.0.0.1")
	got := mapPortSummaries([]container.PortSummary{
		{IP: ip, PrivatePort: 5432, PublicPort: 40001, Type: "tcp"},
		{IP: netip.MustParseAddr("0.0.0.0"), PrivatePort: 8080, PublicPort: 80, Type: "TCP"},
	})
	if len(got) != 2 {
		t.Fatalf("len=%d", len(got))
	}
	if got[0].HostIP != "127.0.0.1" || got[0].HostPort != 40001 || got[0].ContainerPort != 5432 || got[0].Protocol != "tcp" {
		t.Fatalf("got[0]=%+v", got[0])
	}
	if got[1].HostIP != "0.0.0.0" || got[1].Protocol != "tcp" {
		t.Fatalf("got[1]=%+v", got[1])
	}
}

func TestMapSummary_StripsLeadingSlashName(t *testing.T) {
	c := mapSummary(container.Summary{
		ID:     "abc123",
		Names:  []string{"/my-service"},
		Labels: map[string]string{"stacklane.enable": "true"},
		Ports:  []container.PortSummary{{IP: netip.MustParseAddr("127.0.0.1"), PrivatePort: 80, PublicPort: 8080, Type: "tcp"}},
	})
	if c.ID != "abc123" || c.Name != "my-service" {
		t.Fatalf("got %+v", c)
	}
	if c.Labels["stacklane.enable"] != "true" {
		t.Fatalf("labels=%v", c.Labels)
	}
	if len(c.Ports) != 1 || c.Ports[0].HostPort != 8080 {
		t.Fatalf("ports=%+v", c.Ports)
	}
}

func TestNormalizeAction_HealthStatusPrefix(t *testing.T) {
	if got := normalizeAction("health_status: unhealthy"); got != "health_status" {
		t.Fatalf("got %q", got)
	}
	if got := normalizeAction("start"); got != "start" {
		t.Fatalf("got %q", got)
	}
}

func TestSDKClient_ListRunningMapsContainers(t *testing.T) {
	api := &stubSDK{
		list: mobyclient.ContainerListResult{
			Items: []container.Summary{
				{
					ID:     "id1",
					Names:  []string{"/one"},
					Labels: map[string]string{"a": "b"},
					Ports: []container.PortSummary{
						{IP: netip.MustParseAddr("127.0.0.1"), PrivatePort: 5432, PublicPort: 40001, Type: "tcp"},
					},
				},
			},
		},
	}
	c := &sdkClient{api: api}
	got, err := c.ListRunning(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "id1" || got[0].Name != "one" {
		t.Fatalf("got %+v", got)
	}
	if !api.listCalled || api.listAll {
		t.Fatalf("expected ContainerList All=false, got called=%v all=%v", api.listCalled, api.listAll)
	}
}

func TestSDKClient_EventsFiltersAndMaps(t *testing.T) {
	msgCh := make(chan events.Message, 4)
	errCh := make(chan error, 1)
	api := &stubSDK{
		eventsResult: mobyclient.EventsResult{Messages: msgCh, Err: errCh},
	}
	c := &sdkClient{api: api}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	evCh, errs := c.Events(ctx)

	// ignored action
	msgCh <- events.Message{Type: events.ContainerEventType, Action: events.ActionCreate, Actor: events.Actor{ID: "x"}}
	// watched
	msgCh <- events.Message{
		Type:   events.ContainerEventType,
		Action: events.ActionStart,
		Actor:  events.Actor{ID: "c1"},
		Time:   100,
	}
	msgCh <- events.Message{
		Type:   events.ContainerEventType,
		Action: events.ActionHealthStatusUnhealthy,
		Actor:  events.Actor{ID: "c1"},
		Time:   101,
	}

	select {
	case ev := <-evCh:
		if ev.Action != "start" || ev.ActorID != "c1" || ev.Type != "container" {
			t.Fatalf("start ev=%+v", ev)
		}
		if !ev.Time.Equal(time.Unix(100, 0)) {
			t.Fatalf("time=%v", ev.Time)
		}
	case err := <-errs:
		t.Fatalf("err %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timeout start")
	}

	select {
	case ev := <-evCh:
		if ev.Action != "health_status" {
			t.Fatalf("health ev=%+v", ev)
		}
	case err := <-errs:
		t.Fatalf("err %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timeout health")
	}

	if api.eventsFilters == nil {
		t.Fatal("expected events filters")
	}
}

func TestNewClient_InvalidHost(t *testing.T) {
	_, err := NewClient("not a valid host url!!!")
	if err == nil {
		t.Fatal("expected error for invalid docker host")
	}
}

func TestSDKClient_Close(t *testing.T) {
	api := &stubSDK{}
	c := &sdkClient{api: api}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if !api.closed {
		t.Fatal("expected Close on SDK")
	}
}

type stubSDK struct {
	list         mobyclient.ContainerListResult
	listErr      error
	listCalled   bool
	listAll      bool
	eventsResult mobyclient.EventsResult
	eventsFilters mobyclient.Filters
	closed       bool
}

func (s *stubSDK) ContainerList(ctx context.Context, options mobyclient.ContainerListOptions) (mobyclient.ContainerListResult, error) {
	s.listCalled = true
	s.listAll = options.All
	if s.listErr != nil {
		return mobyclient.ContainerListResult{}, s.listErr
	}
	return s.list, nil
}

func (s *stubSDK) Events(ctx context.Context, options mobyclient.EventsListOptions) mobyclient.EventsResult {
	s.eventsFilters = options.Filters
	return s.eventsResult
}

func (s *stubSDK) Close() error {
	s.closed = true
	return nil
}
