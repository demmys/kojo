package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/loppo-llc/kojo/internal/agent"
)

func TestExternalQuestionAnswerRemoteAndNoReplay(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "uncertain"}[uncertain], func(t *testing.T) {
			var calls atomic.Int32
			holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/ready") {
					_ = json.NewEncoder(w).Encode(externalChatReadyResponse{Ready: true, HolderPeer: "holder"})
					return
				}
				calls.Add(1)
				var req externalChatSteerRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				if req.Content != "" || req.SessionKey != "slack:s" || req.Question == nil || req.Question.RequestID != "r" || req.Question.Answers["color"] != "Blue" {
					t.Errorf("bad answer envelope: %+v", req)
				}
				if uncertain {
					conn, _, _ := w.(http.Hijacker).Hijack()
					_ = conn.Close()
					return
				}
				writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
			}))
			defer holder.Close()
			_, router, id := prepareRemoteExternalChat(t, holder.URL)
			err := router.AnswerOneShotQuestion(context.Background(), id, "slack:s", agent.QuestionAnswer{RequestID: "r", Answers: map[string]any{"color": "Blue"}})
			if uncertain && !errors.Is(err, agent.ErrSteerDeliveryUncertain) || !uncertain && err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 {
				t.Fatalf("answer replayed %d times", calls.Load())
			}
		})
	}
}

func TestExternalQuestionOptInDoesNotBreakOldPeerDecoder(t *testing.T) {
	seen := false
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var oldRequest struct {
			Message       string `json:"message"`
			SessionKey    string `json:"sessionKey,omitempty"`
			HubMCPBaseURL string `json:"hubMcpBaseUrl,omitempty"`
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&oldRequest); err != nil {
			t.Error(err)
			http.Error(w, "unknown field", 400)
			return
		}
		if r.Header.Get("X-Kojo-Interactive-Questions") != "v1" {
			t.Error("missing opt-in header")
		}
		seen = true
		writeJSONResponse(w, 200, map[string]bool{"ok": true})
	}))
	defer holder.Close()
	_, router, id := prepareRemoteExternalChat(t, holder.URL)
	resp, _, err := router.postRemote(context.Background(), id, "holder", externalChatTextRequest{Message: "hello", SessionKey: "s", InteractiveQuestions: true})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !seen || resp.StatusCode != 200 {
		t.Fatal("old holder cannot accept ordinary turn")
	}
}
