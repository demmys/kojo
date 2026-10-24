package server

import "sync"

// externalHub tracks live external-agent event streams. Phase 2 fills
// in event fan-out; Phase 1 only needs presence and forced disconnect.
type externalHub struct {
	mu      sync.Mutex
	streams map[string]map[*externalStream]struct{}
	closed  bool
}

type externalStream struct {
	done   chan struct{}
	once   sync.Once
	reason string
}

func (st *externalStream) close(reason string) {
	st.once.Do(func() { st.reason = reason; close(st.done) })
}

func newExternalHub() *externalHub {
	return &externalHub{streams: map[string]map[*externalStream]struct{}{}}
}

func (h *externalHub) online(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.streams[id]) > 0
}

func (h *externalHub) disconnect(id, reason string) {
	h.mu.Lock()
	set := h.streams[id]
	delete(h.streams, id)
	h.mu.Unlock()
	for st := range set {
		st.close(reason)
	}
}

func (h *externalHub) shutdown() {
	h.mu.Lock()
	h.closed = true
	all := h.streams
	h.streams = map[string]map[*externalStream]struct{}{}
	h.mu.Unlock()
	for _, set := range all {
		for st := range set {
			st.close("shutdown")
		}
	}
}

// Phase 2 hooks.
func (s *Server) registerExternalGroupRoutes(mux interface{}) {}
func (s *Server) onExternalAgentChanged(id string)            {}
func (s *Server) onExternalAgentDeleted(id string)            {}
