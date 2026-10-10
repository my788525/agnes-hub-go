## 白嫖 Hub (Agnes Hub) v{{VERSION}}

多账号聚合中转网关，支持 agnes / AMD / NVIDIA / M365 / OpenRouter 等渠道统一调度与 RPM 限流排队。

### 本版主要变更：控制台实时动态图表（任务管理器风格）

总览页首屏新增「运行实况 · 实时」区块——**每秒采样、不刷新页面、图表自动向左滚动**，挂机时一眼看出网关是不是在干活。

三张图：

1. **多账号分配**（堆叠面积图）：各账号完成请求的占比。色块上直接标注账号名（带宽足够时附百分比），不用对照图例；某个颜色消失 = 该账号正在限流 / 冷却 / 熔断。
2. **负载率 %**：到达速率 ÷ text 池 RPM 容量。绿 <70% 有余量、黄 70~100% 接近打满、红 ≥100% 说明排队在兜底；红虚线 = 池容量 100%。
3. **处理规模**：到达 req/s vs 完成 req/s（两条线贴合 = 没积压）、在途数（紫虚线）、排队深度（黄色面积，右轴）。

细节：

- 窗口档位 **30s / 60s / 5m / 1h**，默认 60s；1h 自动切到粗粒度数据。
- 图表底部竖线标记事件时刻：上游 429（红）、熔断打开（橙）。
- 轮询只在总览页且页面可见时进行，切走标签页或最小化即停，不空耗。
- 图表引擎为自绘 canvas（约 120 行），**不引第三方库**，保持 go:embed 单文件零依赖。

### 后端

- 新增 `internal/hub/history.go` 采样器：每秒拍一个快照（负载率 / 到达率 / 完成率 / 排队深度 / 在途 / 429 速率 / 每账号完成数差分 / 事件位掩码），写入双环形缓冲 —— fine 600 点 @1s（10 分钟）+ coarse 360 点 @10s（1 小时，自动降采样），内存上界 <100KB。
- 新增 `GET /api/metrics-history?win=fine|coarse`，账号图例按 id 排序，颜色稳定不跳变。
- `main.go` 启动时拉起 `StartHistory(ctx)`，与 `StartMaintenance` 并列。

### 修复

- **重启后堆叠图首帧出现巨大尖峰**：账号 `Stats.Requests` 是持久化累计值，而差分基线从 0 起算，于是重启后第一个采样点把全部历史量当成「这一秒完成」（实测 1103 / 1079 / 500）。改为首点只建基线、不记增量，并补回归测试 `TestHistoryFirstSampleIsBaselineOnly`。

### 质量

`go build` / `go vet` 全绿；hub 包 3 个 history 测试通过；全量 `go test ./...` 通过；前端绘图用假 canvas 做运行时冒烟（4 种窗口 + 空数据 + 单账号场景）。

### 下载

- `baipiao-hub-{{VERSION}}.fpk` — 飞牛 fnOS 安装包（应用中心安装，应用名「白嫖 Hub」，内部 appname 保持 `agnes-hub` 以保留已装应用数据）
- `agnes-hub-go-windows-{{VERSION}}.zip` — Windows 绿色版（含 exe + 启动脚本 + README）
- `baipiao-hub-linux-amd64` / `baipiao-hub-linux-arm64` — Linux 二进制（x86_64 / aarch64）
- `agnes-hub-go.exe` — Windows 裸可执行文件
