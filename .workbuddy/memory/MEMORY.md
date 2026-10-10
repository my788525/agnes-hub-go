# 项目长期记忆（agnes-hub-go / 白嫖 Hub）

## fnOS(飞牛) NAS 热更二进制（最高频踩坑，固化）
- SSH 节点 192.168.100.3，用户 `admin`（**uid=1000，不是 root**，属 Administrators/docker 组），口令在 `D:/work/M365-fpk/.nas_pass`，用 `paramiko.SSHClient()`（5.0.0 精简版，连接类叫 SSHClient 非 Client）。
- 二进制路径 `/vol1/@appcenter/agnes-hub/app/baipiao-hub`；数据 `/vol1/@appdata/agnes-hub/data`；端口 4142。app id = `agnes-hub`（应用中心显示名「白嫖 Hub」）。
- **稳健热更三步（2026-10-10 修正，按这个来）**：
  1. 停服：**PTY sudo** 调 `appcenter-cli stop agnes-hub`（非 PTY 会 panic、且 stop 后进程常残留）→ 取 `pgrep -f baipiao-hub` 的 PID → **PTY sudo `kill -9 <pid>`**（关键修正：admin 非 root，普通 `pkill -9 -f` / `kill -9` **杀不掉**以 agnes-hub 身份运行的进程，实测旧进程 PID 原封不动活着、healthz 一直显示旧版本）→ 等 2~3s 确认 NO_PROC。
  2. 替换：SFTP 传新二进制到 `/tmp`，然后**普通 `cp -f` 覆盖**（admin 可写该路径，无需 sudo）+ `chown agnes-hub:agnes-hub` + `chmod 755`。
  3. 启动：**PTY sudo** 调 `appcenter-cli start agnes-hub`。
- PTY sudo 写法（三处通用）：`chan.get_pty(); chan.exec_command("sudo -S "+cmd); chan.send(PW+"\n")`，轮询 `chan.exit_status_ready()`（start 给 60~90s，`stopping/starting...` 动画很长）。
- **三个必踩的雷（已实证）**：
  - 非 PTY 的 `echo '<PW>' | sudo -S <cmd>` 在 fnOS 上**不返回退出状态**→ 每个 sudo 调用耗满 120s 超时挂死，导致 replace/start 跑不到、服务停服。
  - 普通（非 PTY）SSH 会话里 `appcenter-cli stop/start/list` 会 **Go panic**：`panic: ApplyPermission ... dial unix /run/trim_app_cgi/rpcbroker: connect: permission denied`（拿不到 broker 会话上下文）。
  - **只做「stop 成功」不算停服**：`[Info]Application [agnes-hub] stop success` 之后进程仍可能还在跑（healthz 仍是旧版本）。必须 `pgrep` 复核到 NO_PROC 再替换，否则新二进制只是躺在磁盘上，跑的还是旧进程。
- 验证：`curl -s http://127.0.0.1:4142/healthz` 看 `version`/`ok`/`panics_total`；`grep -ac 'truncated?' <binary>` 确认前端修复已嵌入。
- 保留数据直接替换二进制即可，不走 uninstall；旧 binary 先备份到 `/vol1/@appdata/agnes-hub/app.PRE<时间戳>.bak`。

## 发布流水线（固化）
- 版本单一来源：`main.go` 的 `var version = "x.y.z"`；`tools/build_fpk.py` 与 `tools/release.py` 都从它读，勿手改两处。
- `tools/release.py`：交叉编译 linux-amd64/arm64 + windows exe（前置校验嵌入版本==main.go）→ 打 `baipiao-hub-{ver}.fpk` + `agnes-hub-go-windows-{ver}.zip` → tag v{ver} → GitHub Release + 上传 5 资产。
- 跑 release.py 用 venv python（`C:/Users/pguoy/.workbuddy/binaries/python/envs/default/Scripts/python.exe`，有 requests）。设 `GITHUB_PAT` 环境变量（绝不入代码），走代理 `http://127.0.0.1:3067`。
- **Release 正文**：`release_notes()` 优先读 `tools/release_notes.md`（支持 `{{VERSION}}` 占位），缺失才回退内置文案。发版时**只改这个 md，不要再改 release.py**（此前正文硬编码在 py 里，导致长期挂着旧版「上游上下文策略」文案）。
- 顺序：先把脚本/说明改动本地 commit，再跑 release.py，让 tag 指向含这些改动的 commit。
- release.py 只推 **tag**，分支要**单独 fast-forward 推送**（勿 `--force-with-lease`，本地远程跟踪过期会被拒）。
- GitHub 仓库 `my788525/agnes-hub-go`；无 `gh` CLI，用 PAT + REST API。

