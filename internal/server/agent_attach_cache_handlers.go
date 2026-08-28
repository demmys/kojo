package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/blob"
)

// attachCacheResponse reports what the purge actually removed. Deleted and
// Bytes are deliberately NOT omitempty: "there was nothing cached" is the
// interesting answer for an operator who just pressed the button, and
// omitting the fields would make it indistinguishable from an older server
// that never sent them.
type attachCacheResponse struct {
	Deleted int   `json:"deleted"`
	Bytes   int64 `json:"bytes"`
	Failed  int   `json:"failed,omitempty"`
}

// agentAttachPrefix is the blob namespace every ingested attachment for an
// agent lands under: scope=global, path=agents/{agentID}/attach/{messageID}/
// {filename} (see peer_blob_ingest_handler and attach_scan). The trailing
// slash is load-bearing — without it the prefix would also match a sibling
// agent whose ID merely starts with this one's.
func agentAttachPrefix(agentID string) string {
	return "agents/" + agentID + "/attach/"
}

// handleGetAgentAttachCache GET /api/v1/agents/{id}/attach-cache
//
// Reports the size of the agent's ingested-attachment cache so the settings
// screen can show what the purge button would free before it is pressed.
func (s *Server) handleGetAgentAttachCache(w http.ResponseWriter, r *http.Request) {
	objs, ok := s.listAgentAttachBlobs(w, r)
	if !ok {
		return
	}
	var total int64
	for _, o := range objs {
		total += o.Size
	}
	writeJSONResponse(w, http.StatusOK, attachCacheResponse{Deleted: len(objs), Bytes: total})
}

// handleDeleteAgentAttachCache DELETE /api/v1/agents/{id}/attach-cache
//
// Deletes every blob under agents/{id}/attach/. This is a cache purge, not a
// transcript edit: past chat messages keep their attachment references and
// will render as broken links afterwards. That trade is deliberate — the
// operator asked to reclaim the disk now, and rewriting historical messages
// is both lossy and far more dangerous than a dead thumbnail.
//
// Owner-only. It is not in isSelfScopedRoute, so an agent token cannot wipe
// its own (or anyone's) attachment history.
func (s *Server) handleDeleteAgentAttachCache(w http.ResponseWriter, r *http.Request) {
	// Owner-only. AllowNonOwner admits RolePeer to the whole /api/v1/agents/
	// surface, and this route is exempt from proxying, so without an
	// explicit check a paired peer could wipe THIS device's blobs through a
	// route that never forwards anywhere.
	if !auth.FromContext(r.Context()).IsOwner() {
		writeError(w, http.StatusForbidden, "forbidden", "attachment cache purge is owner-only")
		return
	}
	objs, ok := s.listAgentAttachBlobs(w, r)
	if !ok {
		return
	}
	var deleted, failed int
	var freed int64
	for _, o := range objs {
		// Conditional delete: List and Delete are separate operations, so
		// an attachment ingested into an already-listed path in between
		// would otherwise be destroyed by a purge the operator started
		// before it existed. Head gives the current etag; a mismatch (or a
		// blob that vanished under us) means the entry is no longer the one
		// that was listed, so leave it alone rather than counting it.
		cur, err := s.blob.Head(blob.ScopeGlobal, o.Path)
		if err != nil {
			if !errors.Is(err, blob.ErrNotFound) {
				failed++
				s.logger.Warn("attach cache purge: head failed", "path", o.Path, "err", err)
			}
			continue
		}
		// An empty etag (no blob_refs row, or a row with no digest —
		// possible for files written before the ref table existed)
		// degrades Delete to unconditional. That is the right trade: a
		// blob the button could never remove would be worse than a
		// vanishingly rare lost race on a file with no recorded digest.
		err = s.blob.Delete(blob.ScopeGlobal, o.Path, blob.DeleteOptions{IfMatch: cur.ETag})
		switch {
		case err == nil:
			deleted++
			freed += cur.Size
		case errors.Is(err, blob.ErrNotFound):
			// Already gone between Head and Delete. Not a failure.
		case errors.Is(err, blob.ErrETagMismatch):
			// Rewritten under us, so the blob is still on disk. Counting
			// this as success would let the UI report an empty cache while
			// a file survives; surface it so the operator re-probes.
			failed++
		default:
			failed++
			s.logger.Warn("attach cache purge: delete failed", "path", o.Path, "err", err)
		}
	}
	writeJSONResponse(w, http.StatusOK, attachCacheResponse{Deleted: deleted, Bytes: freed, Failed: failed})
}

// listAgentAttachBlobs resolves the agent, checks the blob store is wired,
// and returns the objects under the agent's attach prefix. It writes the
// error response itself and returns ok=false on failure.
func (s *Server) listAgentAttachBlobs(w http.ResponseWriter, r *http.Request) ([]blob.Object, bool) {
	id := r.PathValue("id")
	// Remote-held agents count: their attachments replicate to this device
	// too, and the purge deliberately runs against the local blob store
	// (see the /attach-cache exemption in remoteAgentProxyMiddleware).
	if _, ok := s.agents.Get(id); !ok && s.agents.GetRemote(id) == nil {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return nil, false
	}
	if s.blob == nil {
		writeError(w, http.StatusServiceUnavailable, "blob_unavailable", "blob store not configured")
		return nil, false
	}
	// Guard against a malformed id turning the prefix into something that
	// matches a wider subtree. Agent IDs are "ag_" + hex, so anything with
	// a separator in it is a caller bug, not a lookup miss.
	if strings.ContainsAny(id, "/\\.") {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid agent id")
		return nil, false
	}
	objs, err := s.blob.List(blob.ScopeGlobal, agentAttachPrefix(id))
	if err != nil {
		s.writeBlobErr(w, err)
		return nil, false
	}
	return objs, true
}
