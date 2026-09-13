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

// visitorKey 只在当前 HTTP 请求的 context 中携带凭证哈希，避免与字符串键冲突。
type visitorKey struct{}

const cookieName = "tripo_visitor"

// Handler 先核对 Origin 并签发/续期匿名 Cookie，再把访客归属传给具体路由。
// 会话内容与控制接口还必须调用 owned；知道会话 ID 并不等于拥有访问权限。
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	s.conversationRoutes(mux)
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
			c, e := s.store.GetRunConversation(r.Context(), v.ID)
			if e == nil && c.Available(time.Now()) {
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
		// HTTP 负责输入与归属；答案是否对应可恢复的等待点由 Service.Answer 校验。
		v, ok := s.owned(w, r)
		if !ok {
			return
		}
		if executionVersion(v) == ConversationPromptVersion {
			s.fail(w, 409, fmt.Errorf("聊天回答必须携带当前Run、问题和暂停代次，经会话消息入口提交"))
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
		// 这是停止本地执行的命令，不表示 Tripo 远端任务已经取消。
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
		// 导出复用页面快照及脱敏路径，不直接序列化包含 History/恢复材料的 Session。
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
		// 路径只能来自已校验归属的会话记录，不接受客户端提供任意本地文件路径。
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
		// 凭证失效会产生新的匿名归属；不会借此迁移旧会话或停止其后台任务。
		issued, owner, err := s.store.Visitor(r.Context(), token, s.Config.VisitorTTL)
		if err != nil {
			s.fail(w, 500, err)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: issued, Path: "/", HttpOnly: true, Secure: s.Config.SecureCookie, SameSite: http.SameSiteLaxMode, MaxAge: int(s.Config.VisitorTTL.Seconds())})
		mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), visitorKey{}, owner)))
	})
}

// owned 将不存在、他人所有和数据已过期统一返回 404，不泄露他人会话是否存在。
func (s *Service) owned(w http.ResponseWriter, r *http.Request) (Session, bool) {
	v, err := s.store.Get(r.Context(), r.PathValue("id"))
	c, ce := s.store.GetRunConversation(r.Context(), r.PathValue("id"))
	if err != nil || ce != nil || v.Owner != r.Context().Value(visitorKey{}).(string) || !c.Available(time.Now()) {
		s.fail(w, 404, fmt.Errorf("会话不存在或已到期"))
		return Session{}, false
	}
	return v, true
}

// decode 限制请求体大小并拒绝未知 JSON 字段，路由仍需校验各业务字段的取值。
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

// respond 是普通 JSON 响应的统一脱敏出口；WebSocket 写入也显式复用 sanitize。
func (s *Service) respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(s.sanitize(v))
}
func (s *Service) fail(w http.ResponseWriter, status int, err error) {
	s.respond(w, status, map[string]string{"error": s.redact(err.Error())})
}

// sanitize 在序列化副本上遮蔽已知敏感字段、配置密钥和独立 URL 字符串的查询参数。
// 持久化原记录不会被改写；调用方仍应先用 View/snapshot 排除内部恢复材料。
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
			// 供应商输入可能是任意形状的token，不能依赖file_前缀识别凭证。
			if _, params := x["face_limit"]; params {
				if input, ok := x["input"].(string); ok && input != "" && !strings.HasPrefix(input, "version:") {
					x["input"] = "[REDACTED_PROVIDER_INPUT]"
				}
			}
			for k, v := range x {
				if k == "owner" || k == "api_key" || k == "authorization" || k == "cookie" || k == "file_token" || k == "token" || k == "path" || k == "source_url" {
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
			if strings.HasPrefix(x, "file_") {
				return "[REDACTED_FILE_REFERENCE]"
			}
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

// snapshot 合并公开会话状态、事件及基于完整记录的单次核验。
// Runtime 拦截违规提议仍算行为证据，不能因为成功拦截就替 Agent 宣告通过。
func (s *Service) snapshot(v Session, events []Event) map[string]any {
	view := v.View()
	if executionVersion(v) == ConversationPromptVersion {
		view = conversationRunView(v)
	}
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
	e.Checks = append(e.Checks, proposalViolationCheck(violations))
	view["evaluation"] = e
	return map[string]any{"session": view, "events": events, "source": v.Source}
}

func proposalViolationCheck(violations int) asset.Check {
	status := "passed"
	if violations > 0 {
		status = "failed"
	}
	return asset.Check{Name: "可核验的违规提议", Status: status, Detail: fmt.Sprintf("记录到 %d 次；Runtime 拦截不抵消 Agent 违规", violations)}
}

// eventsSocket 是单向进度通道；回答、停止和重试仍通过 HTTP 命令提交。
// 每次轮询读取最新会话快照，并按 after 游标发送增量事件；断线不会取消后台生产。
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
	// 持续消费连接关闭信号，避免浏览器离开后继续写入；服务关闭也会结束此连接。
	ctx := conn.CloseRead(lifetime)
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	for {
		if !s.store.ValidVisitor(ctx, v.Owner) {
			return
		}
		v, err = s.store.Get(ctx, v.ID)
		c, ce := s.store.GetRunConversation(ctx, v.ID)
		if err != nil || ce != nil || !c.Available(time.Now()) {
			return
		}
		events, e := s.store.Events(ctx, v.ID, after)
		if e != nil {
			return
		}
		// 单次核验仍依赖完整历史；发送给浏览器的 events 则只包含游标后的增量。
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
