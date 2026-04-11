package server

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// --- Git Handlers ---

func (s *Server) handleGitStatus(w http.ResponseWriter, r *http.Request) {
	workDir := r.URL.Query().Get("workDir")
	result, err := s.git.Status(workDir)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, result)
}

func (s *Server) handleGitLog(w http.ResponseWriter, r *http.Request) {
	workDir := r.URL.Query().Get("workDir")
	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := fmt.Sscanf(l, "%d", &limit); n != 1 || err != nil {
			limit = 20
		}
	}
	if limit < 1 {
		limit = 1
	}
	skip := 0
	if sk := r.URL.Query().Get("skip"); sk != "" {
		if n, err := fmt.Sscanf(sk, "%d", &skip); n != 1 || err != nil || skip < 0 {
			skip = 0
		}
	}
	result, err := s.git.Log(workDir, limit, skip)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, result)
}

func (s *Server) handleGitDiff(w http.ResponseWriter, r *http.Request) {
	workDir := r.URL.Query().Get("workDir")
	ref := r.URL.Query().Get("ref")
	result, err := s.git.Diff(workDir, ref)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, result)
}

func (s *Server) handleGitExec(w http.ResponseWriter, r *http.Request) {
	var req struct {
		WorkDir string   `json:"workDir"`
		Args    []string `json:"args"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	result, err := s.git.Exec(req.WorkDir, req.Args)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, result)
}
