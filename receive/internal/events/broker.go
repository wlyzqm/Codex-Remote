package events

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
)

const (
	maxSubscribers  = 16
	subscriberQueue = 32
)

// Event is one immutable message in the receiver's bounded replay window.
type Event struct {
	ID   uint64
	Data []byte
}

// Broker fans events out to browsers and keeps a bounded replay window for
// EventSource reconnects. Slow subscribers are disconnected rather than being
// allowed to grow memory without a limit.
type Broker struct {
	mu          sync.Mutex
	instanceID  string
	nextID      uint64
	maxEvents   int
	maxBytes    int
	ring        []Event
	ringBytes   int
	nextSubID   uint64
	subscribers map[uint64]chan Event
}

func New(maxEvents, maxBytes int) *Broker {
	if maxEvents < 1 {
		maxEvents = 1
	}
	if maxBytes < 1024 {
		maxBytes = 1024
	}
	var seed [12]byte
	_, _ = rand.Read(seed[:])
	return &Broker{
		instanceID:  hex.EncodeToString(seed[:]),
		maxEvents:   maxEvents,
		maxBytes:    maxBytes,
		subscribers: make(map[uint64]chan Event),
	}
}

func (b *Broker) InstanceID() string {
	return b.instanceID
}

func (b *Broker) LatestID() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.nextID
}

func (b *Broker) SubscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subscribers)
}

func (b *Broker) Publish(data []byte) Event {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.nextID++
	if len(data) > b.maxBytes {
		// A single pathological notification must not defeat the replay
		// window's byte ceiling. Browsers can recover authoritative state with
		// thread/read after this explicit reset marker.
		data = []byte(`{"type":"reset","reason":"event_too_large"}`)
	}
	event := Event{ID: b.nextID, Data: append([]byte(nil), data...)}
	b.ring = append(b.ring, event)
	b.ringBytes += len(event.Data)
	for len(b.ring) > b.maxEvents || b.ringBytes > b.maxBytes {
		b.ringBytes -= len(b.ring[0].Data)
		b.ring = b.ring[1:]
	}

	for id, ch := range b.subscribers {
		select {
		case ch <- event:
		default:
			delete(b.subscribers, id)
			close(ch)
		}
	}
	return event
}

// Subscribe returns replay events newer than afterID, whether the caller fell
// outside the replay window, a live channel, and an idempotent cancel function.
// An afterID of zero means "start live" and intentionally does not replay old
// events from before the browser connected.
func (b *Broker) Subscribe(afterID uint64) (replay []Event, reset bool, live <-chan Event, accepted bool, cancel func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.subscribers) >= maxSubscribers {
		ch := make(chan Event)
		close(ch)
		return nil, false, ch, false, func() {}
	}

	if afterID > b.nextID {
		reset = true
	} else if afterID > 0 && len(b.ring) == 0 {
		reset = true
	} else if afterID > 0 {
		oldest := b.ring[0].ID
		if afterID+1 < oldest {
			reset = true
		} else {
			for _, event := range b.ring {
				if event.ID > afterID {
					replay = append(replay, event)
				}
			}
		}
	}

	b.nextSubID++
	id := b.nextSubID
	ch := make(chan Event, subscriberQueue)
	b.subscribers[id] = ch
	var once sync.Once
	cancel = func() {
		once.Do(func() {
			b.mu.Lock()
			if current, ok := b.subscribers[id]; ok {
				delete(b.subscribers, id)
				close(current)
			}
			b.mu.Unlock()
		})
	}
	return replay, reset, ch, true, cancel
}
