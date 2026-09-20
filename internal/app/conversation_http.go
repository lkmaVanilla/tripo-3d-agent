package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

type conversationCommand struct {
	Kind            string `json:"kind"`
	Text            string `json:"text"`
	ClientMessageID string `json:"client_message_id"`
	VersionID       string `json:"version_id,omitempty"`
	RunID           string `json:"run_id,omitempty"`
	WaitID          string `json:"wait_id,omitempty"`
	Generation      int64  `json:"generation,omitempty"`
}

func (s *Service) conversationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/conversations", func(w http.ResponseWriter, r *http.Request) {
		all, err := s.store.ListConversations(r.Context(), r.Context().Value(visitorKey{}).(string))
		if err != nil {
			s.fail(w, 500, err)
			return
		}
		out := []Conversation{}
		for _, c := range all {
			if c.Available(time.Now()) {
				out = append(out, c)
			}
		}
		s.respond(w, 200, out)
	})
	mux.HandleFunc("POST /api/conversations", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Request         string `json:"request"`
			ClientMessageID string `json:"client_message_id"`
		}
		if !decode(w, r, &in) {
			return
		}
		in.Request = strings.TrimSpace(in.Request)
		if err := validConversationCommand(in.ClientMessageID, in.Request); err != nil {
			s.fail(w, 400, err)
			return
		}
		c, _, err := s.CreateAssetConversation(r.Context(), r.Context().Value(visitorKey{}).(string), in.Request, in.ClientMessageID)
		if err != nil {
			s.conversationFail(w, err)
			return
		}
		s.writeConversation(w, r, c.ID, 201)
	})
	mux.HandleFunc("GET /api/conversations/{id}", func(w http.ResponseWriter, r *http.Request) { s.writeConversation(w, r, r.PathValue("id"), 200) })
	mux.HandleFunc("POST /api/conversations/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		var in conversationCommand
		if !decode(w, r, &in) {
			return
		}
		in.Text = strings.TrimSpace(in.Text)
		if err := validConversationCommand(in.ClientMessageID, in.Text); err != nil {
			s.fail(w, 400, err)
			return
		}
		var err error
		owner := r.Context().Value(visitorKey{}).(string)
		switch in.Kind {
		case "message":
			if in.RunID != "" || in.WaitID != "" || in.Generation != 0 {
				s.fail(w, 400, fmt.Errorf("新目标不能携带旧问题身份"))
				return
			}
			_, err = s.ContinueConversation(r.Context(), r.PathValue("id"), owner, in.Text, in.ClientMessageID, in.VersionID)
		case "answer":
			_, _, err = s.store.AcceptConversationAnswer(r.Context(), r.PathValue("id"), owner, in.ClientMessageID, in.RunID, in.WaitID, in.VersionID, in.Generation, in.Text)
		default:
			s.fail(w, 400, fmt.Errorf("消息类型必须为message或answer"))
			return
		}
		if err != nil {
			s.conversationFail(w, err)
			return
		}
		s.writeConversation(w, r, r.PathValue("id"), 200)
	})
	for _, action := range []string{"stop", "retry"} {
		mux.HandleFunc("POST /api/conversations/{id}/runs/{run}/"+action, func(w http.ResponseWriter, r *http.Request) {
			c, ok := s.ownedConversation(w, r)
			if !ok {
				return
			}
			run, err := s.store.GetRunConversation(r.Context(), r.PathValue("run"))
			if err != nil || run.ID != c.ID {
				s.fail(w, 404, ErrConversationExpired)
				return
			}
			if action == "stop" {
				err = s.Stop(r.Context(), r.PathValue("run"))
			} else {
				err = s.RetryQueue(r.Context(), r.PathValue("run"))
			}
			if err != nil {
				s.conversationFail(w, err)
				return
			}
			s.writeConversation(w, r, c.ID, 200)
		})
	}
	mux.HandleFunc("GET /api/conversations/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		snap, err := s.readConversation(r, r.PathValue("id"))
		if err != nil {
			s.conversationFail(w, err)
			return
		}
		s.respond(w, 200, map[string]any{"messages": snap.Messages, "has_more": snap.HasMore})
	})
	mux.HandleFunc("GET /api/conversations/{id}/versions", func(w http.ResponseWriter, r *http.Request) {
		snap, err := s.readConversation(r, r.PathValue("id"))
		if err != nil {
			s.conversationFail(w, err)
			return
		}
		s.respond(w, 200, snap.Versions)
	})
	mux.HandleFunc("GET /api/conversations/{id}/versions/{version}/file", func(w http.ResponseWriter, r *http.Request) {
		c, ok := s.ownedConversation(w, r)
		if !ok {
			return
		}
		v, err := s.store.GetAssetVersion(r.Context(), c.ID, r.PathValue("version"))
		if err != nil {
			s.fail(w, 404, ErrInvalidVersion)
			return
		}
		w.Header().Set("Content-Type", "model/gltf-binary")
		if r.URL.Query().Get("download") == "1" || !v.Report.Valid {
			w.Header().Set("Content-Disposition", `attachment; filename="asset.glb"`)
		}
		http.ServeFile(w, r, v.Path)
	})
	mux.HandleFunc("GET /api/conversations/{id}/trace", func(w http.ResponseWriter, r *http.Request) {
		snap, err := s.readConversation(r, r.PathValue("id"))
		if err != nil {
			s.conversationFail(w, err)
			return
		}
		for snap.HasMore && len(snap.Messages) > 0 {
			page, e := s.store.ConversationSnapshot(r.Context(), snap.Conversation.ID, r.Context().Value(visitorKey{}).(string), 0, snap.Messages[0].Seq, 100)
			if e != nil {
				s.conversationFail(w, e)
				return
			}
			snap.Messages = append(page.Messages, snap.Messages...)
			snap.HasMore = page.HasMore
		}
		w.Header().Set("Content-Disposition", `attachment; filename="tripo-conversation-`+snap.Conversation.ID+`.json"`)
		s.respond(w, 200, snap)
	})
	mux.HandleFunc("GET /api/conversations/{id}/events", s.conversationSocket)
}

