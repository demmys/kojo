package agent

import "sync"

// chatBroadcaster fans out events from a single source channel to multiple
// subscribers. It keeps a log of all events so that late-joining subscribers
// can replay the full history.
type chatBroadcaster struct {
	mu   sync.Mutex
	log  []ChatEvent
	subs map[*chatSub]struct{}
	done bool
}

type chatSub struct {
	ch chan ChatEvent
}

func newChatBroadcaster(src <-chan ChatEvent) *chatBroadcaster {
	b := &chatBroadcaster{subs: make(map[*chatSub]struct{})}
	go b.run(src)
	return b
}

func (b *chatBroadcaster) run(src <-chan ChatEvent) {
	for ev := range src {
		terminal := ev.Type == "done" || ev.Type == "error"

		b.mu.Lock()
		b.log = append(b.log, ev)

		if terminal {
			// Terminal events: best-effort delivery then close.
			// Remove subs under lock, send outside lock to avoid
			// deadlock with Unsubscribe. Non-blocking send prevents
			// deadlock on full/orphaned channels; the subscriber
			// will synthesize from transcript on channel close.
			channels := make([]chan ChatEvent, 0, len(b.subs))
			for sub := range b.subs {
				channels = append(channels, sub.ch)
				delete(b.subs, sub)
			}
			b.mu.Unlock()
			for _, ch := range channels {
				select {
				case ch <- ev:
				default:
				}
				close(ch)
			}
		} else {
			for sub := range b.subs {
				select {
				case sub.ch <- ev:
				default:
					// subscriber too slow, skip non-terminal event
				}
			}
			b.mu.Unlock()
		}
	}

	// Source closed — close any remaining subscriber channels.
	b.mu.Lock()
	b.done = true
	for sub := range b.subs {
		close(sub.ch)
	}
	b.subs = make(map[*chatSub]struct{})
	b.mu.Unlock()
}

// Subscribe returns all past events and a channel for future events.
// Call the returned unsub function when done to avoid leaking the channel.
// If the source is already closed, the returned channel is pre-closed.
func (b *chatBroadcaster) Subscribe() (past []ChatEvent, live <-chan ChatEvent, unsub func()) {
	ch := make(chan ChatEvent, 64)
	sub := &chatSub{ch: ch}

	b.mu.Lock()
	defer b.mu.Unlock()

	past = make([]ChatEvent, len(b.log))
	copy(past, b.log)

	if b.done {
		close(ch)
		return past, ch, func() {}
	}

	b.subs[sub] = struct{}{}
	unsub = func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.subs[sub]; ok {
			delete(b.subs, sub)
			close(sub.ch)
		}
	}
	return past, ch, unsub
}
