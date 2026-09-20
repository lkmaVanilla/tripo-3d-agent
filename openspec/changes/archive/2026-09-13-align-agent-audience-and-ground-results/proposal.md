## Why

项目已经确认面向 3D 创作者的资产制作助手定位，但 Agent 系统提示和需求 Skill 仍预设独立游戏开发者与游戏原型用途。当前交付只核对资产检查是否通过，模型的自由解释却直接保存为正式结果，可能出现报告正确而结论中的面数、已完成操作或停止原因错误。

## What Changes

- 对齐非网页的 Agent 身份、需求 Skill 及相关模型可见描述：理解创作者的实际用途，保留当前文本到静态 GLB 的 MVP 限制、Tripo-only 边界和既定预算。只修改有冲突的内容，保留合理的游戏案例。
- 保留 Agent 的交付或结束选择；正式结果中的事实统一由 Go 根据持久化状态、资产引用和实测报告生成。模型的原始解释继续留作决策与评测证据，不直接成为已核验结论。
- 统一成功、Agent 结束、用户停止、期限到期、预算收尾和已知任务恢复的结果依据，明确区分技术失败、无法验证、提交结果未知与本地停止。
- 保持现有 `session.final` 字符串与前端消费方式；普通接口、WebSocket、导出及单次核验使用同一后端结果依据。处理历史自由解释的证据边界，不将旧文字自动升级为可信事实。
- 新会话使用新提示/Skill 版本；在途旧会话继续使用匹配的内置旧版本，保持已有暂停、答案、预算和操作的恢复语义。
- 为上述行为增加受控回归及针对受众与解释的真实 LLM 小样本检查，分别记录执行证据，不将其代替既定完整 Agent 评测。

范围以 [PROJECT_BRIEF.md](../../../PROJECT_BRIEF.md)、[CONTEXT.md](../../../CONTEXT.md)、[ADR 0003](../../../docs/adr/0003-technical-validation-only.md)、[ADR 0004](../../../docs/adr/0004-agent-decisions-and-deterministic-execution.md)、[ADR 0017](../../../docs/adr/0017-execution-timeline-and-evidence-export.md) 和 [ADR 0020](../../../docs/adr/0020-tripo-only-asset-production.md) 为依据，不改变已确认产品共识。

**非目标：** 不修改 `internal/web/` 下的页面文案、标题、示例、布局、脚本或样式；后端生成的动态结果内容会因可信度修复而改变。不开启图片、绑定、动画、持续会话或新供应商；不增加视觉检查、第二个审查 Agent、通用事实核查平台或资产版本体系；不在本变更中完成整个 20 案例 × 3 次评测。

## Capabilities

### New Capabilities

- `agent-audience`: 非网页提示与 Skill 对齐服务对象、保持当前能力边界，并保证提示升级与在途会话恢复兼容。
- `verified-results`: 根据持久化证据生成正式结果，区分原始 Agent 解释、各类终态和历史证据，并保持现有输出接口兼容。

### Modified Capabilities

无。现行 `checkpoint-recovery` 的身份、原子提交、重放及停止优先要求保持不变；本次增加提示版本升级时的具体兼容要求。`asset-demo` 仍位于既有 `first-asset-demo` 变更中，其游戏木箱是首条案例，本次不重写或归档该变更，也不把该案例的受众当作整个 Agent 的长期限制。

## Impact

- 预计涉及 `internal/app/agent.go`、`internal/app/skills/`、`types.go`、`service.go`、`production.go`、`store.go`、恢复版本解析及 `http.go` 的结果投影/核验路径，以及相应后端测试和验证记录。
- 保留现有 `finish_request` 的 `deliver`、`artifact_id`、`explanation` 参数形状，避免给旧消息增加必填字段。会话持久化可增加可选、版本化的提示配置标识及终态依据；现有路由、状态枚举和 `session.final` 类型保持兼容。
- 现有前端无需修改，继续显示后端生成的动态结果。历史证据不足的结果采用保守输出，原始说明和状态保留审计来源，不重新生产或改变原保留期限。
- 不引入依赖或供应商接口变更。受控系统回归不调用付费服务；真实 LLM 小样本使用固定 Tripo 测试响应，单独记录模型调用证据。
