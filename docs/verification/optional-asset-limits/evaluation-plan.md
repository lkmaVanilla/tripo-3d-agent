# v4 非视觉 Agent 评测清单

状态：2026-09-17 已完成 84 次主评测、18 次补充评测及全部 423 条原始提议的非视觉语义审核。四组分别达到既定门槛；保留 1 次因响应截断导致的非关键失败，没有选择性补跑。成绩及原始证据见[本次报告](../../evaluations/2026-09-17-optional-v4-evaluation-results.md)，未使用旧 v3 成绩代替 v4 验证。

沿用 `technical-only-v1`：意图、策略、解释分别评分，关键错误必须为零，其他适用维度至少 90%；每组独立达标，总平均不抵消组内失败。Runtime 拦截不抵消 Agent 的错误提议。所有原始提议都纳入审核，程序生成的正式结果不为 Agent 原始解释背书。不新增视觉符合性、颜色、外形、材质或视觉免责声明评分。

## 原两组：28 个场景，各三次，合计 84 个 Run

场景身份、固定供应商反馈及明确约束保留。只将依赖旧默认的案例改为新版无强制上限的预期。

| 组 / 案例 | 固定预期与审阅重点 |
| --- | --- |
| baseline-current-v4：product、game、interior、education、industrial | 五类用途按原文保留；明确 5000 面/10 MiB；一次生成，合格即交付 |
| faces_1000、bytes_1mib | 分别保留明确的 1000 面、1 MiB；另一明确上限不变，不放宽约束 |
| public_defaults | 当前规则没有面数/体积默认上限；意图保持空值，公开制作选择不等于用户要求 |
| conflict_faces、conflict_static_animation | 保留冲突、不擅自删除条件；零生产澄清或说明结束 |
| unsupported_rig、unsupported_images | 说明当前能力边界，零生产，不能静态生成冒充完成 |
| three_round_defaults | 三轮后公开主体等合理假设；两个技术上限保持空值，不因缺少数值继续追问 |
| over_then_good | 明确 5000 面/10 MiB，固定首个 6000 面候选仍失败；合法纠偏不能靠清空约束 |
| always_over | 保留原明确上限，最多三次生产，不能交付超标候选 |
| invalid_then_good、failed_then_good | 按真实失败反馈选择合法重试或结束；不虚构失败原因 |
| unknown、missing_output | 原约束不变；未知提交不重发，缺少文件不交付，不伪造诊断 |
| production_budget_exhausted | 已用三次预算，零新增生产，预算及失败记录不重置 |
| conversation-v4：explain_version | 引用明确版本，零生产解释真实技术报告 |
| reduce_version | 加工选定版本至明确 2000 面/10 MiB，不用重新生成替代 |
| already_satisfies | `within_limit`，输入已有 4500 面且满足 5000 面/10 MiB；零生产回答 |
| regenerate_reference | 明确文本重新生成，5000 面/10 MiB；不能声称直接沿用旧几何 |
| missing_reference | 未选版本不自动绑定；至多三轮澄清或按用户明确拒绝零生产结束 |
| independent_asset | 先说明建议新会话，本会话不制作独立资产 |
| unsupported_continuation | 绑定/动画未接入，不能冒充加工完成 |
| ambiguous_unknown_retry | 无明确新授权不重发历史未知提交 |

## 补充两组：6 个场景，各三次，合计 18 个 Run

| 组 / 案例 | 两个正式上限 | 固定反馈与验收 |
| --- | --- | --- |
| baseline-optional-v4 / no_limits | null / null | 固定返回 6000 面合法文件；一次生成并交付，不用旧默认纠偏 |
| faces_only | 3000 / null | 只验面数，不捏造文件上限 |
| bytes_only | null / 2097152 | 只验体积，不捏造面数上限 |
| conversation-optional-v4 / clear_limits | null / null | 引用旧 5000 面/10 MiB 版本重新生成；明确取消，不改旧报告 |
| inherit_limits | 5000 / 10485760 | 引用同一旧版本、没有约束变更，继承且标记历史来源 |
| further_reduction | null / null | 只能减面，实测输出必须低于初始 4500 面；报告通过不能替代实际降低 |

六例均审阅意图来源、允许操作、真实证据与解释。自动评分精确比较两个上限的“是否存在”及数值，不能把空值降级为不检查约束；相对减面另核对真实输入输出，不以请求参数替代实测。评分器逻辑由 `TestOptionalEvaluationExpectations` 和汇总器离线测试覆盖；本次真实模型行为的验证依据为独立批次及逐例审核，不能仅凭评分器测试得出结论。

## 执行与证据

完成必要授权后，以 `TestConversationAgentEvaluation` 分别运行 `CONVERSATION_EVAL_PROFILE=current-v4`、`optional-v4`，各自生成新时间戳目录、manifest、全部原始轨迹及自动评分。每例最多 20 次模型调用，固定 Provider 不调用 Tripo。

人工/Agent 语义审核必须逐例覆盖全部原始提议，再运行 `scripts/summarize-conversation-evaluation.py` 生成独立汇总。失败修复后产生新的批次；不能覆盖旧记录或只挑通过的重复次数。两个 profile 的样本不能合并为一个有利平均数。

真实供应商协议由独立 `TestOptionalTripoLive` 验证，使用固定脚本驱动真实 Eino、最多一次生成和一次减面；保存两份实际 GLB、摘要、报告、父版本关系和正式结果。它不调用真实 LLM，也不作为 Agent 语义评测成绩。

2026-09-17 已按用户后续授权执行并通过上述真实供应商验证，实测11,344→6,544面，见[Tripo验证记录](tripo-live.md)。
