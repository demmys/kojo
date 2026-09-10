package agent

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

type codexRPCWriteError struct {
	Written int
	Err     error
}

func (e *codexRPCWriteError) Error() string { return e.Err.Error() }
func (e *codexRPCWriteError) Unwrap() error { return e.Err }

// codexSteerTurnWait bounds how long a steer call waits for the active
// turn id to become known. The turn/start RPC response normally arrives
// within milliseconds of the request, so this only trips when the
// app-server is wedged.
const codexSteerTurnWait = 15 * time.Second

// codexSteerRespWait bounds how long a steer call waits for the
// turn/steer RPC response after the request was written. The app-server
// validates expectedTurnId and answers immediately.
const codexSteerRespWait = 10 * time.Second

// codexSteerer injects additional user input into an in-flight codex
// turn via the app-server turn/steer RPC. Unlike claude (where steering
// is a bare fire-and-forget user-message line on stdin), codex requires
// the active turn id as a precondition (expectedTurnId) — and that id is
// only known once the turn/start RPC response (or the turn/started
// notification) has been read from the stream. steer() therefore blocks
// briefly on ready until parseCodexStream captures the id, and then
// waits for the turn/steer response so a rejected steer (turn already
// over) is reported as an error instead of being silently dropped.
type codexSteerer struct {
	threadID string
	// writeRPC allocates a request id and writes a JSON-RPC request to
	// the app-server stdin. Returns the id and any write error.
	writeRPC func(method string, params any) (int64, error)

	mu             sync.Mutex
	turnID         string
	retryUntil     time.Time // intentional overload backoff; extends only the turn-readiness wait
	steerUncertain bool      // sticky: a steer may have arrived without an acknowledgement
	closed         bool
	ready          chan struct{}        // closed once the current turnID is set or the steerer is closed
	readyDone      bool                 // guards close(ready); reset for an automatic continuation turn
	pending        map[int64]chan error // in-flight turn/steer request id -> response slot
}

func newCodexSteerer(threadID string, writeRPC func(method string, params any) (int64, error)) *codexSteerer {
	return &codexSteerer{
		threadID: threadID,
		writeRPC: writeRPC,
		ready:    make(chan struct{}),
		pending:  make(map[int64]chan error),
	}
}

// setTurnID records the active turn id and unblocks pending steer calls.
func (s *codexSteerer) setTurnID(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	if s.turnID == "" && !s.closed {
		s.turnID = id
		if !s.readyDone {
			close(s.ready)
			s.readyDone = true
		}
	}
	s.mu.Unlock()
}

// finishTurn atomically retires the completed turn id and prepares the steer
// handle for a possible automatic continuation. Existing waiters on the old
// readiness channel are woken so they can observe the replacement channel;
// in-flight steer RPCs remain tracked because their acknowledgements may be
// delivered just after turn/completed and consumed by the next parse loop.
func (s *codexSteerer) finishTurn() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if !s.readyDone {
		close(s.ready)
	}
	s.turnID = ""
	s.ready = make(chan struct{})
	s.readyDone = false
	s.mu.Unlock()
}

// close marks the turn as over: subsequent steer calls fail with
// ErrAgentNotBusy and steers still waiting for a response are failed.
// Idempotent.
func (s *codexSteerer) close() {
	s.mu.Lock()
	s.closed = true
	pending := s.pending
	s.pending = make(map[int64]chan error)
	if !s.readyDone {
		close(s.ready)
		s.readyDone = true
	}
	s.mu.Unlock()
	for _, ch := range pending {
		// These requests were already written. Completion before their RPC
		// responses arrive cannot prove rejection, so canonical history must
		// retain the input instead of offering a duplicate-producing fallback.
		ch <- fmt.Errorf("%w: codex turn ended before turn/steer was acknowledged", ErrSteerDeliveryUncertain)
	}
}

