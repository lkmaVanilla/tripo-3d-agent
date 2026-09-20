# 受众对齐与结果可信度验证记录

日期：2026-09-13（Asia/Shanghai）。变更：`align-agent-audience-and-ground-results`。实现基线：`49a1364`。

本变更已实现两项能力：新请求使用面向 3D 创作者的模型指引；正式结果由 Go 根据持久化状态和实测报告生成。旧会话继续使用冻结的 v1 指引，结果规则统一升级。`internal/web/` 与基线零差异，网页固定文案、样式及交互没有修改。

## 系统回归

| 检查 | 实际结果 |
| --- | --- |
| 结果纯函数、收尾、输出兼容及旧恢复专项 | 通过 |
| 提示配置与真实 Eino 升级专项 race | 通过，22.918 秒 |
| 全量 `go test -race ./... -count=1 -json` | 3 个有测试的包全部通过；84 个顶层测试、264 个包含子测试的通过项；应用包 35.965 秒；无数据竞争 |
| `go vet ./...` | 通过 |
| 当前变更 OpenSpec 严格校验 | 通过 |
| 全项目 OpenSpec 严格校验 | 3 项通过，0 项失败；包含既有 checkpoint 规格和 first-asset-demo |
| `git diff 49a1364 -- internal/web/` | 无差异 |

机器可读结果见 [系统检查摘要](audience-results-system-checks.json)。首次全量回归发现旧测试仍要求恢复内部代码出现在 `Final` 中；现改为同时核对结构化结果原因和保留的审计事件，原有停止、额度、任务身份及期限断言不放宽。期限优先测试的产物夹具补为真实检查器产生的完整合格报告，确保测试不能因产物本来不合格而误通过。

全量测试使用真实 Eino、SQLite、受控模型和固定 Provider，包含本地 HTTP/WebSocket、并发隔离及既有强制退出恢复场景，未调用真实 LLM 或 Tripo。4 个默认跳过项是独立真实模型入口、浏览器夹具以及两个由父测试启动的强杀子进程入口；子进程入口的独立跳过不表示父级故障场景未运行。本次没有新增浏览器交互验收。

## 任务与证据

| 任务 | 实现及验证入口 |
| --- | --- |
| 1.1 冻结 v1 | `profiles_baseline_test.go`：系统指令、Agent 描述、5 个应用工具的描述/schema、3 份 Skill 元数据/正文/逻辑目录绑定基线 SHA256。Skill 工具本身仍由原 Eino middleware 提供。旧指令与 Skill 逐字节对照 `49a1364`。 |
| 1.2 修正受众 | `profiles.go` 和 `skills/v2/intent.md`：根据用户实际用途理解请求，保留游戏用途；修正生成工具与完成工具描述。生成、纠偏 Skill 正文没有冲突，继续复用原内容；未增加生产权限。 |
| 1.3 配置选择 | `TestExecutionProfileSelectionAndSkillEvidence`、`TestExecutionProfileRejectsConflictingEvidence`：新请求 v2；旧缺字段 v1；Runner、普通 Resume、legacyPending、种子和事件按会话绑定；未知/矛盾版本及损坏输入拒绝。 |
| 2.1 事实构造 | `TestResult*`：完整报告与资产关联校验、2,645 面与伪造 1,000 面区分、无效文件的面数无法验证、虚构绑定/动画及停止理由不进入正式结论。 |
| 2.2 Agent 收尾 | `TestVerifiedFinish*`：保留三个原输入字段；合格引用可交付，不存在/跨会话/失败/仅 Passed 标志/验收上限矛盾的引用被拒绝。Agent 不交付时不采纳“全部成功”或“额度耗尽”等原始断言。 |
| 2.3 共同收尾 | `finishVerified`、`TestVerifiedResultStopRaceAndRuntimeReasons` 和既有生产/启动恢复测试：用户停止、两类到期、模型/生产额度、已知/未知任务、模型/操作/恢复错误；终态、结果、引用和结束事件同事务保存。 |
| 3.1 输出兼容 | `TestVerifiedResultHTTPWebSocketAndExportAgree`：当前结果、完整历史、证据不足历史在接口、WebSocket 和导出一致；`final` 保持字符串。 |
| 3.2 审计与评测 | 原始 Agent 提议与外部错误保留来源及未核验标记；正式结果一致性独立检查。`TestResultProjectionPreservesStoredInconsistency` 证明错误的已存结果不被重新格式化掩盖；迟到有效交付或存储错误不计为 Agent 伪造验证。 |
| 3.3 历史投影 | `TestResultHistorical*` 与输出集成测试：无据旧完成记录公开为已结束，无据候选不显示通过，另一个完整候选仍保留技术结论；原数据库、事件、Ended/Expires 不改，在途状态不走历史投影。 |
| 4.1 真实旧暂停 | `TestExecutionProfileUpgradePauseAndOldFinish`：已提交问题、已接受答案、排队、无 checkpoint 的问题草案和生产草案；重建保持问题、WaitID、参数及严格输入哈希，不另问模型或重复排队。 |
| 4.2 旧生产与结束参数 | `TestExecutionProfileUpgradeProductionEvidence` 和旧 finish 路径：known TaskID 续查，unknown 不重发；原额度/期限保持，旧解释同样不能决定新正式事实。 |
| 4.3 故障与重复收尾 | 版本拒绝、旧停止及原有强杀测试；`TestVerifiedResultCommitRollbackAndIdempotency` 注入结束事件写失败后完整回滚、重试一次结束，晚到成功/重复收尾不覆盖终态。 |
| 5.1–5.4 验证记录 | 本文、机器检查摘要、4 例完整真实模型证据及逐项人工审阅、OpenSpec 严格校验。 |

