package fake_test

import (
	"context"
	"testing"
	"time"

	"github.com/aleksclark/stacklane/internal/dockerapi"
	"github.com/aleksclark/stacklane/internal/dockerapi/fake"
)

func TestFake_ListRunning(t *testing.T) {
	f := fake.NewFake()
	f.SetContainers([]dockerapi.Container{
		{ID: "abc", Name: "one", Labels: map[string]string{"k": "v"}},
		{ID: "def", Name: "two"},
	})

	got, err := f.ListRunning(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].ID != "abc" || got[1].ID != "def" {
		t.Fatalf("unexpected containers: %+v", got)
	}
	// Mutating returned slice must not affect store.
	got[0].ID = "mutated"
	again, err := f.ListRunning(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if again[0].ID != "abc" {
		t.Fatal("ListRunning should return a copy")
	}
}

func TestFake_EventsDeliverStartAndDie(t *testing.T) {
	f := fake.NewFake()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events, errs := f.Events(ctx)

	start := dockerapi.Event{Type: "container", Action: "start", ActorID: "c1", Time: time.Unix(1, 0)}
	die := dockerapi.Event{Type: "container", Action: "die", ActorID: "c1", Time: time.Unix(2, 0)}
	f.Emit(start)
	f.Emit(die)

	gotStart := recvEvent(t, events, errs)
	if gotStart.Action != "start" || gotStart.ActorID != "c1" {
		t.Fatalf("start = %+v", gotStart)
	}
	gotDie := recvEvent(t, events, errs)
	if gotDie.Action != "die" || gotDie.ActorID != "c1" {
		t.Fatalf("die = %+v", gotDie)
	}
}

func TestFake_EventsCancelClosesChannel(t *testing.T) {
	f := fake.NewFake()
	ctx, cancel := context.WithCancel(context.Background())
	events, errs := f.Events(ctx)
	cancel()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}
		case err, ok := <-errs:
			if ok && err != nil && err != context.Canceled {
				// allow nil close
				if err != context.Canceled {
					// channel closed is fine
				}
			}
			// keep waiting for events close
		case <-deadline:
			t.Fatal("events channel did not close after cancel")
		}
	}
}

func TestFake_ImplementsClient(t *testing.T) {
	var _ dockerapi.Client = fake.NewFake()
}

func TestFake_CloseIdempotent(t *testing.T) {
	f := fake.NewFake()
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func recvEvent(t *testing.T, events <-chan dockerapi.Event, errs <-chan error) dockerapi.Event {
	t.Helper()
	select {
	case ev, ok := <-events:
		if !ok {
			t.Fatal("events closed unexpectedly")
		}
		return ev
	case err := <-errs:
		t.Fatalf("unexpected error: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for event")
	}
	return dockerapi.Event{}
}
