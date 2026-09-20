# 可选技术上限 v4：真实模型评测结果

后续状态：用户继续授权后，已完成[真实Tripo生成与相对减面验证](../verification/optional-asset-limits/tripo-live.md)，变更进度达到24/24。下文23/24及“Tripo尚未执行”保留为模型评测完成时的状态；这两个独立模型批次的原始成绩和用量不变。

2026-09-17（Asia/Shanghai），按用户授权完成 **84 次主评测 + 18 次补充评测**，审核全部 **423 条原始 Agent 提议**，包括正文、工具参数与空响应。四个场景组分别达到既定门槛：**非视觉关键错误为 0，意图、策略和解释各适用维度至少 90%**。任务6.3完成，变更进度为23/24；真实Tripo冒烟任务6.4尚未执行。

这不是“全部样本零错误”：主批保留1次模型响应截断失败，未选择性补跑，也未改变评分门槛。原始记录中的 `semantic_review.status=pending` 和 `suite_acceptance=pending_semantic_review` 是记录写入时的状态，保留不改写；最终成绩由各目录的独立 `semantic-review.json` 与 `summary.json` 给出。

## 分组结果

| 场景组 | Run数 | 自动检查通过 | 意图与澄清 | 策略选择 | 非视觉解释 | 关键错误 | 结论 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | --- |
| 基线 `baseline-current-v4` | 60 | 59/60 | 59/60（98.33%） | 59/60（98.33%） | 56/57（98.25%） | 0 | 通过 |
| 连续创作 `conversation-v4` | 24 | 24/24 | 24/24（100%） | 24/24（100%） | 24/24（100%） | 0 | 通过 |
| 可选上限补充 `baseline-optional-v4` | 9 | 9/9 | 9/9（100%） | 9/9（100%） | 9/9（100%） | 0 | 通过 |
| 连续约束补充 `conversation-optional-v4` | 9 | 9/9 | 9/9（100%） | 9/9（100%） | 9/9（100%） | 0 | 通过 |

主批与补充批独立汇总，没有用合并平均分抵消组内失败。三个 `unknown` 样本由 Runtime 在提交未知后直接结束，模型没有收到反馈并作最终解释的机会，解释维度按既有规则不适用；不计为通过。空响应造成的说明缺失不享受该豁免。

评分沿用 [technical-only-v1](2026-09-14-technical-only-evaluation-scope.md)，不评分外观、源模型视觉声明或视觉免责声明。审核检查全部原始提议，不用 Runtime 正式结果替模型说明背书，不用 Runtime 拦截抵消错误提议。审核由本任务的 Codex 对保存轨迹逐例完成，未调用额外外部评委模型；每条审核记录保存依据、原始提议序号及原始文件 SHA-256。

## 保留的一次失败

[three_round_defaults/2 原始记录](optional-v4-evidence/20260916T165451Z-898214840/baseline-current-v4-three_round_defaults-2.json)：完成两轮澄清后，第4次模型调用的 `seq 18` 响应为 `finish_reason=length`，`completion_tokens=8192`，正文长度0、工具调用数0。随后 `seq 19` 记录 `model_error`，没有生产或交付，也没有最终说明。

该样本的意图、策略、解释均记失败，关键错误0。响应元数据证明这次返回达到了配置的输出上限；无法据此断言模型内部为何没有及时形成可执行响应，也不能把它归因为 API 网络错误。没有上调预算、改动重试策略或用复测结果替换该样本。主批 `go test` 因此退出1，原始测试日志保留失败；评测汇总按照预先规定的90%门槛仍通过。两者含义不同。

## 可选约束行为

- 无上限案例三次均保存 `null/null`，固定供应商返回6000面合法GLB后一次交付，没有按旧5000面默认触发纠偏。生产目标分别为3000、8000、6000，未写成验收上限。
- 只设面数三次均为 `3000/null`；只设体积三次均为 `null/2097152`。未设置项只展示实测值，不生成隐藏上限。
- 明确取消三次均清空旧上限，来源为 `cleared`；历史版本报告仍保留5000面/10MiB及4500面实测值。
- 继承三次均保留5000面/10MiB，来源为 `legacy_inherited` 并指向原版本；没有冒充本轮新设默认。
- 进一步减面三次均只调用减面，取消绝对上限，实测分别从4500降至4000、3000、3000；新版本保留父关系，旧版本仍存在。减面目标是否完成依据实际下降，不只看 `report.passed`。

以上三维文件均为固定 Provider 的合成GLB，用于验证真实模型的决策和解释；不能视为真实Tripo能按目标生成或减面的证据。

## 执行与用量

模型 `deepseek-v4-pro`，high thinking，v4配置输出上限8192 token；每Run最多20次模型调用、3次生产预算、3轮澄清。主批和补充批顺序运行，各6并发。临时数据库、固定案例与合成GLB，没有读取真实用户会话或资产。

| 批次 | 模型调用 | 固定Provider提交 | 记录输入token | 记录输出token | Go包耗时 | 退出码 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 主批84次 | 336 | 68 | 2,146,693 | 149,593 | 307.442秒 | 1 |
| 补充18次 | 87 | 18 | 607,972 | 35,908 | 80.161秒 | 0 |
| 合计 | 423 | 86 | 2,754,665 | 185,501 | — | — |

输入token包含供应商记录的缓存token；总缓存输入781,696。此表为响应元数据用量，未估算账单金额。**真实Tripo调用为0**。两个批次的指令及Skill哈希一致，保存在各自manifest；执行期间未修改业务代码、提示或评分器。

## 证据与复核

- 主批：[manifest](optional-v4-evidence/20260916T165451Z-898214840/manifest.json)、[逐例审核](optional-v4-evidence/20260916T165451Z-898214840/semantic-review.json)、[汇总](optional-v4-evidence/20260916T165451Z-898214840/summary.json)、[执行审计](optional-v4-evidence/20260916T165451Z-898214840/run-observation.json)、[测试日志](optional-v4-evidence/20260916T165451Z-898214840/test.log)。
- 补充：[manifest](optional-v4-evidence/20260916T170058Z-3219405932/manifest.json)、[逐例审核](optional-v4-evidence/20260916T170058Z-3219405932/semantic-review.json)、[汇总](optional-v4-evidence/20260916T170058Z-3219405932/summary.json)、[执行审计](optional-v4-evidence/20260916T170058Z-3219405932/run-observation.json)、[测试日志](optional-v4-evidence/20260916T170058Z-3219405932/test.log)。

两个目录分别保留84和18份原始Run记录，全部原始文件哈希和已审提议序号已核对。执行审计记录完整命令与退出码；以下只读命令可重复计算并核对汇总，不发起新的模型调用：

```sh
python3 scripts/summarize-conversation-evaluation.py docs/evaluations/optional-v4-evidence/20260916T165451Z-898214840 --check-against docs/evaluations/optional-v4-evidence/20260916T165451Z-898214840/summary.json
python3 scripts/summarize-conversation-evaluation.py docs/evaluations/optional-v4-evidence/20260916T170058Z-3219405932 --check-against docs/evaluations/optional-v4-evidence/20260916T170058Z-3219405932/summary.json
```

本轮只补充执行证据、审核与验证状态，没有修改业务实现、提交commit或归档变更。剩余真实Tripo一次无上限生成及一次同会话相对减面仍须独立完成，不能由本评测或历史供应商证据替代。