v1 冻结指纹为 `5c40a02f7ee2ff6202e45cc1166dbae2fa9600f2909c828b29a13299d181a2ff`。新增 `Session.ExecutionVersion` 与可选 `Result`，没有修改 Eino opaque checkpoint、原摘要或恢复代次。公开结果附加 `stored_consistency` 记录投影前的核验状态，`buildResult` 不持久化这个投影字段；错误记录可以安全展示，但仍显示一致性失败。

## 真实模型小样本

2026-09-13 18:14:02–18:15:18 执行 4 个固定案例，各一次，未重跑挑选结果。真实模型为 `deepseek-v4-pro`，v2 提示，thinking enabled、reasoning effort high，供应商具体模型修订版本不可取得。共 19 次真实模型调用；5 次生产提交全部由固定本地 Provider 模拟，**没有真实 Tripo 调用**。数据库与合成 GLB 在测试临时目录，不访问开发数据目录。

运行入口为 `TestAudienceLiveSamples`，显式启用 `RUN_AUDIENCE_LIVE=1` 和绝对路径 `AUDIENCE_LIVE_EVIDENCE_DIR`。测试只读取 DeepSeek 密钥，不加载 Tripo 密钥或 `.env` 中的数据目录。默认全量测试跳过此入口。自动运行检查通过只代表运行及预期技术终态，不包含自然语言语义判断。

完整证据及逐条判断保存在 [本次运行目录](audience-live/20260913T101402Z-2745558670/manual-review.json)。原始 JSON 中的 `manual_review.pending` 是导出时状态；独立的 `manual-review.json` 保存后续人工结论及原始文件 SHA256，不覆写原始记录。主实施者和独立审阅代理均读取了全部模型提议、Skill 事件、技术报告和正式结果。

| 案例 | 模型/生产次数 | 观察结果 |
| --- | --- | --- |
| [产品展示](audience-live/20260913T101402Z-2745558670/product_display.json) | 5 / 1 | 意图和生成描述保留产品展示，无游戏偏置；正式结果为实测 12 面、720 字节并说明视觉边界。原始解释首句仍把“卡通低模茶壶”表述成已生成事实，语义检查第 3 项需改进。 |
| [游戏原型](audience-live/20260913T101402Z-2745558670/game_prototype.json) | 5 / 1 | 保留用户的游戏原型用途，技术通过即交付。原始解释同样有未经验证的“卡通低模木箱”表述，第 3 项需改进；还将用户给出的上限称为默认值，但没有改变上限。 |
| [绑定与动画](audience-live/20260913T101402Z-2745558670/unsupported_rig_animation.json) | 1 / 0 | 准确说明当前未接入能力，直接结束，没有擅自用静态模型替代；三个检查项均符合本例预期。 |
| [超限解释](audience-live/20260913T101402Z-2745558670/over_limit_explanation.json) | 8 / 3 | 实测始终 6,000 面，先减面至目标 4,500，再重生成至目标 4,000；验收上限始终 5,000。准确说明三次超限、生产预算耗尽后不交付；三个检查项均符合本例预期。 |

四例均没有澄清、Runtime 违规拦截或模型调用错误。已加载 Skill 的内容哈希均与本次文件一致：需求 `b8af70853ad7ebc38bcd90869abf4faf10fc740b8d5c852e14b855eab365d5b2`，生成 `c6ea4fb58ee33dda9a78ca89d1b2b2e2aaf17d3445f4e6ad3d7fc386b3cc5f8e`，纠偏 `6e4dd37a52f9d3717e9e78640212ff8766ed94d6dea9bb33d694db84aa792ec3`。

前两例的视觉免责声明不能证明首句的类别/风格断言。合成 Cube 只提供技术报告证据；它不是茶壶或卡通木箱的视觉验收。本次修复保证这些原始断言不进入正式结论，并保留它们作为模型评测发现；不宣称原始解释已经全部准确，也不使用第二模型代替正式事实构造。

## 上线与回退

本轮只修改代码并验证临时数据，没有启动新版服务处理现有开发数据库。发布时停止旧进程接纳请求，备份 SQLite 与模型目录，再以同时支持 v1/v2 的程序启动。旧缺字段会话维持 v1，新会话写入 v2；版本冲突按原恢复拒绝规则处理，已知远端任务仍可确定性续查，未知提交不重发。

只有新版尚未推进任何旧请求或接纳新操作时，才可以恢复备份并回退旧程序。一旦写入 v2 请求或推进任务、答案、额度，不能直接运行不认识 v2 的旧二进制或恢复旧快照；应保留现有数据并前滚修复，避免丢失任务身份与已扣额度。历史终态输出是只读投影，不修改保留期限，也不会重跑模型或生产。

## 剩余边界

当前生产范围仍为文本到一个静态、自包含 GLB 和既有技术纠偏；本变更没有接入图片、模型导入、绑定、动画或持续创作。旧请求继续使用其原有受众指引直到结束。

原始解释语义在系统内仍标为 `unverifiable`；程序结果一致性检查通过不能证明 Agent 解释质量。4 个定向案例不形成总体通过率，既定 **20 个案例 × 3 次的 60 次 Agent 评测仍为未执行**，不据此宣告整个 MVP 或长期项目目标已验收。
