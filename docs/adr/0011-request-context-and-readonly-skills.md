# 使用当前请求的结构化 Context 与只读 Skill

适用范围：以下记录当前静态资产 MVP 的 Context 与 Skill 决策。长期同一资产会话的上下文延续由 [ADR 0021](0021-continuous-asset-collaboration.md) 补充定义，当前实现保持不变。

Agent 需要与当前资产请求一致的决策依据，以及生成和纠偏所需的任务指引。Context 每轮提供原始需求、已确认意图、默认假设、计划、剩余预算、关键工具结果和必要对话；完整执行日志保存到 SQLite，硬约束和预算从结构化状态读取，不依赖模型回忆或摘要重建。

一个会话只承载一个资产请求，其澄清、首次生成和纠偏持续使用该请求的 Context；制作另一个资产时新建会话，不自动继承其他请求的上下文，见 [ADR 0014](0014-anonymous-sessions-and-concurrent-access.md)。

首版 Skill 限定为三份随仓库版本管理的只读任务指引：

| Skill | 内容 |
|---|---|
| 需求理解与澄清 | 提取用途、识别约束和冲突、明确默认假设 |
| Tripo 生成 | 将意图转成生成描述及适用参数 |
| 技术纠偏 | 根据检查结果、工具条件和预算选择减面、重新生成或停止 |

使用 Eino 的 Skill 按需加载机制，在当前 Agent 内执行，保持单 Agent 架构。首版范围限定在当前资产请求，不加入跨请求长期记忆或动态安装 Skill 的能力。

这一选择使任务指引可以独立维护和随版本评测，同时把每次决策必需的事实与完整执行记录区分开。Skill 加载不能改变业务层对预算、约束和工具适用条件的强制校验。

依据：[Eino 官方 Skill 文档](https://www.cloudwego.io/docs/eino/core_modules/eino_adk/eino_adk_chatmodelagentmiddleware/middleware_skill/)。
