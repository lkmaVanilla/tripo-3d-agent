# 外部验证数据范围

计划依据：用户在审阅 make-asset-limits-optional 提案后明确调用 openspec-apply-change；任务 6.3 要求新版 84 次真实 LLM 固定供应商评测及补充 6 例各三次，6.4 要求一次真实生成及一次减面。以下说明外部数据范围，实际完成状态见后文。

DeepSeek 评测仅发送仓库内的 Agent 指令/Skill、固定案例文本（茶壶、木箱、椅子、火山、台灯等）、随机测试身份以及固定 Provider 合成的任务/技术报告。每例 DefaultConfig 的 DataDir 强制为 t.TempDir，不读取真实 data 目录、真实用户会话或资产；历史模型由 testfixture.Cube 合成。没有其他用户数据进入 payload。

凭证读取只提取 DEEPSEEK_API_KEY 用于 DeepSeek 官方客户端认证，不加载 .env 其余配置；TripoKey 为空，Provider 被固定本地实现替换，不调用 Tripo。代码：internal/app/audience_live_test.go 的 audienceDeepSeekKey，internal/app/conversation_eval_test.go 的 runConversationEvaluation/seedConversationEvaluation。

84 + 18 表示两个独立批次的目标执行数，每个目标受原有 20 次模型调用、3 次生产提议预算控制。6 并发，主批超时 50 分钟，补充批超时 20 分钟。固定 Provider 不产生真实 Tripo 制作费用；真实 DeepSeek 调用消耗账户额度。日志由 s.sanitize 处理后保存在新目录，原评测证据不改写。

真实 Tripo 验证另行执行，使用固定木箱文本和本测试刚生成的 GLB 做一次减面，临时数据库，不上传项目已有资产或用户模型；不把 API 成功当作检查通过。

## 授权与执行状态

此前自动审批要求明确授权外部数据发送和额度消耗。2026-09-17，用户明确授权“给你授权，把真实模型评测跑完”，已完成 84 次主评测与 18 次补充评测，以及全部 423 条原始提议的审核。实际调用 DeepSeek 423 次；两个独立汇总均通过，保留 1 次响应截断失败，见[结果报告](../../evaluations/2026-09-17-optional-v4-evaluation-results.md)。此次新增授权只执行真实模型评测，真实 Tripo 调用为 0，生成与减面冒烟仍未运行。

随后用户明确要求“执行呀”，继续授权完成剩余 Tripo 冒烟。`TestOptionalTripoLive` 已执行并通过：真实生成1次、减面1次、内部上传1次，实测11,344→6,544面，保留两份实际GLB及父版本关系；本步骤真实LLM调用0次。详见[Tripo验证记录](tripo-live.md)，当前24/24。

Tripo 冒烟入口是 `TestOptionalTripoLive`，使用 `RUN_OPTIONAL_TRIPO=1` 显式开启。每个 Run 最多一次生产、最多15分钟观察；第二步仅上传本次生成的固定木箱 GLB，输出实测未减少也照实记失败，不追加第三次生产。原始 GLB 应先保存在独立的本地证据目录（例如 `/private/tmp`），审核后只把必要的脱敏索引和报告纳入仓库，避免误提交临时签名链接或大型二进制。
