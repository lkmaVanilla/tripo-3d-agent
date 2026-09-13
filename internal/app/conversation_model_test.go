package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cloudwego/eino-ext/components/model/deepseek"
	"github.com/cloudwego/eino/schema"
)

// 用真实SDK核对发出的请求，避免仅断言本地选项却没有传到提供方；旧profile仍为4096。
func TestConversationModelOutputBudgetIsProfileScoped(t *testing.T) {
	for _, version := range []string{PromptVersion, CurrentPromptVersion, ConversationPromptVersion} {
		t.Run(version, func(t *testing.T) {
			requests := make(chan int, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					MaxTokens int `json:"max_tokens"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					http.Error(w, "bad JSON", 400)
					return
				}
				requests <- body.MaxTokens
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "budget-fixture", "choices": []any{map[string]any{"index": 0, "message": protocolProposal("ask_user", "fixture-call", questionInput{Question: "说明模型用途"}), "finish_reason": "tool_calls"}}})
			}))
			defer server.Close()
			ctx := context.Background()
			s := testService(t, t.TempDir(), &fakeProvider{}, false)
			defer s.Close()
			v := s.newRun("owner", "能力说明", version)
			if err := s.store.Create(ctx, v); err != nil {
				t.Fatal(err)
			}
			base, err := deepseek.NewChatModel(ctx, &deepseek.ChatModelConfig{APIKey: "fixture", BaseURL: server.URL, HTTPClient: server.Client(), Model: "deepseek-v4-pro", MaxTokens: 4096})
			if err != nil {
				t.Fatal(err)
			}
			m := &countedModel{BaseChatModel: base, s: s, id: v.ID}
			if _, err = m.Generate(ctx, []*schema.Message{schema.UserMessage("能力说明")}); err != nil {
				t.Fatal(err)
			}
			want := 4096
			if version == ConversationPromptVersion {
				want = 8192
			}
			if got := <-requests; got != want {
				t.Fatalf("SDK sent max_tokens=%d want=%d", got, want)
			}
		})
	}
}
