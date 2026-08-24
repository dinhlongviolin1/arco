package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// ImageRelay is the outbound side of the Telegram image relay: send a local
// image into a session's operator channel (forum topic). The telegram bot
// implements it; nil when telegram is disabled, in which case the route 503s.
type ImageRelay interface {
	SendSessionImage(ctx context.Context, sessionID, path, caption string) (int64, error)
}

// EnableImageRelay installs the outbound relay behind POST /v1/image/send.
func (s *Server) EnableImageRelay(r ImageRelay) { s.imageRelay = r }

// ImageSendReq is the `arco image send` payload. Worktree is the CALLING
// worker's cwd — arco resolves which worker/session owns it, so the agent needs
// no id. Path is relative to that worktree (an absolute path must stay within it).
type ImageSendReq struct {
	Worktree string `json:"worktree"`
	Path     string `json:"path"`
	Caption  string `json:"caption"`
}

// ImageSendResp is the result of a successful send.
type ImageSendResp struct {
	OK        bool   `json:"ok"`
	MessageID int64  `json:"message_id"`
	Session   string `json:"session"`
}

func (s *Server) imageSend(w http.ResponseWriter, r *http.Request) {
	if s.imageRelay == nil {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("image relay not configured (telegram disabled)"))
		return
	}
	var req ImageSendReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Worktree == "" || req.Path == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("worktree and path are required"))
		return
	}
	worker, err := s.workerByWorktree(req.Worktree)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	// The file must live inside the worker's worktree — a worker can't ask arco to
	// exfiltrate /etc/passwd or another worker's tree through the relay.
	abs, err := resolveWithin(worker.Worktree, req.Path)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	mid, err := s.imageRelay.SendSessionImage(r.Context(), worker.OwnerSession, abs, req.Caption)
	if err != nil {
		writeErr(w, errStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, ImageSendResp{OK: true, MessageID: mid, Session: worker.OwnerSession})
}

// workerByWorktree finds the worker whose worktree contains cwd (longest match
// wins for nested trees).
func (s *Server) workerByWorktree(cwd string) (core.Worker, error) {
	cwd = filepath.Clean(cwd)
	ws, err := s.store.Reader().ListWorkers(core.WorkerFilter{})
	if err != nil {
		return core.Worker{}, err
	}
	var best core.Worker
	bestLen := -1
	for _, wk := range ws {
		if wk.Worktree == "" {
			continue
		}
		wt := filepath.Clean(wk.Worktree)
		if cwd == wt || strings.HasPrefix(cwd, wt+string(filepath.Separator)) {
			if len(wt) > bestLen {
				best, bestLen = wk, len(wt)
			}
		}
	}
	if bestLen < 0 {
		return core.Worker{}, fmt.Errorf("no worker owns worktree %q (run this from inside a worker's worktree)", cwd)
	}
	return best, nil
}

// resolveWithin joins rel onto base and rejects any result that escapes base.
func resolveWithin(base, rel string) (string, error) {
	base = filepath.Clean(base)
	p := rel
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, rel)
	}
	p = filepath.Clean(p)
	if p != base && !strings.HasPrefix(p, base+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the worktree", rel)
	}
	return p, nil
}
