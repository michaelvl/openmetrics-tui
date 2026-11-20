package managed

import (
	"errors"
	"testing"
	"time"
)

func recv(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
		return Event{}
	}
}

func TestBroadcastReachesEverySubscriber(t *testing.T) {
	var b Broadcaster
	a, unsubA := b.Subscribe()
	c, unsubC := b.Subscribe()
	defer unsubA()
	defer unsubC()
	want := Event{Child: "victoria-metrics", Err: errors.New("boom")}
	b.Send(want)
	if got := recv(t, a); got != want {
		t.Errorf("a got %+v", got)
	}
	if got := recv(t, c); got != want {
		t.Errorf("c got %+v", got)
	}
}

func TestBroadcastSkipsUnsubscribedAndNeverBlocks(t *testing.T) {
	var b Broadcaster
	stuck, _ := b.Subscribe() // never read: its buffer fills up
	gone, unsubGone := b.Subscribe()
	live, unsubLive := b.Subscribe()
	defer unsubLive()
	unsubGone()
	unsubGone() // idempotent

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 10*subscriberBuffer; i++ {
			b.Send(Event{Child: "vm"})
			select {
			case <-live:
			default:
				t.Error("live subscriber missed an event")
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Send blocked on a full subscriber")
	}
	if len(stuck) != subscriberBuffer {
		t.Errorf("stuck subscriber holds %d events, want %d", len(stuck), subscriberBuffer)
	}
	if len(gone) != 0 {
		t.Errorf("unsubscribed channel received %d events", len(gone))
	}
}
