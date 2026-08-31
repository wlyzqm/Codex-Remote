package events

import (
	"fmt"
	"testing"
	"time"
)

func TestBrokerReplayAndReset(t *testing.T) {
	b := New(3, 4096)
	for i := 0; i < 4; i++ {
		b.Publish([]byte(fmt.Sprintf("%d", i)))
	}

	replay, reset, _, accepted, cancel := b.Subscribe(2)
	defer cancel()
	if reset || !accepted {
		t.Fatal("unexpected reset inside replay window")
	}
	if len(replay) != 2 || replay[0].ID != 3 || replay[1].ID != 4 {
		t.Fatalf("unexpected replay: %#v", replay)
	}

	b.Publish([]byte("4"))
	_, reset, _, _, cancelOld := b.Subscribe(1)
	defer cancelOld()
	if !reset {
		t.Fatal("expected reset outside replay window")
	}
}

func TestBrokerLiveAndCancel(t *testing.T) {
	b := New(8, 4096)
	_, _, live, accepted, cancel := b.Subscribe(0)
	if !accepted {
		t.Fatal("first subscriber was rejected")
	}
	event := b.Publish([]byte("hello"))
	select {
	case got := <-live:
		if got.ID != event.ID || string(got.Data) != "hello" {
			t.Fatalf("unexpected event: %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for live event")
	}
	cancel()
	if b.SubscriberCount() != 0 {
		t.Fatal("subscriber was not removed")
	}
}

func TestBrokerReplacesOversizedEvent(t *testing.T) {
	b := New(8, 1024)
	previous := b.Publish([]byte(`{"type":"seed"}`))
	event := b.Publish(make([]byte, 2048))
	if string(event.Data) != `{"type":"reset","reason":"event_too_large"}` {
		t.Fatalf("oversized event was retained: %d bytes", len(event.Data))
	}
	replay, reset, _, _, cancel := b.Subscribe(previous.ID)
	defer cancel()
	if reset || len(replay) != 1 || len(replay[0].Data) > 1024 {
		t.Fatalf("unexpected bounded replay: reset=%v replay=%#v", reset, replay)
	}
}

func TestBrokerResetsFutureCursor(t *testing.T) {
	b := New(8, 4096)
	_, reset, _, _, cancel := b.Subscribe(99)
	defer cancel()
	if !reset {
		t.Fatal("future cursor from another receiver instance was accepted")
	}
}

func TestBrokerCapsSubscribers(t *testing.T) {
	b := New(8, 4096)
	var cancels []func()
	for i := 0; i < maxSubscribers; i++ {
		_, _, _, accepted, cancel := b.Subscribe(0)
		if !accepted {
			t.Fatalf("subscriber %d was rejected", i)
		}
		cancels = append(cancels, cancel)
	}
	_, _, _, accepted, cancel := b.Subscribe(0)
	cancel()
	if accepted {
		t.Fatal("subscriber limit was not enforced")
	}
	for _, release := range cancels {
		release()
	}
}
