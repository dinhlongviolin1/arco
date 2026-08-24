package telegram

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// inboxSubdir is where an operator-sent image lands inside the target worker's
// worktree; the agent reads it from here (see the arco-image skill).
const inboxSubdir = ".arco/inbox"

// SendSessionImage is the OUTBOUND relay: a worker asks arco (via `arco image
// send`) to post a local image into its session's forum topic. The daemon holds
// the bot token, so the worker never sees it. Returns the sent message id.
func (b *Bot) SendSessionImage(ctx context.Context, sessionID, path, caption string) (int64, error) {
	thread, err := b.ensureTopic(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	m, err := b.api.SendPhoto(ctx, SendPhotoReq{ChatID: b.groupID, MessageThreadID: thread, Path: path, Caption: caption})
	if err != nil {
		return 0, err
	}
	return m.MessageID, nil
}

// handleInboundImage is the INBOUND relay: an operator photo/document posted in a
// session topic is downloaded into that session's active worker worktree
// (.arco/inbox/) and confirmed in the topic. Auth-gated like every inbound path.
func (b *Bot) handleInboundImage(ctx context.Context, m *Message) {
	if m.From == nil || !b.allowed[m.From.ID] {
		return
	}
	if m.MessageThreadID == 0 {
		_, _ = b.api.SendMessage(ctx, SendMessageReq{ChatID: b.groupID,
			Text: "📎 send images inside a session topic so I can route them to that worker"})
		return
	}
	sess, ok := b.sessionByTopic(m.MessageThreadID)
	if !ok {
		return
	}
	w, ok := b.activeWorker(sess.ID)
	if !ok || w.Worktree == "" {
		_, _ = b.api.SendMessage(ctx, SendMessageReq{ChatID: b.groupID, MessageThreadID: m.MessageThreadID,
			Text: "📎 image received, but this session has no active worker with a worktree to place it in"})
		return
	}
	fileID, name := imageFileID(m)
	if fileID == "" {
		_, _ = b.api.SendMessage(ctx, SendMessageReq{ChatID: b.groupID, MessageThreadID: m.MessageThreadID,
			Text: "unsupported attachment (send a photo or an image document)"})
		return
	}
	f, err := b.api.GetFile(ctx, fileID)
	if err != nil {
		b.imgErr(ctx, m.MessageThreadID, err)
		return
	}
	data, err := b.api.DownloadFile(ctx, f.FilePath)
	if err != nil {
		b.imgErr(ctx, m.MessageThreadID, err)
		return
	}
	if name == "" {
		name = filepath.Base(f.FilePath)
		if name == "" || name == "." {
			name = "image_" + strconv.FormatInt(m.MessageID, 10) + ".jpg"
		}
	}
	dir := filepath.Join(w.Worktree, inboxSubdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		b.imgErr(ctx, m.MessageThreadID, err)
		return
	}
	safe := sanitizeName(name)
	if err := os.WriteFile(filepath.Join(dir, safe), data, 0o644); err != nil {
		b.imgErr(ctx, m.MessageThreadID, err)
		return
	}
	rel := filepath.Join(inboxSubdir, safe)
	msg := fmt.Sprintf("📎 image saved for worker %s → %s", w.ID, rel)
	if m.Caption != "" {
		msg += "\ncaption: " + m.Caption
	}
	msg += "\n(the agent reads it from its worktree — see the arco-image skill)"
	_, _ = b.api.SendMessage(ctx, SendMessageReq{ChatID: b.groupID, MessageThreadID: m.MessageThreadID, Text: msg})
}

func (b *Bot) imgErr(ctx context.Context, thread int64, err error) {
	_, _ = b.api.SendMessage(ctx, SendMessageReq{ChatID: b.groupID, MessageThreadID: thread,
		Text: "image relay error: " + err.Error()})
}

// sessionByTopic reverse-maps a forum topic id to its session.
func (b *Bot) sessionByTopic(thread int64) (core.Session, bool) {
	sessions, err := b.store.ListSessions(core.SessionFilter{})
	if err != nil {
		return core.Session{}, false
	}
	for _, s := range sessions {
		if s.TGTopicID != nil && *s.TGTopicID == thread {
			return s, true
		}
	}
	return core.Session{}, false
}

// activeWorker picks the session's worker to hand an inbound image to: a running
// one with a worktree, else any non-terminal one with a worktree.
func (b *Bot) activeWorker(sessionID string) (core.Worker, bool) {
	ws, err := b.store.ListWorkers(core.WorkerFilter{OwnerSession: sessionID})
	if err != nil {
		return core.Worker{}, false
	}
	for _, w := range ws {
		if w.State == core.WorkerRunning && w.Worktree != "" {
			return w, true
		}
	}
	for _, w := range ws {
		if !w.State.Terminal() && w.Worktree != "" {
			return w, true
		}
	}
	return core.Worker{}, false
}

// imageFileID returns the file id + suggested name for a photo or image document.
func imageFileID(m *Message) (id, name string) {
	if p := m.LargestPhoto(); p != "" {
		return p, ""
	}
	if m.Document != nil {
		return m.Document.FileID, m.Document.FileName
	}
	return "", ""
}

// sanitizeName reduces a Telegram-supplied filename to a safe basename (no path
// traversal) for writing under the worktree inbox.
func sanitizeName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.ReplaceAll(name, "..", "_")
	if name == "" || name == "." || name == "/" {
		return "image"
	}
	return name
}
