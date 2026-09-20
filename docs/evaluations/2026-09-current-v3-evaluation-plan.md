# 当前发布 v3 完整评测预声明

此批用于当前发布入口 v3 的正式验收。上一批 60 次 v2 兼容模型基线与 24 次初期 v3 结果独立保留；当前批重新完整运行 20 个固定场景×3及8类连续创作×3，共84次，不仅重跑失败项。

沿用[原预声明](2026-09-conversation-evaluation-plan.md)的场景、用户要求、固定 Provider 反馈、预算和门槛，仅作以下明确协议修订：

1. 原20场景改用 `CreateAssetConversation`，固定为当前发布 profile `asset-agent-v3`。
2. 无生产的冲突、未接入能力场景采用 v3 的纯回答结局；已生产或已消耗预算的场景仍按真实结果结束。
3. 缺失引用最多澄清三轮；用户明确不选择后允许提前零生产解释结束，不强制问满。非关键主体/用途/风格持续不明确的场景仍验证三轮后公开默认。
4. 新批保留每次响应的 `FinishReason/Usage`、公开正文长度和工具参数长度，不导出思考正文。单纯空输出不推测 token 耗尽原因。

当前 v3 指引已明确：未知或停止任务不能通过本应用取回/导入；供应商错误不足以确认失败因果，不能无证据地排除参数或保证重试效果；未确定模型引用不得默认最新或唯一版本。对应指引和 Skill 的哈希由运行前 `manifest.json` 保存。模型依然是 `deepseek-v4-pro`，high reasoning、4096 MaxTokens，不因猜测修改模型预算。

每个受评目标每个维度一个综合项，所有原始提议、正文及参数纳入审核。20场景、8类连续场景分别统计，也给出整体供观察；不允许整体平均掩盖连续能力不足。擅自放宽硬约束、超预算提议、伪造验证结论为关键错误；其中把未经诊断的失败原因或参数排除当作确定事实也计关键错误。意图与澄清、策略选择、结果解释分别至少90%，关键错误必须为零。

```sh
RUN_CONVERSATION_EVAL=1 CONVERSATION_EVAL_PROFILE=current-v3 \
CONVERSATION_EVAL_EVIDENCE_DIR=/Users/lk_ma/code/tripo-3d-agent/docs/evaluations/conversation-evidence \
GOCACHE=/private/tmp/tripo-agent-go-cache \
go test ./internal/app -run '^TestConversationAgentEvaluation$' -count=1 -parallel=4 -v -timeout=180m
```

只读取 DeepSeek 凭证，所有生产仍是固定 Provider、临时目录和合成 GLB，不调用真实 Tripo。先写完整 manifest，再开始请求；保留本批全部结果，不修改旧批次成绩。
