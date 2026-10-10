## 白嫖 Hub (Agnes Hub) v{{VERSION}}

多账号聚合中转网关，支持 agnes / AMD / NVIDIA / M365 / OpenRouter 等渠道统一调度与 RPM 限流排队。

> 本版是自 v1.0.29 以来五个迭代的合集，主线只有一个目标：**除网络故障与上游彻底失效外，任何中断都在网关内消化——不让客户端的任务停下来**；同时把「网关到底在不在干活」变成肉眼可见。

### 一、不让客户端停摆：截断续写 + 软失败换号（核心）

总原则：**能续写就续写，能重发就换号重发**，绝不把中间态甩给客户端。已覆盖的七条中断路径：

1. **429 限流** → 网关内长预算自动重试（换号 + 重新排队 + `Retry-After` 退避），预算用尽才透传。
2. **MAX_TOKENS 截断**（OpenAI `finish_reason=length` / Gemini `MAX_TOKENS` / Anthropic `max_tokens`）→ 自动续写：把已产出的部分文本作为 assistant 上下文，追加续写指令后重新排队，最多 `continuation_max_rounds`（默认 2）轮，客户端只看到一条完整连贯的响应。
3. **HTTP 200 但内容不可用**（空内容 / body 内嵌 error / 非法 JSON / 审查类 finish_reason）→ 判定为软失败后**换号重发**，上限 `soft_fail_retry_max`（默认 2，设为 0 即回到旧行为）。
4. **流式隐式截断**（未见任何 finish_reason 就断流）、**中途 error 帧**、**传输异常** → 自动续写；若完全无产出，则用原始请求换号重发（`Exclude` + 断开 SessionKey 粘性，确保换通道而非粘回原号）。
5. **402 额度耗尽** → 额度是按账号计的，换号重试；全部耗尽才交还真实 402。
6. **无号可用** → 返回最后一次的真实上游结果（含 429/402 语义），而不是甩一个没有信息量的「无可用账号」。
7. **缺 finish chunk 就断流** → 合成 `finish_reason=stop` + `[DONE]`，否则客户端 SDK 会判「响应未完成」直接抛错终止任务。

**边界铁律**：软失败检测只认 OpenAI / Gemini / Anthropic 三种文本对话结构，**认不出的一律视为可用**。视频轮询返回 `{status:"queued"}` 没有 choices，误判成空内容会导致无限重发——已由 `TestVideoPollShapeNotRefired` 锁死。Gemini 的文本藏在 `content:{parts:[{text}]}` 里，必须单独取值。

新增配置：`continuation_max_rounds`(2)、`soft_fail_retry_max`(2)、`stream_auto_resume`(true)。

### 二、看得见：控制台实时动态图表

总览页首屏「运行实况 · 实时」——**每秒采样、不刷新页面、canvas 自动向左滚动**，挂机时一眼看出网关是否在干活。

- **多账号分配**（横向直方图）：Y 轴账号名、X 轴占比百分比，带缓动插值；某个账号的条消失 = 它正在限流 / 冷却 / 熔断。
- **负载率 %**：到达速率 ÷ text 池 RPM 容量，绿 <70% 有余量、黄 70~100% 接近打满、红 ≥100% 说明排队在兜底。
- **处理规模**：到达 req/s vs 完成 req/s（贴合 = 无积压）、在途数、排队深度（右轴）。
- 窗口档位 **30s / 60s / 5m / 1h**，默认 60s，1h 自动切粗粒度数据；图表底部竖线标记 429（红）与熔断打开（橙）时刻。
- 轮询只在总览页且页面可见时进行；新增「网关正在处理 N 个请求」胶囊（持续未走完转橙色）与「待转发」读数。
- 图表引擎为自绘 canvas（约 120 行），**不引第三方库**，保持 `go:embed` 单文件零依赖。

后端配套 `internal/hub/history.go`：每秒拍快照写入双环形缓冲（fine 600 点 @1s + coarse 360 点 @10s 自动降采样），内存上界 <100KB；`GET /api/metrics-history?win=fine|coarse`，图例按 id 排序、颜色稳定不跳变。

### 三、调度：不同任务走不同的号

多个客户端任务并发时，不再把压力堆到同一个账号通道上。`pickScore` 新增 **sessions 维度**（当前绑定会话数越少越优先），配合 `BindingsCountByAccount()`，让并发任务天然分散到不同账号，同时保留同一会话的粘性（上下文不乱跑）。

### 四、修复

- **body 读取无超时**（重要）：`http.Server` 此前漏了 `ReadTimeout`，若客户端把请求体发一半就挂着（或直接不发），handler 会永久阻塞，且**所有指标看起来都是 0** —— 表现为「客户端一直转圈，控制台负载 0」。现补 `ReadTimeout: 5m`。
- **负载率在低频大任务场景恒接近 0**：单请求 60s 窗口里只有 1/60 rps，指标天然失真，不能据此判断网关是否空闲。改以 `pending` / `accepted` 绝对数为准。
- **重启后图表首帧出现巨大尖峰**：账号 `Stats.Requests` 是持久化累计值，而差分基线从 0 起算，重启后第一个采样点会把全部历史量当成「这一秒完成」。改为首点只建基线、不记增量（`TestHistoryFirstSampleIsBaselineOnly`）。
- **堆叠图部分尖峰漏标账号名**：改为在该账号「最厚的一列」上标注，并放宽门槛 + 防重叠；行高 <13px 时不写数字。
- OpenAI 非流式续写时内容取错层级（在 `choices[0].message.content` 而非 choice 顶层），导致合并后为空。

### 五、可观测性

`/healthz` 新增字段：`pending`、`accepted`、`continuations`、`soft_fail_retries`、`resumed_streams`。

排障判据（写在这里便于日后对照）：

- `pending = 0` 且 `accepted` 不涨 → **请求压根没到网关**，查客户端 / 网络。
- `pending` 持续 > 0 → 卡在网关内的某个环节。
- `last_success_age_ms` 是「多久没有流量真正跑通」的第一指标，比 `uptime` 有用得多。

### 六、质量

`go build` / `go vet` 全绿；全量 `go test ./...` 通过（新增 `continuation_test.go`、`resilience_test.go`、`history_test.go` 等）。

### 下载

- `baipiao-hub-{{VERSION}}.fpk` — 飞牛 fnOS 安装包（应用中心安装，应用名「白嫖 Hub」，内部 appname 保持 `agnes-hub` 以保留已装应用数据）
- `agnes-hub-go-windows-{{VERSION}}.zip` — Windows 绿色版（含 exe + 启动脚本 + README）
- `baipiao-hub-linux-amd64` / `baipiao-hub-linux-arm64` — Linux 二进制（x86_64 / aarch64）
- `agnes-hub-go.exe` — Windows 裸可执行文件
