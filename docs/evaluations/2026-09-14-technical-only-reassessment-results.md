# 第五批完整轨迹的非视觉重新审核

2026-09-14，用户在取消视觉相关评分后，明确要求“按新规则重新审核”。本次对第五批全部 **84 个目标、344 条原始 Agent 提议**重新审核，采用 `technical-only-v1`。两组独立达到既定门槛，整体结果为 **passed**。

这是对已执行证据的离线重新审核，不是第六批真实模型运行。本次新增模型调用、固定 Provider 提交、真实 Tripo 调用均为 **0**。没有修改 Agent 运行代码、Skill、模型输出、自动检查结果或旧评分。

## 新成绩

| 场景组 | 意图与澄清 | 策略选择 | 非视觉解释 | 关键错误 | 结论 |
|---|---:|---:|---:|---:|---|
| 基线 60 例 | 60/60，100% | 59/60，98.33% | 55/58，94.83% | 0 | 通过 |
| 连续创作 24 例 | 24/24，100% | 24/24，100% | 22/24，91.67% | 0 | 通过 |
| 总计 84 例 | 84/84，100% | 83/84，98.81% | 77/82，93.90% | 0 | 通过 |

门槛仍为两组各自关键错误零、每个适用维度至少 90%，没有降低百分比或合并两组掩盖失败。`unknown/1`、`unknown/3` 在提交未知后由 Runtime 直接结束，模型没有最终解释机会，且此前没有非视觉解释错误，因此解释为不适用；未计为通过。`unknown/2` 此前已有解释错误，仍计入失败分母。

**达到验收门槛不等于 84 例全部通过。** 78 例的全部适用维度通过，6 例各保留一个非关键失败。原始自动检查仍是 83/84，原执行退出码仍为 1；它们没有被重写。夹具预期的生产失败或未知提交，也不能仅凭运行状态 `failed` 算作 Agent 决策失败。

## 与旧成绩的两处变化

完整重读后，仅以下两个判分发生变化，其余维度判分与关键错误记录不变：

| 案例 | 原始位置 | 本次处理 |
|---|---|---|
| `baseline-current-v3-product-1` | seq20 | 原先对成品部件、材质和外观断言扣分，属于明确排除的视觉项；技术数据、交付和意图通过 |
| `conversation-v3-regenerate_reference-3` | seq17 | 原先对旧茶壶具体外形的归因扣分，属于明确排除项；引用授权、文本重生成、版本来源与技术报告通过 |

不要求输出视觉免责声明，也不通过“解释真实性”等名称恢复这些已排除评分。用户原始需求与默认选择的文字来源仍需核对。

## 保留的六项非关键失败

| 案例 | 维度与位置 | 仍存在的问题 |
|---|---|---|
| `baseline-current-v3-conflict_static_animation-3` | 策略，seq6 | 零生产的能力拒绝选择了 `ended`，应为 `answer` |
| `baseline-current-v3-interior-3` | 解释，seq20 | 把 Agent 加入的默认配色说成用户要求；核对的是文字来源，不是产物配色 |
| `baseline-current-v3-invalid_then_good-3` | 解释，seq6/12 | 目标和上限同为 5000 面，却声称预留了余量 |
| `baseline-current-v3-unknown-2` | 解释，seq6 | 把减少贴图细节说成能够保证低面数和体积，缺少技术依据 |
| `conversation-v3-independent_asset-1` | 解释，seq7 | 用户只更换资产，回答却断言用途、风格和硬约束全部改变 |
| `conversation-v3-unsupported_continuation-1` | 解释，seq7 | 把本应用未接入绑定/动画，扩大成 Tripo 整体只能生成静态模型 |

这些问题没有被修复、删除或改记不适用。沿用既定严重程度校准，它们不涉及超预算提议、放宽硬约束或伪造技术实测结论，属于 90% 门槛允许保留的非关键失败；后续可针对性改进，不把当前验收改成全案例零错误标准。

## 证据保存与复现

- [新 manifest](conversation-reviews/2026-09-14-technical-only-fifth/manifest.json)：独立声明评分版本、排除项、来源与 84 份原始文件摘要。
- [逐例语义审核](conversation-reviews/2026-09-14-technical-only-fifth/semantic-review.json)：每例包含三维判分、全部已审提议序号、技术报告和判分依据。
- [新完整汇总](conversation-reviews/2026-09-14-technical-only-fifth/summary.json)：由离线汇总器生成，可复现全部计数和逐例结果。
- [审核审计](conversation-reviews/2026-09-14-technical-only-fifth/run-observation.json)：记录原目录全部 94 个文件摘要、源码适用性与零新增调用。
- [原始第五批报告](2026-09-14-fifth-v3-results.md)和原目录所有文件保持不变；其中的失败结论仍属于旧评分版本。

新目录包含原来 84 份轨迹的逐字节副本，摘要全部相同。复制记录内的旧案例说明描述当时执行背景，不能当成当前评分规则；新 manifest 顶层和独立语义审核共同指定有效的 `technical-only-v1`。不往旧 manifest 混入新评分，也不修改原响应或其中的视觉文字。

第五批原执行使用真实 DeepSeek V4 Pro 和固定 Provider，包含 344 次模型调用、69 次固定 Provider 提交，耗时 341.437 秒。本次复用了这些历史证据，没有新增调用。对照 75 个冻结文件，只有 6 个测试文件因删除视觉评分而变化，运行代码、Skill 和执行约束没有变化；没有从其他批次挑选更好的结果替换。

复现命令：

```sh
python3 scripts/summarize-conversation-evaluation.py \
  docs/evaluations/conversation-reviews/2026-09-14-technical-only-fifth \
  --check-against docs/evaluations/conversation-reviews/2026-09-14-technical-only-fifth/summary.json
```

本次检查新旧完整汇总均可等值复现，84 份原始副本及原目录 94 个文件摘要保持不变；程序比较确认只有上述两个判分改变。汇总器 23 项测试通过（0.845 秒）；文档更新后 OpenSpec 严格校验通过，CLI 返回 `all_done`、47/47，相关文档链接与差异格式检查通过。此前删除视觉项后的 Go 9 个顶层针对性测试通过（2.464 秒），3 个真实模型入口明确跳过，保存的[日志](conversation-reviews/2026-09-14-technical-only-fifth/prior-nonvisual-go-test.log)供核对；这不是本次重跑 Go 全量的结果。

## 阶段验收

`10.6` 已完成：经用户授权，复用运行行为未变的完整第五批真实模型轨迹，按新范围逐例独立重新审核并达到门槛。

`11.2` 已完成：合并本次 Agent 评测、[系统回归 8/8](../verification/input-binding-acceptance/system-checks.md)、删除视觉项后的本地回归、[浏览器证据](../verification/conversation-browser/README.md)、[真实 Tripo 链路](../verification/conversation-tripo/README.md)及[升级演练](../conversation-upgrade.md)。没有因本次只更新文档和审核结果而重复请求供应商。应用测试仍要求全部通过；Agent 的六项非关键失败按原门槛保留并公开，未宣称已修复。

`add-conversational-asset-workspace` 任务进度更新为 **47/47**。这表示当前变更按约定范围完成验收，不表示长期 3D 创作能力全部完成，也不表示已经提交、归档或部署。图片输入、绑定和动画仍未接入。
