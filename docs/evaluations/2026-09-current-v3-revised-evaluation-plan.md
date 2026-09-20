# 当前 v3 工具协议修复后的完整复测预声明

执行状态：用户明确确认后，已完成唯一一批完整 84 例真实 DeepSeek 复测和全部语义审核，结论仍未通过。证据目录为 `conversation-evidence/20260913T153145Z-3276251327`，详见[第三批完整结果](2026-09-current-v3-revised-evaluation-results.md)。此前自动审批因缺少明确授权拒绝启动，期间没有创建第三批 manifest 或改走其他路径；本批是在用户确认后才启动。以下预声明及评分标准原样保留。

前两批完整结果原样保留：首批包含 v2 兼容基线，第二批是 4096 输出上限、双布尔收尾协议的当前 v3 验收。第二批未达到门槛。本批重新运行全部 20 个固定场景×3 和 8 类连续创作×3，共 84 次，不用局部诊断替代完整验收。

相对于第二批的实施修复：

- 只对 v3 将每次模型输出上限从 4096 提高到 8192。第二批有三份空响应的 `finish_reason=length` 与 `completion_tokens=4096` 实证；没有证据判断具体 reasoning token 比例。
- v3 的 `finish_request` 从 `answer`/`deliver` 双布尔改为 `outcome=answer|delivery|ended`。已消费生产后失败须 `ended`，旧 v1/v2 协议不变。
- v3 返回普通正文但没有暂停/收尾工具时，保留原始提议并按模型动作错误结束，不再误报为 checkpoint 恢复失败；该行为仍是评测失败，程序分类修复不提高成绩。
- 历史输入的 set_intent 在同一次调用内先持久化需求差异草案，再保存正式意图；草案不授权生产。未提供字段承接来源背景，明确新值优先，旧报告不变。v3 的运行状态增加 intent_draft，运行前按最终工作树生成 manifest。
- 明确目标动作冻结后不能重新提交另一动作；生成提示保留用户真实用途，不能默认加入游戏用途。预算按当前 Run 计算，同会话后续新目标仍可启动新 Run。

场景输入、固定 Provider 反馈、用户澄清答案、最多三轮澄清、三次生产预算以及判分门槛与第二批一致。评测自动检查同时识别旧布尔和新枚举；新增 JSON/布尔类型与枚举契约检查，使 Runtime 兜底不能掩盖工具参数错误。旧批原始自动结果不重新计算，旧批语义审核已将此类错误判为策略失败。

84 是案例运行次数；每例最多 20 次模型调用，因此理论最多 1680 次模型请求。上一批实际为 329 次模型调用。发送内容限定为已列出的固定场景、系统与 Skill 指引、模拟 Provider 反馈和临时会话状态，不包含用户真实资产文件。

每目标每维度一个适用综合项，所有原始 Agent 提议共同决定成绩。20 场景与 8 类连续能力分别统计意图与澄清、策略、结果解释；各维度至少 90%，关键错误为零。三次未知提交被 Runtime 立即停止，Agent 无最终解释机会，因此解释维度不适用；模型空响应造成应有说明缺失仍判失败。无依据确定宣称失败原因、排除参数或认定未验证用途符合性属于关键错误；未实测的参数效果保证、遗漏视觉未验边界属于解释失败。

运行前 manifest 记录新 profile/Skill 哈希、模型输出上限、工具协议和全部案例。只读取 DeepSeek 凭证，Provider、GLB 和业务数据均为隔离临时夹具，不访问真实 Tripo。保留全部结果，不挑选重跑，不从程序正式报告为 Agent 原始说明背书。

```sh
RUN_CONVERSATION_EVAL=1 CONVERSATION_EVAL_PROFILE=current-v3 \
CONVERSATION_EVAL_EVIDENCE_DIR=/Users/lk_ma/code/tripo-3d-agent/docs/evaluations/conversation-evidence \
GOCACHE=/private/tmp/tripo-agent-go-cache \
go test ./internal/app -run '^TestConversationAgentEvaluation$' -count=1 -parallel=4 -v -timeout=180m
```
