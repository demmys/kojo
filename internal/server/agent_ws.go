package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/loppo-llc/kojo/internal/agent"
)

// Agent WebSocket message types
type agentWSClientMsg struct {
	Type    string `json:"type"`    // "message", "abort"
	Content string `json:"content"` // for "message" type
}

func (s *Server) handleAgentWebSocket(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if agentID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing agent id")
		return
	}

	if _, ok := s.agents.Get(agentID); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+agentID)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"100.*.*.*", "*.ts.net", "localhost:*", "127.0.0.1:*"},
	})
	if err != nil {
		s.logger.Error("agent websocket accept failed", "err", err)
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(256 * 1024) // 256KB max for chat messages

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	s.logger.Info("agent websocket connected", "agent", agentID)

	// Channel for client messages (read goroutine → main loop)
	clientMsgs := make(chan agentWSClientMsg, 8)

	// Keepalive ping
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				pingCtx, pingCancel := context.WithTimeout(ctx, 10*time.Second)
				if err := conn.Ping(pingCtx); err != nil {
					pingCancel()
					cancel()
					return
				}
				pingCancel()
			}
		}
	}()

	// Read goroutine: continuously reads from client, decoupled from write
	go func() {
		defer cancel()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}

			var msg agentWSClientMsg
			if err := json.Unmarshal(data, &msg); err != nil {
				s.logger.Debug("invalid agent ws message", "err", err)
				continue
			}

			select {
			case clientMsgs <- msg:
			case <-ctx.Done():
				return
			}
		}
	}()

	// Main loop: process client messages and stream events
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-clientMsgs:
			switch msg.Type {
			case "message":
				if msg.Content == "" {
					continue
				}

				// Check if agent is busy
				if s.agents.IsBusy(agentID) {
					_ = writeJSON(ctx, conn, map[string]string{
						"type":         "error",
						"errorMessage": "agent is busy",
					})
					continue
				}

				// Send "thinking" status
				_ = writeJSON(ctx, conn, map[string]string{
					"type":   "status",
					"status": "thinking",
				})

				// Start chat
				events, err := s.agents.Chat(ctx, agentID, msg.Content)
				if err != nil {
					_ = writeJSON(ctx, conn, map[string]string{
						"type":         "error",
						"errorMessage": err.Error(),
					})
					continue
				}

				// Stream events to client, while also listening for abort
				s.streamAgentEvents(ctx, conn, events, agentID, clientMsgs)

			case "abort":
				s.agents.Abort(agentID)
			}
		}
	}
}

// streamAgentEvents streams chat events to the WebSocket while allowing
// abort messages to be processed concurrently.
func (s *Server) streamAgentEvents(
	ctx context.Context,
	conn *websocket.Conn,
	events <-chan agent.ChatEvent,
	agentID string,
	clientMsgs <-chan agentWSClientMsg,
) {
	for {
		select {
		case <-ctx.Done():
			s.agents.Abort(agentID)
			return
		case event, ok := <-events:
			if !ok {
				return // channel closed
			}
			if err := writeJSON(ctx, conn, event); err != nil {
				s.agents.Abort(agentID)
				return
			}
		case msg := <-clientMsgs:
			if msg.Type == "abort" {
				s.agents.Abort(agentID)
				// Drain remaining events with timeout
				drainTimer := time.NewTimer(10 * time.Second)
				defer drainTimer.Stop()
			drainLoop:
				for {
					select {
					case _, ok := <-events:
						if !ok {
							break drainLoop
						}
					case <-drainTimer.C:
						break drainLoop
					}
				}
				return
			}
			// Ignore other messages while streaming
		}
	}
}