## 「客户端任务中断」防御体系（v1.0.33~1.0.34，续写/重试总纲）
核心原则：**除网络故障与上游彻底失效外，任何中断都要在网关内消化**——能续写就续写，能重发就换号重发，绝不把中间态甩给客户端。已覆盖的七条路径：
1. **429** → 网关内长预算自动重试（换号 + 重新排队 + Retry-After 退避），预算尽才透传。
2. **MAX_TOKENS 截断**（`length`/`MAX_TOKENS`/`max_tokens`）→ `internal/relay/continuation.go` 自动续写（部分输出作 assistant 上下文 + user 续写指令，重新排队）。
3. **HTTP 200 但内容不可用**（空内容 / body 内嵌 error / 非法 JSON / 审查类 finish_reason）→ `resilience.go` 的 classifyResponse 判定后**换号重发**（`soft_fail_retry_max` 默认 2）。
4. **流式隐式截断**（未见任何 finish_reason 就断流）、**中途 error 帧**、**传输异常** → 续写；无产出则用原始请求换号重发（`Exclude` + 断 SessionKey 粘性）。
5. **402 额度耗尽** → 换号重试（额度按账号）；全部耗尽才交还真实 402。
6. **无号可用** → 返回最后那次真实上游结果（含 429/402 语义），不甩无信息量的「无可用账号」。
7. **缺 finish chunk 就断流** → 合成 `finish_reason=stop` + `[DONE]`：client SDK 否则会判「响应未完成」抛错终止任务。
- **边界铁律**：软失败检测只认 OpenAI/Gemini/Anthropic 三种文本对话结构，**认不出的一律视为可用**（视频轮询 `{status:"queued"}` 无 choices，误判空内容会无限重发，已有测试守住）；只在 Continuable 路径启用；Gemini content 是 `{parts:[{text}]}` 必须单独取。
- 相关设置：`continuation_max_rounds`(2)、`soft_fail_retry_max`(2)、`stream_auto_resume`(true)；线上另有 `request_timeout_ms`(300000，非流式墙钟)、`stream_idle_timeout_ms`(90000，流式空闲看门狗)、`retry_max`(3)、`rate_limit_retry_max`(8)。注意 `internal/relay/relay.go` 的 upstream `http.Client` **不设** Client.Timeout（会掐长 SSE），超时由 per-request context 承载，别误加成 Client.Timeout。
- **这套体系的边界：以下情况客户端仍会停，且网关救不了**（2026-10-10 盘点，答复「现在是不是基本不会异常停止」用）：
  1. **请求没到网关**（今日实证的 CF 隧道黑洞 / 代理 / NAT 止血）。发生于网关之前，续写换号都够不着 → 只能客户端侧超时 + 内网直连。
  2. **网关进程重启 / 热更**：优雅停机有 30s 排空宽限，但窗口外的在途请求会被切断。热更前知会用户。
  3. **非文本模态**（生图 / 生视频 / 视频轮询）不在 Continuable 路径，不享受续写与软失败换号重发，仅保留 429/5xx 换号重试。
  4. **tool_calls 截断**提取不到纯文本 → 明确不续写，原样返回。
  5. **上游彻底失效 / 所有号额度耗尽 / 全熔断**：预算耗尽后如实把最后一次真实上游结果（402/429/5xx）交还客户端——这是刻意设计，不是缺陷。
  6. **客户端自身超时**：客户端预算应 ≥ 网关预算（非流式 5min × 3 次重试的量级），否则网关还在重试、客户端已放弃。
- 观测：`/healthz` 的 `continuations` / `soft_fail_retries` / `resumed_streams` / `pending` / `accepted`。

## 客户端接入路径铁律（2026-10-10 实证，血泪）
**内网一律 `http://192.168.100.3:4142/v1` 直连，绝不要绕任何中间层。** 用户此前把 WorkBuddy 渠道配成 **Cloudflare Tunnel 域名**（NAS 上装了 `com.dustinky.tunnel`，cloudflared `--protocol quic`），导致三大症状：
- 客户端「思考中」卡 **18 分钟**，网关侧 usage/raw_capture **零痕迹**（请求死在到达网关之前）。
- 「测试连接」时好时坏：1 秒成功 / 20 秒 / 1 分钟失败。
- 换回局域网 IP 后 **10/10 全部成功**（决定性对照实验）。
机理：CF 边缘对 idle 连接约 100s 就掐，客户端 keep-alive 池复用半死连接、往里写 275KB 大上下文 → 写进黑洞 → TCP 重传超时十几分钟；QUIC/UDP 走家宽 QoS 抖动 + 边缘重建慢 = 20s~1min 延迟。
**这类中断网关的续写/换号韧性层救不了**（发生在网关之前），只能靠客户端侧超时 + 直连。用户已卸载 cloudflared（2026-10-10 19:0x，验证：无 cloudflared 进程、应用中心列表已无 tunnel）。
同理提醒：动态域名/DDNS 在内网访问依赖路由器 **NAT 回环（hairpin）**，多数家用路由不支持 → 在家照样别用域名；外网要用需端口转发 4142，且必须确认控制台强密码 + downstream key 随机后再暴露。

## 排障套路：「客户端转圈但控制台看不出负载」
1. 先看 `/healthz`：`pending>0` → 请求在网关内（HTTP 层已进入但没走完），去查 upstream；`accepted` 不涨 → 请求压根没到网关，查客户端/网络/**接入路径是否绕了隧道或代理**。
2. `last_success_age_ms` 是「多久没有流量真正跑通」的第一指标，比 uptime 有用得多。
3. `ss -tn | grep 4142` 看是否还有连接存活；`raw_capture/` 目录 mtime 是「最近一次收到请求」的旁证（RawCaptureEnabled 开启时）。
4. 用 curl `-w "TTFB=%{time_starttransfer}s TOTAL=%{time_total}s"` 精确测网关吞吐，避免凭感觉判断慢在哪。
5. **负载%在低频大任务场景恒接近 0**（单请求 60s 窗口只有 1/60 rps），不能据此判断网关是否空闲——要看 pending/inflight 绝对数（控制台已有「网关正在处理 N 个请求」胶囊）。

## 全局约定（来自 user memory，跨项目）
- 源码改动默认自动 push（经代理 + PAT），推送前核查无遗漏源码目录。
- git commit summary/description 用中文。
- 站点/工具 UI 文案北美英语（禁 CJK），但思考与回复可用中文。
- 配图走 Unsplash API 本地化 WebP，不热链 CDN。