// resolve delivers the RPC response for an outstanding turn/steer
// request. rpcErr is nil on success. Reports whether id belonged to a
// steer request (so the caller can stop treating the response as
// unclaimed).
func (s *codexSteerer) resolve(id int64, rpcErr *rpcError) bool {
	s.mu.Lock()
	ch, ok := s.pending[id]
	if ok {
		delete(s.pending, id)
	}
	s.mu.Unlock()
	if !ok {
		return false
	}
	// Buffered; never blocks.
	if rpcErr == nil {
		ch <- nil
	} else {
		// Any rejection (no active turn, expectedTurnId mismatch, turn not
		// steerable) means the input did not enter the turn — surface it
		// as ErrAgentNotBusy so the HTTP layer answers 409 not_busy and
		// the client can fall back to sending a normal message.
		ch <- fmt.Errorf("codex turn/steer rejected: %s: %w", rpcErr.Message, ErrAgentNotBusy)
	}
	return true
}

// steer implements SteerFunc for the codex backend.
func (s *codexSteerer) steer(text string) error {
	return s.steerWithTimeouts(text, codexSteerTurnWait, codexSteerRespWait)
}

func (s *codexSteerer) steerWithTimeouts(text string, turnWait, responseWait time.Duration) error {
	waitUntil := time.Now().Add(turnWait)
	deadline := time.NewTimer(turnWait)
	defer deadline.Stop()

	for {
		// Read the current readiness channel under the lock. An automatic
		// continuation replaces this channel between turns; looping after it
		// closes avoids racing a steer onto the just-completed turn.
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return ErrAgentNotBusy
		}
		if s.turnID == "" {
			// A deliberate overload wait is not a wedged turn/start. Extend
			// existing waiters too, then retain the normal RPC-start allowance.
			if until := s.retryUntil.Add(turnWait); until.After(waitUntil) {
				waitUntil = until
				deadline.Reset(time.Until(waitUntil))
			}
			ready := s.ready
			s.mu.Unlock()
			select {
			case <-ready:
				continue
			case <-deadline.C:
				s.mu.Lock()
				extended := s.retryUntil.Add(turnWait).After(waitUntil)
				s.mu.Unlock()
				if extended {
					continue
				}
				return fmt.Errorf("codex: turn did not start within %s", turnWait)
			}
		}

		// Hold the lock across the write (mirroring claudeStdinWriter) so
		// finishTurn/close cannot change the expected turn id between the
		// check and the JSON-RPC request.
		id, err := s.writeRPC("turn/steer", map[string]any{
			"threadId":       s.threadID,
			"expectedTurnId": s.turnID,
			"input": []map[string]any{
				{"type": "text", "text": text},
			},
		})
		if err != nil {
			var writeErr *codexRPCWriteError
			if errors.As(err, &writeErr) && writeErr.Written == 0 {
				s.mu.Unlock()
				return fmt.Errorf("%w: codex turn/steer write: %v", ErrAgentNotBusy, err)
			}
			s.steerUncertain = true
			s.mu.Unlock()
			// The app-server may have received the full JSON-RPC frame before
			// the pipe surfaced a write error; do not let canonical history
			// roll back input whose delivery cannot be disproved.
			return fmt.Errorf("%w: codex turn/steer write: %v", ErrSteerDeliveryUncertain, err)
		}
		respCh := make(chan error, 1)
		s.pending[id] = respCh
		s.mu.Unlock()

		select {
		case err := <-respCh:
			return err
		case <-time.After(responseWait):
			s.mu.Lock()
			delete(s.pending, id)
			s.steerUncertain = true
			s.mu.Unlock()
			return fmt.Errorf("%w: codex turn/steer was not acknowledged within %s", ErrSteerDeliveryUncertain, responseWait)
		}
	}
}

// prepareOverloadRetry extends readiness deadlines before sleeping. Do not wait
// with an unacknowledged steer: its response timer is already running and its
// delivery is uncertain. A nil steerer is the ordinary noninteractive case.
func (s *codexSteerer) prepareOverloadRetry(delay time.Duration) bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.turnID != "" || len(s.pending) != 0 || s.steerUncertain {
		return false
	}
	s.retryUntil = time.Now().Add(delay)
	// Wake already-waiting steers so they see the extended deadline.
	if !s.readyDone {
		close(s.ready)
	}
	s.ready = make(chan struct{})
	s.readyDone = false
	return true
}
