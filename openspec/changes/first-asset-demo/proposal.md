## Why

项目已有需求与架构共识，但仓库尚无可运行的业务实现。需要把木箱示例变成浏览器中可操作、可追踪、可验收的真实资产生产链路，证明 Agent 能根据需求和技术反馈选择行动。

## What Changes

- 新增匿名会话、澄清、结构化意图和计划，使用 Eino 单 Agent 与 DeepSeek V4 Pro。
- 新增 Tripo 文本生成和减面工具，由 Go 完成异步查询、下载、技术检查、预算控制和恢复。
- 新增最小页面：多会话、WebSocket 进度、GLB 预览下载、停止、技术报告、事件时间线和 JSON 导出。
- 新增单次执行规则评测、受控集成测试、启动配置及真实演示操作说明。
- 沿用 CONTEXT.md 与 ADR 0001–0018 的边界。非目标包括视觉检查、通用平台、多 Agent、复杂前端和动画能力。

## Capabilities

### New Capabilities

- `asset-demo`: 从匿名资产请求到可检查、可预览、可追踪结果的首条完整链路。

### Modified Capabilities

无。

## Impact

新增 Go 服务、Eino/模型适配器、SQLite 存储、Tripo 客户端、静态页面及测试。真实演示需要本地提供 DeepSeek 与 Tripo 凭证，并消耗外部服务额度。固定测试响应必须标明来源，不得作为真实 Tripo 演示证据；60 次案例集验收独立于本次单条真实链路验证。
