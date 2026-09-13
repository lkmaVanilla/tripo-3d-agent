# 初始历史输入选择：三次定向复测

执行日期：2026-09-14（Asia/Shanghai）。在 [input_binding 修复](../verification/2026-09-14-input-binding-fix.md)后，对原 `missing_reference` 场景独立运行三次，真实 DeepSeek 加固定 Provider。三次自动检查与意图、策略、解释审核均通过，关键错误为零。

三次均在 seq7 先澄清输入版本。用户明确不选择且不授权默认后，seq13 以 `answer` 零生产结束；没有把唯一可见历史版本当作用户选择，没有先调用被 Runtime 拦截的 `set_intent`。历史版本 ID 和面数可以用于请求确认，但未形成生产授权。

该诊断保持原场景逐字段一致，每次原始提议均保留、独立审核；不以测试放宽预期。每个目标两次模型请求，共 6 次模型调用、0 次生产，真实 Tripo 为零。记录 27903 prompt token、2106 completion token；测试耗时 22.03 秒，Go 包耗时 24.199 秒。

证据：[清单](critical-case-evidence/20260913T170424Z-1996348360/manifest.json)、[审核](critical-case-evidence/20260913T170424Z-1996348360/semantic-review.json)、[独立底稿](critical-case-evidence/20260913T170424Z-1996348360/semantic-independent-review.json)、[汇总](critical-case-evidence/20260913T170424Z-1996348360/summary.json)、[运行观察](critical-case-evidence/20260913T170424Z-1996348360/run-observation.json)、[日志](critical-case-evidence/20260913T170424Z-1996348360/test-output.log)。

这是局部诊断，不能替代完整验收。随后独立运行的[第五批完整 84 例](2026-09-14-fifth-v3-results.md)中，本场景三次同样通过，但其他解释问题使整体仍未通过。
