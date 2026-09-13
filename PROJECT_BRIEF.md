# Tripo Agent

## Goal

构建一个面向 3D 创作者及行业从业者、可公开演示的 3D 资产创作 Agent，通过网页向用户提供服务。

## Product Hypothesis

项目优先承担资产制作助手的角色：用户提供需求、素材或已有资产与目标，单个 Agent 理解用途和限制、规划制作流程、选择并调用已接入的 Tripo 制作能力、跟踪异步任务、依据产物反馈调整后续动作，并解释交付结果。用户获得可使用或继续加工的 3D 资产；网页承载交互、过程展示和结果交付。

长期交互围绕同一资产持续进行：一个制作目标完成后，用户可以在原会话中基于已有资产结果提出后续目标，继续与 Agent 协作。例如，在相应能力完成接入后，可先生成角色，再在原会话提出骨骼绑定和动画制作目标。该交互方向见 [ADR 0021](docs/adr/0021-continuous-asset-collaboration.md)，当前首版尚不支持交付后追加目标。

长期能力方向包括资产生成、资产处理、骨骼绑定和动画，按阶段逐步扩展。文字、图片或已有模型是长期可支持的任务输入方向；各项能力均以 Tripo API 实际开放的操作及输入条件为边界，进入哪个版本和如何验收在相应阶段单独确定。

Tripo API 是项目长期唯一的资产生产与加工能力来源。项目不接入其他 3D 生成服务，也不使用本地建模工具补齐 Tripo 未开放的生产操作；Agent 的模型服务、应用调度、下载、技术检查与网页展示仍由各自组件承担。Tripo 提供某项能力，不代表本项目已经接入并验证该能力。决策依据见 [ADR 0020](docs/adr/0020-tripo-only-asset-production.md)。

## Product Vision and Milestones

- 长期目标描述服务对象、Agent 的角色与能力演进方向，不以当前静态道具链路作为能力上限。
- 当前 MVP 以文本到静态资产的完整制作链路验证 Agent 的理解、规划、工具决策、异步执行、技术检查、有限纠偏及交付能力。
- 本次定位调整保留当前 MVP 的能力范围与原有验收标准。图片输入、已有资产导入、骨骼绑定、动画及同会话持续创作作为后续阶段方向，分别明确范围与验收，不追加为当前 MVP 的必需项；后续优先级尚未确定。
- 当前“一会话只执行一次资产请求、交付后结束执行”属于首版限制；长期会话承载同一资产的连续创作，不因一次目标完成而必然结束协作。
- 首条端到端演示是该阶段的具体案例；木箱用于提供实现与运行证据，不代表项目的最终版本，也不单独构成 MVP 全部验收通过。
- 当前实现与验证状态由 README 和验证记录说明；未来方向不计作已实现能力。阶段验收与约束见 CONTEXT.md 和相应 OpenSpec 规格。

## Required Capabilities

- Agent Runtime
- Context management
- Intent understanding
- Skills and prompt engineering
- Workflow orchestration
- Tool calling
- Async task handling
- Execution tracing
- Automated evaluation
- Minimal WebSocket progress delivery
- Minimal 3D result viewer

## Technology Constraints

- Backend: Go
- Agent framework: CloudWeGo Eino
- 3D asset production and processing: Tripo API only (long-term boundary)
- Architecture: Single Agent
- Frontend: Minimal implementation only

## Explicit Non-goals

- Multi-Agent
- General-purpose Agent platform
- Kubernetes
- GPU or inference infrastructure
- RAG
- Complex frontend
- Training or fine-tuning 3D models
- Building a replacement for Tripo Studio
- Integrating alternative 3D production providers or local modeling tools to extend beyond Tripo API capabilities

## MVP Success

以下为当前静态资产 MVP 的能力目标，具体评测门槛沿用 CONTEXT.md；后续阶段独立定义能力范围与验收条件。

A user can submit one asset request and see:

1. Parsed asset intent
2. Agent plan
3. Tripo tool calls
4. Asynchronous progress
5. Generated 3D result
6. Basic asset validation
7. Complete execution trace
8. Evaluation result
