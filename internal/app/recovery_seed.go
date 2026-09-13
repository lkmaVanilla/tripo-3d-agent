package app

import (
	"encoding/json"
	"fmt"

	"github.com/cloudwego/eino/schema"
)

// recoveryFault 表示无法证明恢复安全的协议错误；重试不会修复缺失或矛盾的证据。
type recoveryFault struct{ code string }

func (e *recoveryFault) Error() string {
	return fmt.Sprintf("%s：无法安全恢复当前执行", e.code)
}
func recoveryError(code string) error { return &recoveryFault{code: code} }

// cloneMessages 通过公开 JSON 表示深拷贝消息，防止后续 Runner 修改共享消息污染重放种子。
func cloneMessages(in []*schema.Message) ([]*schema.Message, error) {
	b, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var out []*schema.Message
	err = json.Unmarshal(b, &out)
	return out, err
}

// validateProtocol 只校验公开消息协议，从不解析 Eino 内部的 gob 检查点。
// 每组 assistant 工具调用必须在下一条非工具消息前全部得到对应结果，且不能遗留悬空调用。
func validateProtocol(in []*schema.Message) error {
	pending := map[string]bool{}
	for _, msg := range in {
		if msg == nil {
			return recoveryError("recovery_seed_invalid")
		}
		if msg.Role == schema.Tool {
			if !pending[msg.ToolCallID] {
				return recoveryError("recovery_seed_invalid")
			}
			delete(pending, msg.ToolCallID)
			continue
		}
		if len(pending) != 0 {
			return recoveryError("recovery_seed_invalid")
		}
		for _, call := range msg.ToolCalls {
			if msg.Role != schema.Assistant || call.ID == "" || pending[call.ID] {
				return recoveryError("recovery_seed_invalid")
			}
			pending[call.ID] = true
		}
	}
	if len(pending) != 0 {
		return recoveryError("recovery_seed_invalid")
	}
	return nil
}

// newReplaySeed 只接收一个工具调用的完整提议，使恢复时能唯一确定要重建的暂停。
// 输入必须已闭合；最后的 Response 故意保留待执行调用，由恢复 Runner 再次进入该工具。
func newReplaySeed(input []*schema.Message, response *schema.Message, modelName string) (*ReplaySeed, error) {
	if err := validateProtocol(input); err != nil {
		return nil, err
	}
	if response == nil || response.Role != schema.Assistant || len(response.ToolCalls) != 1 {
		return nil, recoveryError("recovery_seed_invalid")
	}
	call := response.ToolCalls[0]
	if call.ID == "" || call.Function.Name == "" || !json.Valid([]byte(call.Function.Arguments)) {
		return nil, recoveryError("recovery_seed_invalid")
	}
	messages, err := cloneMessages(append(append([]*schema.Message(nil), input...), response))
	if err != nil {
		return nil, err
	}
	seed := &ReplaySeed{Version: recoveryVersion, DecisionID: newID(), ToolCallID: call.ID, ToolName: call.Function.Name, ArgumentsHash: tokenHash(call.Function.Arguments), Model: modelName, PromptVersion: PromptVersion, Input: messages[:len(messages)-1], Response: messages[len(messages)-1]}
	seed.InputHash = tokenHash(jsonString(seed.Input))
	seed.ResponseHash = tokenHash(jsonString(seed.Response))
	return seed, nil
}

// validateReplaySeed 验证版本、消息完整性以及调用身份与参数的一致性。
// 此处通过仅说明种子自身有效；它与当前问题、资产及生产约束的对应关系由 validatePending 校验。
func validateReplaySeed(seed *ReplaySeed) error {
	if seed == nil || seed.Version != recoveryVersion || seed.DecisionID == "" || seed.Model == "" || seed.PromptVersion != PromptVersion || seed.Response == nil {
		return recoveryError("recovery_seed_invalid")
	}
	if seed.InputHash != tokenHash(jsonString(seed.Input)) || seed.ResponseHash != tokenHash(jsonString(seed.Response)) {
		return recoveryError("recovery_seed_invalid")
	}
	if err := validateProtocol(seed.Input); err != nil {
		return err
	}
	if seed.Response.Role != schema.Assistant || len(seed.Response.ToolCalls) != 1 {
		return recoveryError("recovery_seed_invalid")
	}
	call := seed.Response.ToolCalls[0]
	if call.ID != seed.ToolCallID || call.ID == "" || call.Function.Name != seed.ToolName || call.Function.Name == "" || seed.ArgumentsHash != tokenHash(call.Function.Arguments) || !json.Valid([]byte(call.Function.Arguments)) {
		return recoveryError("recovery_seed_invalid")
	}
	return nil
}
