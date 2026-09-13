# 初始历史输入选择边界修复

第四批发现两次 `set_intent(decimate)` 把会话中可见的历史ID当作用户已经选定的输入。程序原门禁正确阻止生产，之后才询问用户，Agent 的输入识别与策略仍失败。另一次空响应有SDK截断证据，未将其推断为同一原因。

修复保留原权限和状态机，仅改善模型收到的信息及决策顺序：

- `conversationModelView` 增加由持久化 `InputVersion` 派生的 `input_binding`，状态明确为 `not_selected` 或 `selected`。不从历史、预览、版本数量或模型文本推断选择，不另存一份状态。
- 历史报告仍可用于解释；选中身份与文件可加工、技术合格分别核验，不能互相替代。
- 需要加工初始历史输入时，先通过 `ask_user` 取得明确选择，再 `set_intent`；拒绝选择时零生产 `answer`。系统指令、工具说明、意图和加工 Skill 同步这一顺序。
- 新建文本生成不需要历史输入。本Run已生成候选的合法减面纠偏继续直接使用该候选，不要求重新选择或重置固定目标。
- 不增加响应上限，不追加空响应重试，不改原始提议评分。旧profile、公开HTTP/WS、持久化表结构和实际生产门禁保持原协议。

本地验证与真实模型验证分别记录。新系统回归见 [input-binding-acceptance/system-checks.md](input-binding-acceptance/system-checks.md)，真实模型范围见[预声明](../evaluations/2026-09-14-input-binding-evaluation-plan.md)。历史第四批未通过记录保留，只有新批实际达到原门槛才能改变验收状态。