func (s *Service) ownedConversation(w http.ResponseWriter, r *http.Request) (Conversation, bool) {
	c, err := s.store.GetConversation(r.Context(), r.PathValue("id"))
	if err != nil || c.Owner != r.Context().Value(visitorKey{}).(string) || !c.Available(time.Now()) {
		s.fail(w, 404, ErrConversationExpired)
		return c, false
	}
	return c, true
}
func (s *Service) readConversation(r *http.Request, id string) (ConversationSnapshot, error) {
	after, err := queryCursor(r, "after")
	if err != nil {
		return ConversationSnapshot{}, err
	}
	before, err := queryCursor(r, "before")
	if err != nil {
		return ConversationSnapshot{}, err
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 {
			return ConversationSnapshot{}, fmt.Errorf("分页数量无效")
		}
		if limit > 100 {
			limit = 100
		}
	}
	return s.store.ConversationSnapshot(r.Context(), id, r.Context().Value(visitorKey{}).(string), after, before, limit)
}
func queryCursor(r *http.Request, key string) (int64, error) {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return 0, nil
	}
	v, e := strconv.ParseInt(raw, 10, 64)
	if e != nil || v < 0 {
		return 0, fmt.Errorf("事件或消息游标无效")
	}
	return v, nil
}
func (s *Service) writeConversation(w http.ResponseWriter, r *http.Request, id string, status int) {
	snap, err := s.readConversation(r, id)
	if err != nil {
		s.conversationFail(w, err)
		return
	}
	s.respond(w, status, snap)
}
func (s *Service) conversationFail(w http.ResponseWriter, err error) {
	code := 409
	if errors.Is(err, ErrConversationExpired) || errors.Is(err, ErrInvalidVersion) {
		code = 404
	}
	s.fail(w, code, err)
}

// conversationSocket 是会话级下行流；断线不会停止Run，游标跨Run保持有效。
func (s *Service) conversationSocket(w http.ResponseWriter, r *http.Request) {
	c, ok := s.ownedConversation(w, r)
	if !ok {
		return
	}
	after, err := queryCursor(r, "after")
	if err != nil {
		s.fail(w, 400, err)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	lifetime, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	ctx := conn.CloseRead(lifetime)
	for {
		if !s.store.ValidVisitor(ctx, c.Owner) {
			return
		}
		snap, e := s.store.ConversationSnapshot(ctx, c.ID, c.Owner, after, 0, 50)
		if e != nil {
			return
		}
		writeCtx, done := context.WithTimeout(ctx, 5*time.Second)
		e = wsjson.Write(writeCtx, conn, s.sanitize(snap))
		done()
		if e != nil {
			return
		}
		after = snap.Cursor
		if wait(ctx, 500*time.Millisecond) != nil {
			return
		}
	}
}
