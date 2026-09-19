# 国内站接入与诊断变更验证

日期：2026-09-19。变更：`fix-tripo-china-access-and-diagnostics`。源码基线 `85164c621ea261c6265cadf31cee9f6d4082d5fb`，候选包含未提交修改，`vcs.modified=true`。本记录随真实验证追加，不以本地测试代替上线验收。

## 代码回归

| 检查 | 结果 |
| --- | --- |
| `go test ./...` | 通过；app 65.309s，此后新增的 HTTP/WS、上传和下载穷尽测试分别局部通过，并纳入最终全量 race |
| `go test -race ./... -count=1` | 最终源码通过；app 89.934s，Tripo 3.483s，check-tripo 3.185s，无数据竞争报告 |
| `go vet ./...` | 通过 |
| `node --test internal/web/tests/workspace.test.mjs` | 15/15 通过 |
| `npm run build` | 通过，查看器嵌入构建就绪 |
| Linux amd64、CGO=0 server/check-tripo 构建 | 通过，保留 Go VCS revision/dirty 标记 |
| `git diff --check` | 通过 |

全量测试使用受控 Provider/模型，不请求真实 DeepSeek 或 Tripo。覆盖可选约束、连续会话及版本、匿名隔离、checkpoint、输入准备恢复、强制进程退出和既有迟到 TaskID 行为。

新增故障测试覆盖 DNS、连接复位／拒绝、TLS、超时、取消、401、业务拒绝、5xx、非 JSON、缺 TaskID、超长／嵌套恶意响应、上传与下载阶段、远端任务错误码。301/302/303/307/308 的真实接收端计数与复用连接首次写入失败，均验证生产请求不重放。

事务故障注入验证：提交意图保存失败发送 0 次；TaskID 保存失败发送 1 次且不重提；已保存身份／未知提交不会因诊断写入失败回退。重连及重启不会增加未知提交次数。查询和下载中间失败逐次留存，成功后清除当前错误；上传／下载最多三次，重启查询使用新尝试身份并保持原 TaskID。

安全检查涵盖日志、事件、HTTP、WebSocket、会话及执行导出。签名 URL、Authorization、Key、上传 token、控制字符和超长追踪字段不进入公开诊断。受控会话通过真实 Runner 产生未知提交，HTTP、WS 和导出的诊断相同，重复读取／重连保持 1 个制作卡片、1 次生产提交，其他匿名身份被拒绝。

第一次 race 有 1 项新测试断言失败：误把下载穷尽后的工具失败反馈当作 Go error。按既有 Runtime 协议修正测试（不改变预算或生产行为）后，局部与全量 race 均通过。默认沙箱的临时端口监听受限，因此网络测试在获准环境运行。

## ECS 接入核对

实际服务 `tripo-agent.service`，服务用户 `tripo-agent`，环境文件 `/etc/tripo-agent/app.env`，工作目录最终解析为 `/opt/tripo-agent/releases/ySkhUMdg/dist`，数据为 `/var/lib/tripo-agent`。核对时运行旧构建，未显式配置 `TRIPO_BASE_URL`，活动库包含 1 条旧执行。

候选只读预检使用旧进程有效 Key，目标国内 V3，2026-09-19 05:30:58 UTC 返回 HTTP 401、供应商码 2、耗时 56ms，追踪 ID `0487f213c517a1d08cafeb9abc85d505`。网络已到达鉴权接口；不能把它记为接入成功。此时未停服、未清空、未生产。

安全比较确认本地 Key 与 ECS Key 不同；本地已更新 Key 通过临时 ECS 公钥加密传输，命令和报告不含明文凭证。后续候选鉴权、发布、真实生成和减面结果在下方独立记录。

