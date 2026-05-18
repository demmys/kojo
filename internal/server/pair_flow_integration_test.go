package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

// TestPairFlow_EndToEnd walks the Bearer-pairing flow as a black box:
// first POST mints join_secret, repeat POSTs without auth refuse,
// repeat POSTs with the secret refresh, operator approve mints the
// Bearer pair, authenticated poll delivers the pair, subsequent call
// with the permanent peer→Hub Bearer ACKs and consumes the stash.
//
// All in-process: httptest server + minimal Server struct. No tsnet,
// no spawned daemons, no shared kv path — Manager opens its kojo.db
// under the t.TempDir HOME so prod state is untouched.
func TestPairFlow_EndToEnd(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("APPDATA", "")
	t.Setenv("XDG_CONFIG_HOME", "")

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	mgr, err := agent.NewManager(logger)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	st := mgr.Store()
	if st == nil {
		t.Fatal("agent.Manager.Store() nil")
	}

	hubID := &peer.Identity{DeviceID: uuid.NewString(), Name: "hub-test"}
	if _, err := st.UpsertPeer(context.Background(), &store.PeerRecord{
		DeviceID: hubID.DeviceID,
		Name:     hubID.Name,
		Status:   store.PeerStatusOnline,
	}); err != nil {
		t.Fatalf("seed self row: %v", err)
	}

	s := &Server{
		logger:  logger,
		peerID:  hubID,
		agents:  mgr,
		version: "test",
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/peers/join-request", s.handleJoinRequest)
	mux.HandleFunc("GET /api/v1/peers/join-request/{deviceId}", s.handleJoinRequestPoll)
	// Bearer middleware fronts the mux — required so a /join-request
	// poll authenticated with a permanent peer→Hub Bearer gets the
	// principal stamped (callerHoldsPeerBearer reads from
	// Authorization header directly, but the middleware's TouchPeer
	// side-effect keeps OfflineSweeper out of the way).
	bearerPeer := peer.NewBearerPeerMiddleware(st, hubID.DeviceID)
	srv := httptest.NewServer(bearerPeer.Wrap(mux))
	defer srv.Close()

	peerDeviceID := uuid.NewString()

	// Step 1: first POST /join-request — no Authorization, fresh
	// device_id. Expect state=pending, joinSecret returned.
	body1 := mustJSON(t, map[string]string{
		"deviceId": peerDeviceID,
		"name":     "peer-test",
		"url":      "http://peer.example:8080",
	})
	resp1 := doJoinPost(t, srv.URL, body1, "")
	if resp1.State != "pending" {
		t.Fatalf("step1 state = %q want pending", resp1.State)
	}
	if resp1.JoinSecret == "" {
		t.Fatal("step1: joinSecret missing on fresh insert")
	}
	joinSecret := resp1.JoinSecret

	// Step 2: unauthenticated repeat POST — should 401.
	resp2Status := doJoinPostStatus(t, srv.URL, body1, "")
	if resp2Status != http.StatusUnauthorized {
		t.Fatalf("step2 (unauth repeat) status = %d want 401", resp2Status)
	}

	// Step 3: authenticated repeat POST — should land state=pending
	// but NO new joinSecret (single-use).
	resp3 := doJoinPost(t, srv.URL, body1, joinSecret)
	if resp3.State != "pending" {
		t.Fatalf("step3 state = %q want pending", resp3.State)
	}
	if resp3.JoinSecret != "" {
		t.Fatal("step3: joinSecret re-issued on repeat (should be single-use)")
	}

	// Step 4: operator approves — direct store call, simulating
	// what handleApprovePeerPending does. We do it through the
	// handler-side helper so the bearer mint + stash creation
	// runs.
	rec, joinHash, err := st.ApprovePeerPending(context.Background(), peerDeviceID)
	if err != nil {
		t.Fatalf("ApprovePeerPending: %v", err)
	}
	if rec.DeviceID != peerDeviceID {
		t.Fatalf("approve returned wrong device_id: %s", rec.DeviceID)
	}
	if err := s.mintAndStashPairingBearers(context.Background(), peerDeviceID, joinHash); err != nil {
		t.Fatalf("mintAndStashPairingBearers: %v", err)
	}

	// Step 5: authenticated poll — should return state=approved
	// + PeerBearer (raw) + HubBearer (raw).
	resp5 := doJoinPoll(t, srv.URL, peerDeviceID, joinSecret)
	if resp5.State != "approved" {
		t.Fatalf("step5 state = %q want approved", resp5.State)
	}
	if resp5.PeerBearer == "" {
		t.Fatal("step5: peerBearer missing")
	}
	if resp5.HubBearer == "" {
		t.Fatal("step5: hubBearer missing")
	}
	permanentPeerBearer := resp5.PeerBearer

	// Step 6: verify the permanent peer→Hub Bearer resolves to
	// RolePeer on a subsequent Bearer-authenticated call.
	tok, err := st.ResolvePeerToken(context.Background(), permanentPeerBearer)
	if err != nil {
		t.Fatalf("ResolvePeerToken permanent: %v", err)
	}
	if tok.DeviceID != peerDeviceID || tok.Role != store.PeerTokenRolePeerToHub {
		t.Fatalf("permanent bearer wrong shape: %+v", tok)
	}

	// Step 7: ACK poll — peer presents permanent Bearer, stash
	// should be consumed. Subsequent polls return state=approved
	// with EMPTY Bearer fields (one-shot delivery done).
	resp7 := doJoinPoll(t, srv.URL, peerDeviceID, permanentPeerBearer)
	if resp7.State != "approved" {
		t.Fatalf("step7 state = %q want approved", resp7.State)
	}
	if resp7.PeerBearer != "" {
		t.Fatalf("step7: peerBearer re-issued after ACK (got %q)", resp7.PeerBearer)
	}
	if resp7.HubBearer != "" {
		t.Fatalf("step7: hubBearer re-issued after ACK (got %q)", resp7.HubBearer)
	}

	// Step 8: stash should be gone.
	if _, err := st.GetKV(context.Background(), "peer/pairing_bearer_stash", peerDeviceID); err == nil {
		t.Fatal("step8: stash row still present after ACK")
	}

	_ = auth.RolePeer // imported via test for compilation only
}

// --- helpers ---

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func doJoinPost(t *testing.T, base string, body []byte, bearer string) joinRequestResponse {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/api/v1/peers/join-request", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST status %d: %s", resp.StatusCode, string(buf))
	}
	var out joinRequestResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func doJoinPostStatus(t *testing.T, base string, body []byte, bearer string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/api/v1/peers/join-request", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func doJoinPoll(t *testing.T, base, deviceID, bearer string) joinRequestResponse {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+"/api/v1/peers/join-request/"+deviceID, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET status %d: %s", resp.StatusCode, string(buf))
	}
	var out joinRequestResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}
