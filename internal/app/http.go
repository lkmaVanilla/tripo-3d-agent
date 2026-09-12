package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/asset"
	webui "github.com/lkmaVanilla/tripo-3d-agent/internal/web"
)

type visitorKey struct{}

const cookieName = "tripo_visitor"

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		s.respond(w, 200, map[string]any{"ready": s.ready, "model": "deepseek-v4-pro", "mode": s.source, "message": "真实生成需要配置 DEEPSEEK_API_KEY 和 TRIPO_API_KEY。"})
	})
	mux.HandleFunc("GET /api/sessions", func(w http.ResponseWriter, r *http.Request) {
		all, err := s.store.List(r.Context(), r.Context().Value(visitorKey{}).(string))
		if err != nil {
			s.fail(w, 500, err)
			return
		}
		sort.Slice(all, func(i, j int) bool { return all[i].Created.After(all[j].Created) })
		out := []any{}
		for _, v := range all {
			if v.Expires.IsZero() || time.Now().Before(v.Expires) {
				out = append(out, v.View())
			}
		}
		s.respond(w, 200, out)
	})
	mux.HandleFunc("POST /api/sessions", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Request string `json:"request"`
		}
		if !decode(w, r, &in) {
			return
		}
		in.Request = strings.TrimSpace(in.Request)
		if in.Request == "" || len(in.Request) > 8000 {
			s.fail(w, 400, fmt.Errorf("请输入不超过8000字节的资产需求"))
			return
		}
		v, err := s.Create(r.Context(), r.Context().Value(visitorKey{}).(string), in.Request)
		if err != nil {
			s.fail(w, 503, err)
			return
		}
		s.respond(w, 201, v.View())
	})
	mux.HandleFunc("GET /api/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		v, ok := s.owned(w, r)
		if !ok {
			return
		}
		events, err := s.store.Events(r.Context(), v.ID, 0)
		if err != nil {
			s.fail(w, 500, err)
			return
		}
		s.respond(w, 200, s.snapshot(v, events))
	})
	mux.HandleFunc("POST /api/sessions/{id}/answer", func(w http.ResponseWriter, r *http.Request) {
		v, ok := s.owned(w, r)
		if !ok {
			return
		}
		var in struct {
			Answer string `json:"answer"`
		}
		if !decode(w, r, &in) {
			return
		}
		if strings.TrimSpace(in.Answer) == "" {
			s.fail(w, 400, fmt.Errorf("回答不能为空"))
			return
		}
		if err := s.Answer(r.Context(), v.ID, in.Answer); err != nil {
			s.fail(w, 409, err)
			return
		}
		s.respond(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/sessions/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		v, ok := s.owned(w, r)
		if !ok {
			return
		}
		if err := s.Stop(r.Context(), v.ID); err != nil {
			s.fail(w, 409, err)
			return
		}
		s.respond(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/sessions/{id}/retry", func(w http.ResponseWriter, r *http.Request) {
		v, ok := s.owned(w, r)
		if !ok {
			return
		}
		if err := s.RetryQueue(r.Context(), v.ID); err != nil {
			s.fail(w, 409, err)
			return
		}
		s.respond(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /api/sessions/{id}/trace", func(w http.ResponseWriter, r *http.Request) {
		v, ok := s.owned(w, r)
		if !ok {
			return
		}
		events, err := s.store.Events(r.Context(), v.ID, 0)
		if err != nil {
			s.fail(w, 500, err)
			return
		}
		w.Header().Set("Content-Disposition", `attachment; filename="tripo-trace-`+v.ID+`.json"`)
		s.respond(w, 200, s.snapshot(v, events))
	})
	mux.HandleFunc("GET /api/sessions/{id}/artifacts/{artifact}", func(w http.ResponseWriter, r *http.Request) {
		v, ok := s.owned(w, r)
		if !ok {
			return
		}
		for _, a := range v.Artifacts {
			if a.ID == r.PathValue("artifact") {
				w.Header().Set("Content-Type", "model/gltf-binary")
				if r.URL.Query().Get("download") == "1" || !a.Report.Valid {
					w.Header().Set("Content-Disposition", `attachment; filename="asset.glb"`)
				}
				http.ServeFile(w, r, a.Path)
				return
			}
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("GET /api/sessions/{id}/events", s.eventsSocket)
	mux.Handle("/", webui.Handler())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Cache-Control", "no-store")
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Host != r.Host {
				s.fail(w, 403, fmt.Errorf("不允许跨站访问"))
				return
			}
		}
		var token string
		if c, err := r.Cookie(cookieName); err == nil {
			token = c.Value
		}
		issued, owner, err := s.store.Visitor(r.Context(), token, s.Config.VisitorTTL)
		if err != nil {
			s.fail(w, 500, err)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: issued, Path: "/", HttpOnly: true, Secure: s.Config.SecureCookie, SameSite: http.SameSiteLaxMode, MaxAge: int(s.Config.VisitorTTL.Seconds())})
		mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), visitorKey{}, owner)))
	})
}
func (s *Service) owned(w http.ResponseWriter, r *http.Request) (Session, bool) {
	v, err := s.store.Get(r.Context(), r.PathValue("id"))
	if err != nil || v.Owner != r.Context().Value(visitorKey{}).(string) || (!v.Expires.IsZero() && !time.Now().Before(v.Expires)) {
		s.fail(w, 404, fmt.Errorf("会话不存在或已到期"))
		return Session{}, false
	}
	return v, true
}
func decode(w http.ResponseWriter, r *http.Request, out any) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		http.Error(w, "需要 JSON 请求", 415)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		http.Error(w, "请求内容无效", 400)
		return false
	}
	return true
}
func (s *Service) respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(s.sanitize(v))
}
func (s *Service) fail(w http.ResponseWriter, status int, err error) {
	s.respond(w, status, map[string]string{"error": s.redact(err.Error())})
}
func (s *Service) sanitize(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return map[string]string{"error": "serialization"}
	}
	var out any
	_ = json.Unmarshal(b, &out)
	var walk func(any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			for k, v := range x {
				if k == "owner" || k == "api_key" || k == "authorization" || k == "cookie" {
					x[k] = "[REDACTED]"
				} else {
					x[k] = walk(v)
				}
			}
			return x
		case []any:
			for i, v := range x {
				x[i] = walk(v)
			}
			return x
		case string:
			x = s.redact(x)
			if u, err := url.Parse(x); err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.RawQuery != "" {
				u.RawQuery = "REDACTED"
				return u.String()
			}
			return x
		}
		return v
	}
	return walk(out)
}
func (s *Service) snapshot(v Session, events []Event) map[string]any {
	view := v.View()
	e := evaluate(v)
	violations := 0
	for _, event := range events {
		if event.Kind == "runtime_blocked" {
			var data struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(event.Data, &data)
			switch data.Code {
			case "budget", "constraint", "false_validation":
				violations++
			}
		}
	}
	status := "passed"
	if violations > 0 {
		status = "failed"
	}
	e.Checks = append(e.Checks, asset.Check{Name: "可核验的违规提议", Status: status, Detail: fmt.Sprintf("记录到 %d 次；Runtime 拦截不抵消 Agent 违规", violations)})
	view["evaluation"] = e
	return map[string]any{"session": view, "events": events, "source": v.Source}
}
func (s *Service) eventsSocket(w http.ResponseWriter, r *http.Request) {
	v, ok := s.owned(w, r)
	if !ok {
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	lifetime, cancelLifetime := context.WithCancel(r.Context())
	stop := context.AfterFunc(s.ctx, cancelLifetime)
	defer stop()
	defer cancelLifetime()
	ctx := conn.CloseRead(lifetime)
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	for {
		if !s.store.ValidVisitor(ctx, v.Owner) {
			return
		}
		v, err = s.store.Get(ctx, v.ID)
		if err != nil || (!v.Expires.IsZero() && time.Now().After(v.Expires)) {
			return
		}
		events, e := s.store.Events(ctx, v.ID, after)
		if e != nil {
			return
		}
		all, e := s.store.Events(ctx, v.ID, 0)
		if e != nil {
			return
		}
		snap := s.snapshot(v, all)
		snap["events"] = events
		writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = wsjson.Write(writeCtx, conn, s.sanitize(snap))
		cancel()
		if err != nil {
			return
		}
		if len(events) > 0 {
			after = events[len(events)-1].Seq
		}
		if err = wait(ctx, 500*time.Millisecond); err != nil {
			return
		}
	}
}