换用本地已更新 Key 后，候选预检 HTTP 200 / code 0，60ms，追踪 ID `5d05ae12c58fcec696d8172597c8d236`。只读核对可用额度足够计划中的 70 credits（保守覆盖标准/高清纹理低模生成与一次智能减面；不输出余额数值）。价格依据为[国内官方价目表](https://developers.tripo3d.com/en/pricing)，实际消费以本次任务记录为准。

停写前确认唯一旧 Run `df3e72bad162894f4cffa8879943cfa1feb25946754bda6c` 已 failed，操作 `f02bdb36e4a442cc833efe4d1d0d4fb6f357f221f53b2ea2` 保持 submitting、TaskID 空、生产消耗 1。没有重新提交或查询该旧操作。

停服后确认 MainPID=0、无进程打开数据文件，完整备份至 ECS `/root/tripo-china-20260919/backup`。SQLite 完整性为 ok、外键检查无异常，3 个文件的 SHA256/大小与备份一致。旧库有 1 Session、29 events、1 checkpoint、1 conversation、0 asset_versions。旧目录保留为备份下 `retired-data`，原路径创建空目录；配置保持到独立安装步骤才更新。

首次上线版本位于 `/opt/tripo-agent/releases/china-20260919-bd0875b1`，`current` 原子切换至此。该版服务 SHA256 为 `bd0875b130c208a7d8c058b258c15c6e0b7710d93b06178188b97fa4277b8866`，预检程序为 `41e97d593afe8eed68e0b0cc1038ffd497ab4fca826729f579d30500a0c2bd04`。最终源码构建输入清单见 [source-manifest.json](source-manifest.json)。页面增加旧记录“没有更详细诊断”提示后重新通过 15 项网页测试和构建；随后传输层补全及最终换版记录如下。

启动后实际二进制 SHA256 与候选相同，国内基址及候选 Key 加载正确；预检 HTTP 200 / code 0，69ms，追踪 ID `933dd3d05c015d16ab4db2737a19586d`。空库 Session/conversation/checkpoint/version 均为 0，确认没有恢复旧国际任务。网页 `/api/config` 返回 live、ready=true、模型 `deepseek-v4-pro`；这只作为配置辅助事实，不替代上述鉴权证据。

传输层最终复核发现 Go HTTP/2 对尚未写入的不可用连接仍有内部重试路径，因此将带凭证 API 显式限制为 HTTP/1.1。支持 HTTP/2 的 TLS 测试服务器实测仍协商为 HTTP/1.1；禁止重定向、连接复用、诊断、Provider 和恢复测试再次以 race 运行通过（app 20.571s、Tripo 3.302s、check-tripo 2.006s），相关 vet 通过。此为同一“不隐式重发”要求的实现补全，未增加生产能力或重试额度。

该修订版首次 ECS 预检未通过，未切换正式服务。原因是 `Transport.Clone()` 继承了已初始化的 h2 ALPN 通告，单独限制 Protocols 不足；同步限制 ALPN 为 `http/1.1`，TLS 测试仅注入受信任根证书而保留真实 ALPN 配置后，Tripo 全量 race 3.710s、check-tripo race 2.117s 及 vet 通过。早先一次预检误用 `/root` 工作目录也被拒绝，随后按服务实际工作目录执行；两次预检失败都没有清理、部署或增加生产。

最终部署目录 `/opt/tripo-agent/releases/china-20260919-26ecaae4`，服务 SHA256 `26ecaae4d987b5324cac943435d992065ea29816ef27e72773595959fc0a16cf`，预检程序 SHA256 `60ef639b1b85ab6d4e5b3e4548a739e6c6b122dec562ac88fd2ead01ce6d5e5c`。换版前国内预检 200/code 0/79ms（trace `fa87689672e0a0cbd03920514bf059fd`），换版后 200/code 0/72ms（trace `114a3be26f9bf0da56ca2fce9ae937ab`）。本地构建输入清单对应此最终源码。

v1 完成后正常停服，国内数据另备份至 `/root/tripo-china-20260919/domestic-before-transport-update`，切换程序后重启。新旧 Key／国内基址保持相同；v1 Run completed、production=1、TaskID、version ID 和模型 SHA256 前后完全相同。没有再次清空数据、查询旧国际任务或重新生成 v1。网页自动重连为“进度已连接”，v2 输入草稿及显式 v1 引用保持；待后续正式发送才创建新目标。

## 真实生产验收

未运行完整 Agent 案例集；本次仅验证一次真实生成和同会话一个减面目标。减面目标实际包含两次生产提交，第二次为既有预算内的自动纠偏；共三个版本，均核验技术报告、下载、网页加载、重连与版本证据。

真实网页会话：`c54015618826ba10be5ebc3b5bbb9ea5fd68a522346c5929`。首次请求为 Blender 静态场景用低模木箱，标准 PBR 纹理、自包含 GLB，明确不设面数／体积验收上限。真实 DeepSeek 保存两项上限为 null、来源 unset；只选择 2000 面作为生成参数。

v1 生成 Run 与会话同 ID，Operation/version `41c93efacc08793fe613176fb855e8a67c37becd37a9e259`，国内 TaskID `b27d1d22-047a-4c84-906c-3c667f364876`。2026-09-19 05:44:08 UTC 文件登记完成：**2696 三角面、947460 字节**，静态自包含 GLB 技术检查通过，无验收上限。Run completed，生产提交 **1**、模型调用 **5**，任务消费记录 **30 credits**。没有将生成目标 2000 当成验收上限。

浏览器实际下载 HTTP 200、GLB magic 为 `glTF`，SHA256 `63f5eacb67ca5d3ce2e4a3b7d43fe7db92f6972343c5af7590f675a6e4dda224` 与版本登记相同。`model-viewer.loaded=true`，鼠标拖拽前后相机轨道发生变化且仍加载成功。此项只验证加载和交互，不评分外观。

## 减面实际结果与验收偏差

减面 Run `28707de5c7cc63226e4fc6c0efb4ba52ec5423e28955745b` 从网页正式发送，命令显式携带 v1 ID，要求最多 1500 面，其他条件及无体积上限保持。

| 版本 | 来源 | 生成目标 | 实测面数 | 实测字节 | 结论 |
| --- | --- | --- | --- | --- | --- |
| v1 | 文本生成 | 2000 | 2696 | 947460 | 通过；未设置面数验收上限 |
| v2 | 显式引用 v1 减面 | 1500 | 1839 | 2119412 | 文件有效，但超过 1500 验收上限，未通过 |
| v3 | Agent 依据 v2 报告自动纠偏，以 v2 为输入 | 1200 | 1473 | 2166452 | 通过 1500 面验收上限；正式交付 |

v2 Operation/version `3db815ca690d63ca10f0df1cb94ffa9b5322d8c4b266a22b`，TaskID `8d60b07c-83ee-47ad-a7e6-015a1c560ee5`；v3 Operation/version `94b2deaee96f8f06101eab83151b0aa12f012e0c2027cbe4`，TaskID `a4842650-4819-4ee4-9ab7-21fc20f1b281`。第二个 Run 最终 completed，实际生产 **2 次**、模型调用 **6 次**。额外一次由 Agent 在原有纠偏预算内发起，理由是已保存技术报告不达标；不是网络层重试，也没有人工新开 Run 重跑。

**验收范围已由用户确认调整。** 原计划为一次减面生产提交；用户于 2026-09-19 回复“接受”，确认按一个减面目标包含既有预算内自动纠偏的实际链路验收。最终 v3 为 1473 面，满足原有 1500 面上限，来源、下载及重连证据完整，任务 6.5 完成，本次变更为 **23/23**。v2 的 1839 面及未通过报告继续保留，实际生产次数仍为一次生成加两次减面；本次确认未新增模型或供应商调用，未修改代码、预算或未知提交不重试规则。这不代表完整 Agent 案例集重新通过。

## 下载、来源与重连核验

三个版本均通过实际网页加载检查，分别从受保护下载接口返回 HTTP 200 / `glTF`，实测字节和 SHA256 与版本登记完全一致。v2 的未达标状态在页面、报告和导出中保持；v1 未被覆盖，v3 的真实 parent 是 v2。

首次加工的 `input_preparing/input_prepared` 事件都引用 v1，v2 parent 同样为 v1；输入读取路径要求字节哈希与固定版本记录一致。第二次加工事件引用 v2，数据库 Current 的输入 SHA256 与 v2 实际文件哈希相同，上传次数为 1。两次准备各有一对准备事件，没有刷新上传 token 或扩大准备预算。

刷新页面、两次独立 WebSocket 连接和会话导出均得到 cursor=109、相同 2 个 Run、3 个版本、parent/哈希和生产次数。共 3 张制作卡片，没有重连重复卡片。独立匿名上下文读取会话导出返回 404。执行导出和会话导出未匹配认证、上传 token 或签名查询敏感模式；浏览器控制台 error=0。

最终 ECS SQLite `integrity_check=ok`，旧国际 Run 不存在。数据库仅有 **3 个 tool_submitting、3 个 tool_submitted、2 个 input_preparing、2 个 input_prepared**，真实链路 provider_call_failed=0。访问、下载、导出和重连没有新增生产。无错误的真实链路不能替代受控失败诊断测试。

脱敏结构化证据见 [live-evidence.json](live-evidence.json)，包含每个版本的文件校验、两份执行导出的关键事件，以及 HTTP/WS/数据库一致性核对。仓库没有保存真实模型大文件或凭证；运行文件保留在 ECS 私有数据目录。
