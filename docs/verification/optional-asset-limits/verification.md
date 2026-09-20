# 可选技术上限：实施与验证记录

变更：`make-asset-limits-optional`。基线提交 `80dd8dc`。本记录只陈述当前工作树结果，不代表已提交、发布或完成归档。

## 实施结果

新聊天采用 v4：面数、体积上限分别支持继承、设置和取消，正式数据以正整数或显式 `null` 保存，并记录来源；无适用来源时不补 5000 面或 10 MiB。目标面数仍为 500–20000 的制作参数，下载150 MiB与展开解析256 MiB保护独立保留。

输入重评、产物检查、正式结果、HTTP/WS、版本记录和导出使用同一可选约束表示。无上限时技术测量继续执行，相应上限比较为 `not_applicable`。进一步减面除了技术通过，还要求输出实测面数低于初始指定输入。历史执行、旧提示、旧工具协议和旧报告不改写。

## 本地测试与环境

macOS arm64，Go 1.26.1，Node 22.22.1。Go 验证设置 `GOPROXY=off GOSUMDB=off GOCACHE=/private/tmp/tripo-agent-go-cache`。以下本地测试使用临时数据库、脚本模型及合成 GLB，未启用真实模型/供应商开关；后续已授权的真实模型评测另列。需要监听的测试仅使用本机端口。用户现有服务和真实数据没有重启或迁移。

| 验证 | 命令 / 入口 | 证据 |
| --- | --- | --- |
| Go 全项目回归 | `go test ./... -count=1` | `local-checks.json` 和 `go-test.log` |
| Go 全项目竞态 | `go test -race ./... -count=1` | `local-checks.json` 和 `race.log` |
| 静态检查 | `go vet ./...` | `local-checks.json`，无诊断 |
| 网页状态与渲染单测 | `node --test internal/web/tests/workspace.test.mjs` | 13/13 |
| 原前端契约浏览器测试 | `NODE_PATH=/opt/homebrew/lib/node_modules node internal/web/tests/workspace.browser.mjs` | `browser-contract.json`，11项 |
| 真实 Go/Eino + 固定 Provider 浏览器测试 | 显式启动 `TestConversationBrowserHarness`，再运行 `workspace.go-browser.mjs` | `browser-integration.json`，12项；无页面脚本错误 |
| 非视觉评测汇总器 | `python3 scripts/test-summarize-conversation-evaluation.py` | 25/25，包括84例及独立18例的临时汇总夹具 |
| OpenSpec | `openspec validate make-asset-limits-optional --strict` | `spec-validation.log`，只证明规格格式与结构有效 |

浏览器验证包含真实页面的上限取消差异、原版本4500面报告保留、新版本2250面、两个空上限、`not_applicable`、刷新和WS重连。它不评判3D外观。测试服务完成后主动终止；对应常驻 harness 的退出是主动清理，不作为测试失败或运行中生产服务处理。

## 规格—测试—证据对照

以下为12项要求在4个能力增量中的覆盖。共53个场景并不等于53个独立测试函数；参数化、集成和现有回归共同覆盖。真实 Agent 非视觉评测及一次真实 Tripo 生成、一次相对减面验证均已完成。

