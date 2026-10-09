# 项目长期记忆（agnes-hub-go / 白嫖 Hub）

## fnOS(飞牛) NAS 热更二进制（最高频踩坑，固化）
- SSH 节点 192.168.100.3，用户 `admin`（即 root），口令在 `D:/work/M365-fpk/.nas_pass`，用 `paramiko.SSHClient()`（5.0.0 精简版，连接类叫 SSHClient 非 Client）。
- 二进制路径 `/vol1/@appcenter/agnes-hub/app/baipiao-hub`；数据 `/vol1/@appdata/agnes-hub/data`；端口 4142。app id = `agnes-hub`（应用中心显示名「白嫖 Hub」）。
- **稳健热更三步（不要改）**：
  1. 停服：普通 `appcenter-cli stop agnes-hub`（panic 也无害）+ `pkill -9 -f baipiao-hub`；等 2s 确认 NO_PROC。
  2. 替换：SFTP 传新二进制到 `/tmp`，然后**普通 `cp -f` 覆盖**（admin=root 可覆盖 agnes-hub 属主文件，无需 sudo）+ `chown agnes-hub:agnes-hub` + `chmod 755`。
  3. 启动：**必须用 PTY 方式 sudo** 调 `appcenter-cli start`（写法 `chan.get_pty(); chan.exec_command("sudo -S "+cmd); chan.send(PW+"\n")` 轮询 `exit_status_ready()`，40s 超时防挂）。
- **两个必踩的雷（已实证）**：
  - 非 PTY 的 `echo '<PW>' | sudo -S <cmd>` 在 fnOS 上**不返回退出状态**→ 每个 sudo 调用耗满 120s 超时挂死，导致 replace/start 跑不到、服务停服。
  - 普通（非 PTY）SSH 会话里 `appcenter-cli stop/start/list` 会 **Go panic**：`panic: ApplyPermission ... dial unix /run/trim_app_cgi/rpcbroker: connect: permission denied`（拿不到 broker 会话上下文）。
- 验证：`curl -s http://127.0.0.1:4142/healthz` 看 `version`/`ok`/`panics_total`；`grep -ac 'truncated?' <binary>` 确认前端修复已嵌入。
- 保留数据直接替换二进制即可，不走 uninstall；旧 binary 先备份到 `/vol1/@appdata/agnes-hub/app.PRE<时间戳>.bak`。

## 发布流水线（固化）
- 版本单一来源：`main.go` 的 `var version = "x.y.z"`；`tools/build_fpk.py` 与 `tools/release.py` 都从它读，勿手改两处。
- `tools/release.py`：交叉编译 linux-amd64/arm64 + windows exe（前置校验嵌入版本==main.go）→ 打 `baipiao-hub-{ver}.fpk` + `agnes-hub-go-windows-{ver}.zip` → tag v{ver} → GitHub Release + 上传 5 资产。
- 跑 release.py 用 venv python（`C:/Users/pguoy/.workbuddy/binaries/python/envs/default/Scripts/python.exe`，有 requests）。设 `GITHUB_PAT` 环境变量（绝不入代码），走代理 `http://127.0.0.1:3067`。
- release.py 只推 **tag**，分支要**单独 fast-forward 推送**（勿 `--force-with-lease`，本地远程跟踪过期会被拒）。
- GitHub 仓库 `my788525/agnes-hub-go`；无 `gh` CLI，用 PAT + REST API。

## 全局约定（来自 user memory，跨项目）
- 源码改动默认自动 push（经代理 + PAT），推送前核查无遗漏源码目录。
- git commit summary/description 用中文。
- 站点/工具 UI 文案北美英语（禁 CJK），但思考与回复可用中文。
- 配图走 Unsplash API 本地化 WebP，不热链 CDN。
