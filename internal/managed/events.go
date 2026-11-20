package managed

import "sync"

// subscriberBuffer is how many events a subscriber can fall behind by before
// further ones are dropped for it. A subscriber only needs to learn that
// something changed, since it then reads Health.
const subscriberBuffer = 4

// Broadcaster fans events out to every subscriber. Sends never block: a
// subscriber with a full buffer misses the event rather than stalling the rest.
type Broadcaster struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

// Subscribe returns a channel of the events sent from now on and a function
// that unsubscribes it. The function is safe to call more than once; the
// channel is never closed.
func (b *Broadcaster) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, subscriberBuffer)
	b.mu.Lock()
	if b.subs == nil {
		b.subs = map[chan Event]struct{}{}
	}
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}

// Send delivers ev to every subscriber that has room for it.
func (b *Broadcaster) Send(ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}