| 能力 / 要求 | 主要本地验证 | 证据边界 |
| --- | --- | --- |
| optional-asset-constraints / 技术验收上限独立可选 | `TestOptionalConstraintModesAndPersistence`、`TestOptionalLimitsInspection`、`TestOptionalExactBoundaries`；真实模型补充组 | 空值、独立上限、整数合法性、等值边界；no_limits、faces_only、bytes_only 各三次通过 |
| 约束变更区分继承设置与取消 | `TestOptionalHistoricalInheritanceClearAndFreeze`、`TestOptionalInheritanceAndAbsoluteReduction`、`TestOptionalClearDoesNotSilentlyInheritTextLimits`；真实模型补充组 | 继承来源、明确取消、文本清单不自动补回、接受后冻结；clear_limits、inherit_limits 各三次全提议审核通过 |
| 生产参数不充当验收上限 | `TestOptionalTargetReportAndReduction`、`TestOptionalProductionSeedUsesSavedParameterRules`、`TestOptionalPauseRecovery` | 无上限8000目标、范围拒绝、100面要求不放宽、暂停后复用 |
| 系统资源保护独立于用户验收约束 | `TestDownloadResourceLimits`、`TestOptionalLimitsResourceAndEncoding`、`TestOptionalDownloadLimitExplanation` | 超限停止、无截断伪测量、正式结果标注系统保护 |
| 无绝对上限的减面仍需证明目标完成 | `TestOptionalConversationGenerationAndReduction`、`TestOptionalTargetReportAndReduction`、`TestOptionalInheritanceAndAbsoluteReduction` | 8000→4000、输出未降不交付、已满足绝对上限与继续降低分开 |
| 可选约束按非视觉行为独立评测 | `TestOptionalEvaluationExpectations`、汇总器25项离线测试；84+18真实模型评测及423条原始提议审核 | 四组分别通过零关键错误、各适用维度至少90%的门槛；保留一次空响应失败，见本次报告 |
| agent-audience / 受众扩大不扩大当前生产权限 | 现有输入绑定、授权隔离及连续创作回归；冻结Skill和v4四Skill加载；真实模型评测 | 未增加图片、导入、绑定、动画工具；未支持能力与同资产边界案例均零生产正确结束 |
| 提示升级保留在途会话的执行版本 | `TestExecutionProfileV1Frozen`、`TestConversationUpgradeV2FrozenConfiguration`、`TestOptionalV3FrozenConfiguration`、`TestOptionalProfileLoadsAllSkillsAndToolSchema`、v1/v2暂停恢复回归、v3加工崩溃恢复、`TestOptionalPauseRecovery`、`TestOptionalKnownTaskRecoveryPreservesLimits`、`TestOptionalUnknownSubmissionNeverResends` | 新旧混合版本、Skill哈希、预算/截止时间/操作身份不变；旧入口仍v2 |
| verified-results / 正式结果以实测证据为依据 | `TestOptionalRejectsContradictoryReports`、现有results测试、可选报告测试 | 无效文件不可交付、空约束伪造不通过一致性检查、历史报告保留 |
| Agent 决策与事实核验分离 | `TestOptionalTargetReportAndReduction`、现有原始提议及结果测试 | `report.passed`与`goal_satisfied`分离；原始解释不由程序结果代为认证 |
| 输出接口共享按执行核验的正式结果 | `TestOptionalHTTPWebSocketAndExport`、网页单测、Go浏览器集成 | 实时/补读/HTTP/版本/导出空值一致，历史未知不按空值解释 |
| continuous-asset-conversation / 后续执行承接有来源的同资产上下文 | `TestOptionalConversationGenerationAndReduction`、`TestOptionalHistoricalInheritanceClearAndFreeze`、现有输入绑定/跨会话隔离/澄清回归、Go浏览器集成 | 同会话合法输入、真实父关系、旧报告不覆盖、无上限继续制作 |

v1/v2/v3原始Skill和主指令的字节比对结果见 `frozen-profiles.json`。v3完整模型可见协议指纹独立采自原提交的 `git archive`，不是以新实现作为自己的基线。

## 本轮发现并修复

- v4 Skill 最初未加入嵌入目录：实际加载Skill的Eino暂停恢复测试暴露错误；修复后四Skill加载和恢复通过。
- HTTP/WS测试最初复用同一Go快照对象，省略字段保留旧值导致测试误判；改为每帧新对象，真实投影无此问题。
- 新增网页断线测试在刷新后尚未建立WS时触发断线：补充等待真实连接后再断线，保留第一次失败日志和复测记录。
- v4明确取消时，旧文字约束可能因缺失清单被自动继承：要求给出完整更新清单，保留其他硬约束，不做模糊关键词删除。

## 真实模型评测与剩余证据

2026-09-17 按用户授权完成任务6.3：84次主评测、18次补充评测及全部423条原始提议的非视觉语义审核，四组各自通过。基线意图/策略为59/60，解释为56/57；连续创作与补充两组各适用维度均100%，关键错误均0。一次三轮澄清案例的模型响应达到8192 token后截断，保留原失败，未通过重跑覆盖。主批 Go 测试因此退出1，按预先规定的统计门槛进行的 Agent 评测汇总仍为通过。详见[结果报告](../../evaluations/2026-09-17-optional-v4-evaluation-results.md)。

2026-09-17 用户继续授权后完成任务6.4：真实Tripo生成1次、引用该产物减面1次，实测11,344→6,544面（减少42.31%），两项上限始终为null。两份真实GLB、原报告不变、输入上传身份、父版本关系和正式交付均已验证，见[Tripo验证记录](tripo-live.md)。该步骤使用固定Eino决策，不新增真实LLM调用。

当前为24/24，既定变更验收已完成，尚未提交、归档或部署。`local-checks.json` 保留本地回归记录当时的 pending 状态，后续真实评测以本报告和两份真实验证记录为准。外部调用范围及授权见 `external-evaluation-scope.md`，升级与回退见 `upgrade.md`。
