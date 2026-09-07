# Agent 负责策略决策，程序负责确定性执行

项目需要体现 Agent 根据上下文选择下一步行动的能力，同时可靠地处理异步任务、网络操作和产物检查。采用由单个 Agent 决定继续澄清、启动生成、调整策略或结束任务，程序执行确定性操作的职责划分。

| 概念 | 职责 |
|---|---|
| Agent Runtime | 驱动决策循环，管理等待用户、工具执行、预算和终止 |
| Context | 保存当前请求的对话、已确认意图、默认假设、计划和工具结果 |
| Intent | 结构化表达资产目标、用途及约束，作为生成和验收的依据 |
| Skill | 提供任务指引与策略，例如如何将资产需求转成生成参数 |
| Tool | 提供输入输出明确的操作，例如提交生成、查询任务、检查模型 |
| Workflow | 执行固定步骤，包括异步轮询和可安全重试的网络操作 |

例如，下载超时由程序有限重试；实测面数超标交给 Agent 决定如何纠偏或停止。Agent 可以调整生产策略，但不得修改检查结果或放宽验收上限来使产物通过检查。

这一划分使模型决策可以依据实际工具结果接受评测，同时避免让模型反复决定固定的轮询和传输重试步骤。运行时采用的 Eino 构件与 Go 业务执行层边界见 [ADR 0008](0008-single-go-service-with-eino-adk.md)；有限重试次数、退避与单次超时在实现阶段确定，并遵守既定预算和执行截止时间。提交结果未知时不自动重提，见 [ADR 0009](0009-recover-known-executions-without-resubmission.md)；可用纠偏操作已在 [ADR 0005](0005-decimation-and-regeneration-only.md) 中限定为减面和重新生成。

生产操作次数预算由 Runtime 强制执行，Agent 不得自行增加额度；计数与耗尽后的处理规则见 [ADR 0006](0006-three-production-submissions-per-request.md)。

当前请求的 Context 输入范围和三份只读 Skill 的职责见 [ADR 0011](0011-request-context-and-readonly-skills.md)。
