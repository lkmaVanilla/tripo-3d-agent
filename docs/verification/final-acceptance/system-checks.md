# 最终系统检查

执行日期：2026-09-14（Asia/Shanghai）。结果：8 项检查全部通过，退出码均为 0。

本报告只记录当前工作树的系统测试与构建结果，不替代真实模型的逐例语义评测，也不单独代表全部产品验收完成。

## 执行范围与环境

- 在 `conversation_critical_retest_test.go` 完成并冻结后执行 Go 检查。未修改业务源码或测试文件。
- 全部命令采用 `GOPROXY=off GOSUMDB=off GOCACHE=/private/tmp/tripo-agent-go-cache`。执行环境移除了 `RUN_*`、浏览器测试与测试子进程的继承 opt-in，未启动真实 DeepSeek / Tripo 测试。
- 受控 Go 测试使用临时目录和临时回环端口；未启动 8080 服务。Go 检查使用已经授权的回环端口执行权限。
- `npm run build` 仅复制锁定的 `@google/model-viewer` 浏览器产物和许可证到已忽略的 `internal/web/static/vendor/`；构建前后 `git ls-files internal/web/static/vendor` 均为空，无 tracked 文件被构建改写。
- 服务二进制输出到 `/private/tmp/tripo-agent-final-server`，未启动该二进制。
- 4 项独立非 Go 检查并行执行；随后 4 项 Go 检查并行执行。下表耗时是各命令实际墙钟时间，包含等待共享编译资源的时间。

## 检查结果

| 检查 | 命令 | 退出码 | 耗时（秒） | 日志 |
|---|---|---:|---:|---|
| 网页状态与通信测试（12 项） | `node --test internal/web/tests/workspace.test.mjs` | 0 | 0.134 | [01-node-workspace.log](01-node-workspace.log) |
| 评测汇总脚本测试（17 项） | `python3 scripts/test-summarize-conversation-evaluation.py` | 0 | 0.671 | [02-python-summary.log](02-python-summary.log) |
| 查看器构建 | `npm run build` | 0 | 0.153 | [03-npm-build.log](03-npm-build.log) |
| OpenSpec 严格校验 | `openspec validate add-conversational-asset-workspace --strict` | 0 | 0.224 | [04-openspec-validation.log](04-openspec-validation.log) |
| Go 全量测试 | `go test ./...` | 0 | 41.858 | [05-go-test-all.log](05-go-test-all.log) |
| Go app / tripo race 检查 | `go test -race ./internal/app ./internal/tripo` | 0 | 65.066 | [06-go-test-race.log](06-go-test-race.log) |
| Go 静态检查 | `go vet ./...` | 0 | 1.951 | [07-go-vet.log](07-go-vet.log) |
| Go 服务构建 | `go build -o /private/tmp/tripo-agent-final-server ./cmd/server` | 0 | 2.488 | [08-go-build.log](08-go-build.log) |

所有日志记录执行命令、UTC 开始时间、完整标准输出/错误输出、退出码和耗时。Go 全量测试包含现有恢复、强杀、并发、版本、会话和供应商受控测试；需显式 opt-in 的真实模型、真实 Tripo 与浏览器验证不包含在本次运行中。

## 结果边界

Node 为 12/12，Python 为 17/17；Go 全量与 race 命令均成功结束，静态检查无输出错误。OpenSpec 规格校验通过只证明规格格式及结构有效，不替代实现行为测试。原有模型评测失败记录未修改。
