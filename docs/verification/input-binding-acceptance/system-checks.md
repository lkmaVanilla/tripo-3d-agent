# 输入绑定修复后的系统检查

执行日期：2026-09-14（Asia/Shanghai）。结果：8 项检查全部通过，退出码均为 0。

本报告记录输入版本绑定修复后当前工作树的系统测试与构建结果，不替代真实模型的逐例语义评测，也不单独代表全部产品验收完成。此前未通过的模型评测和系统检查证据均保留。

## 执行范围与环境

- 4 项独立非 Go 检查先执行；收到 `conversation_input_binding_test.go` 与相关源码已冻结的确认后，再执行 4 项 Go 检查。未修改业务源码、测试或评分标准。
- 全部命令采用 `GOPROXY=off GOSUMDB=off GOCACHE=/private/tmp/tripo-agent-go-cache`。执行环境移除了 `RUN_*`、`TRIPO_TEST_*`、浏览器与测试子进程继承开关，以及 `DEEPSEEK_API_KEY` / `TRIPO_API_KEY`，未启用真实模型或真实 Tripo 测试。
- 受控 Go 测试使用临时目录和回环端口，使用已授权的本地测试执行权限；未启动或操作 8080 服务。
- `npm run build` 仅准备锁定的 `@google/model-viewer` 浏览器产物及许可证；构建前后 `git ls-files internal/web/static/vendor` 均为空，没有被构建改写的已跟踪 vendor 文件。
- 服务二进制仅写入 `/private/tmp/tripo-agent-input-binding-server`，未启动该二进制。
- 每组 4 项检查并行执行，下表耗时为各命令墙钟时间。Go app 测试因本次源码修改重新执行；未变化的 asset / tripo 包使用 Go 测试缓存，日志如实保留缓存标记。

## 检查结果

| 检查 | 命令 | 退出码 | 耗时（秒） | 日志 |
|---|---|---:|---:|---|
| 网页状态与通信测试（12 项） | `node --test internal/web/tests/workspace.test.mjs` | 0 | 0.136 | [01-node-workspace.log](01-node-workspace.log) |
| 评测汇总脚本测试（17 项） | `python3 scripts/test-summarize-conversation-evaluation.py` | 0 | 0.684 | [02-python-summary.log](02-python-summary.log) |
| 查看器构建 | `npm run build` | 0 | 0.187 | [03-npm-build.log](03-npm-build.log) |
| OpenSpec 严格校验 | `openspec validate add-conversational-asset-workspace --strict` | 0 | 0.226 | [04-openspec-validation.log](04-openspec-validation.log) |
| Go 全量测试 | `go test ./...` | 0 | 41.430 | [05-go-test-all.log](05-go-test-all.log) |
| Go app / tripo race 检查 | `go test -race ./internal/app ./internal/tripo` | 0 | 64.747 | [06-go-test-race.log](06-go-test-race.log) |
| Go 静态检查 | `go vet ./...` | 0 | 0.400 | [07-go-vet.log](07-go-vet.log) |
| Go 服务构建 | `go build -o /private/tmp/tripo-agent-input-binding-server ./cmd/server` | 0 | 0.760 | [08-go-build.log](08-go-build.log) |

日志包含执行命令、UTC 开始时间、标准输出及错误输出、退出码和耗时。Go 全量测试的 app 包执行 39.946 秒，race 下执行 60.449 秒。新输入绑定测试覆盖明确接受的版本选择、澄清后在同一 Run 恢复，以及当前 Run 候选的合法纠偏；现有恢复、强杀、并发、版本、会话和受控供应商测试随全量 app 一起执行。

## 结果边界

Node 为 12/12，Python 为 17/17；Go 全量、app / tripo race、静态检查及构建均成功。需要显式 opt-in 的真实模型、真实 Tripo 与真实浏览器验证不包含在本次运行中。OpenSpec 校验仅证明规格格式和结构有效；本地测试通过不能代替独立模型评测，未据此修改任务 10.6 或 11.2 的完成状态。
