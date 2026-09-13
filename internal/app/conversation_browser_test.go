package app

import (
	"context"
	"encoding/json"
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// 独立临时数据、合成GLB和确定性Agent，仅供真实网页连接Go服务验收。
func TestConversationBrowserHarness(t *testing.T) {
	if os.Getenv("CONVERSATION_BROWSER_TEST") != "1" {
		t.Skip("opt-in controlled conversation browser fixture")
	}
	s := newConversationTestService(t, &conversationProvider{delay: 3 * time.Second})
	s.modelFactory = func(context.Context) (model.BaseChatModel, error) { return conversationBrowserModel{}, nil }
	listener, e := net.Listen("tcp", "127.0.0.1:48090")
	if e != nil {
		t.Fatal(e)
	}
	server := httptest.NewUnstartedServer(s.Handler())
	server.Listener = listener
	server.Start()
	defer server.Close()
	t.Log("CONTROLLED CONVERSATION FIXTURE ONLY: " + server.URL)
	select {
	case <-time.After(30 * time.Minute):
	case <-s.ctx.Done():
	}
}

type conversationBrowserModel struct{ conversationScript }

func (m conversationBrowserModel) Generate(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	var state struct {
		Request        string `json:"request"`
		Clarifications int    `json:"clarifications"`
	}
	for _, msg := range in {
		if i := strings.LastIndex(msg.Content, "<runtime_state>"); i >= 0 {
			_ = json.Unmarshal([]byte(strings.Split(msg.Content[i+15:], "</runtime_state>")[0]), &state)
		}
	}
	if strings.Contains(state.Request, "澄清") && state.Clarifications == 0 {
		return protocolProposal("ask_user", newID(), questionInput{Question: "请确认本次模型的用途和风格。"}), nil
	}
	return m.conversationScript.Generate(ctx, in, opts...)
}
