#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""构建 baipiao-hub 的飞牛 fnOS fpk 安装包。

为什么手写 tarfile 而不用 fnpack：
    本机 fnpack.exe 在 MSYS 环境下 `build` 必然失败
    （`Copy pack ... to tmp dir ... error : CreateFile ... The system cannot find the file specified`，
     路径转换所致）。双 tar.gz 结构本身很简单，手搓反而可控。

产出结构（对齐 fnOS 官方可装 fpk，如 M365-Copilot2API-FNOS）：
    .fpk (外层 tar.gz)
      ├── manifest              key=value 文本，不是 JSON（写成 JSON 会让 appcenter 报 code 10111）
      │                         —— 内含 checksum=<app.tgz 的 MD5>，应用中心据此校验完整性
      ├── ICON.PNG / ICON_256.PNG
      ├── cmd/                  生命周期脚本（main/install_init/upgrade_init/... 必须置于外层）
      ├── config/               privilege + resource（缺失会被应用中心拒绝）
      ├── wizard/install        无交互安装向导（空数组即可）
      └── app.tgz               内层 tar.gz，只放 app/ 双架构二进制
          ├── app/baipiao-hub
          └── app/baipiao-hub-arm64

关键坑：cmd/config/wizard 必须放在**外层** tar；塞进 app.tgz 会导致
应用中心找不到 cmd/main 而安装失败。fnOS 不要求独立的 manifest.checksum 文件。

用法：
    python tools/build_fpk.py                 # 输出到 dist/
    FPK_OUT_DIR=D:/somewhere python tools/build_fpk.py
"""
import io
import hashlib
import json
import os
import re
import shutil
import struct
import sys
import tarfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT_DIR = os.environ.get("FPK_OUT_DIR") or os.path.join(ROOT, "dist")
FPK_DIR = os.path.join(ROOT, "fpk-bundle")

APP_ID = "agnes-hub"
SERVICE_PORT = 4142


def _version_from_main_go() -> str:
    """版本单一来源：读 main.go 的 `var version = "x.y.z"`。

    杜绝 build_fpk.py 与 main.go 两处手改不同步导致产物名 / manifest /
    升级清理 VERSION_TAG 漂移（历史上 1.0.3 的 windows exe 误发事故根因）。
    """
    main_go = os.path.join(ROOT, "main.go")
    with open(main_go, "r", encoding="utf-8") as f:
        src = f.read()
    m = re.search(r'var\s+version\s*=\s*"(\d+\.\d+\.\d+)"', src)
    if not m:
        sys.exit("[ERROR] 无法从 main.go 解析版本号（需要形如 `var version = \"1.0.5\"`）")
    return m.group(1)


VERSION = _version_from_main_go()
# 嵌在二进制里的版本串，upgrade_init 用它判断「这个残留文件是不是本版本的」。
# 必须与 main.go 的 var version 完全一致，否则升级前置清理会把自己刚装的删掉。
# 这里直接取 VERSION，单一来源。
VERSION_TAG = VERSION
# 由 VERSION 推导，避免两处手改不同步导致产物名和 manifest 版本对不上。
FPK_NAME = "baipiao-hub-%s.fpk" % VERSION
APP_DIR = os.path.join(FPK_DIR, "app")
CMD_DIR = os.path.join(FPK_DIR, "cmd")
WIZARD_DIR = os.path.join(FPK_DIR, "wizard")
CONFIG_DIR = os.path.join(FPK_DIR, "config")

# 对齐 fnOS 官方可装 fpk（M365-Copilot2API-FNOS）的 cmd/main 形态：
# fnOS 应用中心会以 `cmd/main start` 拉起、`cmd/main stop` 停止、
# `cmd/main status` 探活（期望运行中返回 0、未运行返回 3）。
# 无参数调用默认按 start 处理，兼容 appcenter 直接 `cmd/main` 的场景。
MAIN_SCRIPT = '''#!/bin/bash

# fnOS 会把 app.tgz 的内容解压到 /var/apps/<app_id>/target（= /vol1/@appcenter/<app_id>/），
# 并且**额外套一层 <app_id>/ 子目录** —— 即真实二进制在 $APP_DIR/$APP_ID/app/。
# 注意：hook 阶段（install_init/upgrade_init/uninstall_init/config_init）的环境变量
# 不一定齐全，因此这里所有变量都要用 ${var:-} 兜底，并且 set -u 已经被禁用。
APP_ID="agnes-hub"
APP_DIR="${TRIM_APPDEST:-}"
[ -z "$APP_DIR" ] && APP_DIR="/var/apps/$APP_ID/target"
[ -d "$APP_DIR" ] || APP_DIR="/vol1/@appcenter/$APP_ID"

TRIM_TEMP_LOGFILE="${TRIM_TEMP_LOGFILE:-/tmp/agnes-hub-main-fallback.log}"

# 数据目录解析：优先 fnOS 生命周期变量，其次 start 时持久化的 datadir，
# 最后 fallback 到 /vol1/@appdata/<app_id>/data（这也是 fnOS 标准的应用数据目录）。
DATA_DIR=""
if [ -n "${TRIM_PKGVAR:-}" ]; then
  DATA_DIR="$TRIM_PKGVAR/data"
elif [ -f "$APP_DIR/$APP_ID/state/datadir" ]; then
  DATA_DIR="$(cat "$APP_DIR/$APP_ID/state/datadir" 2>/dev/null)"
elif [ -f "$APP_DIR/state/datadir" ]; then
  DATA_DIR="$(cat "$APP_DIR/state/datadir" 2>/dev/null)"
fi
[ -z "$DATA_DIR" ] && DATA_DIR="/vol1/@appdata/$APP_ID/data"

PORT="${AGNES_HUB_PORT:-%PORT%}"

# 定位二进制：兼容 $APP_DIR/$APP_ID/app/、$APP_DIR/app/ 以及历史残留路径
ARCH=$(uname -m)
case "$ARCH" in
  x86_64|amd64) BIN_NAME="baipiao-hub" ;;
  aarch64|arm64) BIN_NAME="baipiao-hub-arm64" ;;
  *) echo "unsupported arch: $ARCH" > "$TRIM_TEMP_LOGFILE"; exit 1 ;;
esac

BIN=""
for cand in "$APP_DIR/$APP_ID/app/$BIN_NAME" "$APP_DIR/app/$BIN_NAME" "$APP_DIR/$APP_ID/$BIN_NAME" "$APP_DIR/$BIN_NAME"; do
  if [ -x "$cand" ]; then
    BIN="$cand"
    break
  fi
done
if [ -z "$BIN" ]; then
  echo "binary not found (checked: $APP_DIR/$APP_ID/app/$BIN_NAME, $APP_DIR/app/$BIN_NAME, $APP_DIR/$APP_ID/$BIN_NAME, $APP_DIR/$BIN_NAME)" > "$TRIM_TEMP_LOGFILE"
  exit 1
fi

# 卸载留存恢复：数据目录缺少关键用户文件时，从留存目录回填（不覆盖现有）。
# 留存目录由 uninstall_init 在卸载前写入。
restore_keep() {
  local keep=""
  for cand in "/vol1/@appdata/$APP_ID-keep" "$DATA_DIR/../$APP_ID-keep" "/vol1/$APP_ID-keep"; do
    if [ -d "$cand" ] && [ -f "$cand/settings.json" ]; then
      keep="$cand"
      break
    fi
  done
  [ -z "$keep" ] && return 0
  if [ ! -f "$DATA_DIR/settings.json" ] || [ ! -f "$DATA_DIR/accounts.json" ]; then
    mkdir -p "$DATA_DIR"
    cp -an "$keep/." "$DATA_DIR/" 2>/dev/null || true
    echo "restored user data from $keep" >> "$DATA_DIR/app.log" 2>/dev/null || true
  fi
}

mkdir -p "$DATA_DIR" "$APP_DIR/$APP_ID/state"
echo "$DATA_DIR" > "$APP_DIR/$APP_ID/state/datadir"
if [ -d "$APP_DIR/state" ] || mkdir -p "$APP_DIR/state" 2>/dev/null; then
  echo "$DATA_DIR" > "$APP_DIR/state/datadir"
fi

# 安装向导设置的管理员密码：非空时传给 Go 二进制作为初始/覆盖密码；
# 留空时 Go 端不会覆盖现有密码（重装时保留）。
if [ -n "${wizard_admin_password:-}" ]; then
  export AGNES_ADMIN_PASSWORD="$wizard_admin_password"
fi

# -host 0.0.0.0 由 main.go 内部展开为 IPv4(0.0.0.0)+IPv6(::) 双栈，
# 满足飞牛外网 IPv6 域名直达 + 局域网 IPv4 访问。
start_service() {
  cd "$(dirname "$BIN")"
  nohup "$BIN" -host 0.0.0.0 -port "$PORT" -data "$DATA_DIR" >> "$DATA_DIR/app.log" 2>&1 &
  for i in $(seq 1 30); do
    if pgrep -f "$BIN" >/dev/null 2>&1; then
      sleep 1
      exit 0
    fi
    sleep 1
  done
  echo "failed to start $BIN" > "$TRIM_TEMP_LOGFILE"
  exit 1
}

cmd="${1:-}"; shift || true

case "$cmd" in
  start)
    restore_keep
    start_service
    ;;
  stop)
    pkill -f "$BIN" 2>/dev/null || true
    exit 0
    ;;
  status)
    if pgrep -f "$BIN" >/dev/null 2>&1; then
      exit 0
    fi
    exit 3
    ;;
  *)
    # 无参数默认 start
    restore_keep
    start_service
    ;;
esac
'''

UPGRADE_INIT = '''#!/bin/bash
# ============================================================
# upgrade_init — 升级前：安全停服、备份数据、清理旧版二进制
# ============================================================
APP_ID="agnes-hub"

# 1. 安全停服：按 exe 路径精确匹配，避免杀掉钩子脚本自身
for pid in $(pgrep -f "baipiao-hub" 2>/dev/null); do
  [ "$pid" = "$$" ] && continue
  exe=$(readlink "/proc/$pid/exe" 2>/dev/null) || continue
  case "$exe" in
    */baipiao-hub) kill "$pid" 2>/dev/null || true ;;
  esac
done
sleep 1

# 2. 数据目录解析与备份（升级绝不覆盖原数据）
DATA_DIR=""
if [ -n "${TRIM_PKGVAR:-}" ] && [ -d "${TRIM_PKGVAR}/data" ]; then
  DATA_DIR="${TRIM_PKGVAR}/data"
elif [ -n "${TRIM_APPDEST:-}" ] && [ -f "${TRIM_APPDEST}/state/datadir" ]; then
  DATA_DIR="$(cat "${TRIM_APPDEST}/state/datadir" 2>/dev/null)"
elif [ -n "${TRIM_APPDEST:-}" ] && [ -f "${TRIM_APPDEST}/${APP_ID}/state/datadir" ]; then
  DATA_DIR="$(cat "${TRIM_APPDEST}/${APP_ID}/state/datadir" 2>/dev/null)"
fi
[ -z "$DATA_DIR" ] && DATA_DIR="/vol1/@appdata/${APP_ID}/data"
if [ -d "$DATA_DIR" ] && [ -f "$DATA_DIR/settings.json" ]; then
  BACKUP_DIR="${DATA_DIR}.bak.$(date +%Y%m%d-%H%M%S)"
  cp -a "$DATA_DIR" "$BACKUP_DIR" 2>/dev/null || true
fi

# 3. 清理旧版二进制（保留数据目录）
BASES=""
[ -n "${TRIM_APPDEST:-}" ] && BASES="$BASES $TRIM_APPDEST"
[ -n "${TRIM_PKGROOT:-}" ] && BASES="$BASES $TRIM_PKGROOT"
for base in $BASES; do
  [ -d "$base" ] || continue
  for cand in "$base/$APP_ID/app/baipiao-hub" "$base/$APP_ID/app/baipiao-hub-arm64" "$base/$APP_ID/baipiao-hub" "$base/$APP_ID/baipiao-hub-arm64" "$base/app/baipiao-hub" "$base/app/baipiao-hub-arm64" "$base/baipiao-hub" "$base/baipiao-hub-arm64"; do
    [ -f "$cand" ] || continue
    grep -aq "%VERSION_TAG%" "$cand" 2>/dev/null || rm -f "$cand" 2>/dev/null || true
  done
done
exit 0
'''

INSTALL_INIT = '''#!/bin/bash
# ============================================================
# install_init — 安装/重装前：备份已有数据、清理残留旧版二进制
# ============================================================
APP_ID="agnes-hub"

# 1. 若已有数据目录，先带时间戳备份（绝不删除/覆盖原数据）
DATA_DIR=""
if [ -n "${TRIM_PKGVAR:-}" ] && [ -d "${TRIM_PKGVAR}/data" ]; then
  DATA_DIR="${TRIM_PKGVAR}/data"
elif [ -n "${TRIM_APPDEST:-}" ] && [ -f "${TRIM_APPDEST}/state/datadir" ]; then
  DATA_DIR="$(cat "${TRIM_APPDEST}/state/datadir" 2>/dev/null)"
elif [ -n "${TRIM_APPDEST:-}" ] && [ -f "${TRIM_APPDEST}/${APP_ID}/state/datadir" ]; then
  DATA_DIR="$(cat "${TRIM_APPDEST}/${APP_ID}/state/datadir" 2>/dev/null)"
fi
[ -z "$DATA_DIR" ] && DATA_DIR="/vol1/@appdata/${APP_ID}/data"
if [ -d "$DATA_DIR" ] && [ -f "$DATA_DIR/settings.json" ]; then
  BACKUP_DIR="${DATA_DIR}.bak.$(date +%Y%m%d-%H%M%S)"
  cp -a "$DATA_DIR" "$BACKUP_DIR" 2>/dev/null || true
fi

# 2. 清理残留旧版二进制（避免版本混乱）
BASES=""
[ -n "${TRIM_APPDEST:-}" ] && BASES="$BASES $TRIM_APPDEST"
[ -n "${TRIM_PKGROOT:-}" ] && BASES="$BASES $TRIM_PKGROOT"
for base in $BASES; do
  [ -d "$base" ] || continue
  for cand in "$base/$APP_ID/app/baipiao-hub" "$base/$APP_ID/app/baipiao-hub-arm64" "$base/$APP_ID/baipiao-hub" "$base/$APP_ID/baipiao-hub-arm64" "$base/app/baipiao-hub" "$base/app/baipiao-hub-arm64" "$base/baipiao-hub" "$base/baipiao-hub-arm64"; do
    [ -f "$cand" ] || continue
    grep -aq "%VERSION_TAG%" "$cand" 2>/dev/null || rm -f "$cand" 2>/dev/null || true
  done
done
exit 0
'''

UNINSTALL_INIT = '''#!/bin/bash
# ============================================================
# uninstall_init — 卸载前保留用户数据（账号 / 设置 / 聊天记录等）
# ============================================================
APP_ID="agnes-hub"

# 1. 停服
for pid in $(pgrep -f "baipiao-hub" 2>/dev/null); do
  [ "$pid" = "$$" ] && continue
  exe=$(readlink "/proc/$pid/exe" 2>/dev/null) || continue
  case "$exe" in
    */baipiao-hub) kill "$pid" 2>/dev/null || true ;;
  esac
done
sleep 1

# 2. 解析数据目录
DATA_DIR=""
if [ -n "${TRIM_PKGVAR:-}" ] && [ -d "${TRIM_PKGVAR}/data" ]; then
  DATA_DIR="${TRIM_PKGVAR}/data"
elif [ -n "${TRIM_APPDEST:-}" ] && [ -f "${TRIM_APPDEST}/state/datadir" ]; then
  DATA_DIR="$(cat "${TRIM_APPDEST}/state/datadir" 2>/dev/null)"
elif [ -n "${TRIM_APPDEST:-}" ] && [ -f "${TRIM_APPDEST}/${APP_ID}/state/datadir" ]; then
  DATA_DIR="$(cat "${TRIM_APPDEST}/${APP_ID}/state/datadir" 2>/dev/null)"
fi
[ -z "$DATA_DIR" ] && DATA_DIR="/vol1/@appdata/${APP_ID}/data"
[ -d "$DATA_DIR" ] || exit 0   # 没有数据可保，直接放行

# 3. 复制到留存目录（排除日志与临时文件）
RETAIN="/vol1/@appdata/${APP_ID}-keep"
if [ ! -d "$RETAIN" ]; then
  if ! mkdir -p "$RETAIN" 2>/dev/null; then
    RETAIN="$DATA_DIR/../${APP_ID}-keep"
    mkdir -p "$RETAIN" 2>/dev/null || RETAIN="/vol1/${APP_ID}-keep"
    mkdir -p "$RETAIN" 2>/dev/null || { echo "no writable retain dir" >&2; exit 0; }
  fi
fi
tar -C "$DATA_DIR" --exclude='app.log' --exclude='*.tmp' -cf - . 2>/dev/null | tar -C "$RETAIN" -xf - 2>/dev/null

exit 0
'''

TRIVIAL = "#!/bin/bash\nexit 0\n"

CHANGELOG = (
    "1.0.27→1.0.28：控制台「导出 RawCapture 分析」一键出报告。"
    "把 tools/analyze_captures.py 的字段盘点逻辑移植进网关二进制（internal/web/capture_analysis.go），"
    "新增 GET /api/export/capture-analysis 端点：实时读取 data/raw_capture/*.json，"
    "递归盘点每字段路径/类型/频次/取值分布/示例，并给出模态与规模画像，渲染成可直接打开/另存的 HTML 报告。"
    "控制台「迁移」卡片新增「导出 RawCapture 分析」按钮（新标签页打开）。"
    "相比 Python 版修复了数组元素结构不展开的缺陷（content 数组内的 type/text/image_url 多模态块现在能正确盘点）；"
    "新增 TestCapAnalyzeRealShapes 锁死盘点不变量与疑似凭证脱敏。"
    "1.0.25→1.0.26：strip_content_policy 默认开启——网关默认在转发前剥离客户端注入的 "
    "<content_policy> 段以省 token（高轮次会话每轮重复、约 100~400 词），对 <user_query> "
    "真实输入与其余字段零改动；如需保留原始正文送上游可手动关闭（setopt strip_content_policy off）。"
    "1.0.24→1.0.25：修复 gzip 默认开启（v1.0.23）引入的测试桩回归，并补效率/省 token 项。"
    "① 修正 mock 测试桩读上游请求体不解压 gzip 的问题（web 包一批 e2e 因此 400，现全绿）；"
    "② IsAutoModel 补认识默认统一模型名 agnes-auto（此前被当显式模型名原样透传上游致 400，"
    "与 config.AutoModelName 默认值及控制台聊天页默认名自相矛盾）；"
    "③ 新增可开关的 strip_content_policy（默认关）：转发前纯字符串剥离客户端注入的 "
    "<content_policy> 段，省 token 而非省速度（~0.1ms/请求，远轻于 gzip），绝不触碰 user_query 真实输入；"
    "④ 上游连接池 idle 超时 90s→5min，减少 agentic 编码任务两轮间隙连接被回收后的 TLS 重建成本；"
    "⑤ 清理 relay.Do 循环后不可达死代码与冗余 last 变量，go vet 归零。"
    "另澄清：P0 前缀缓存 addAnthropicCacheControl 早已有 opts.Anthropic 守卫，agnes 主路径（OpenAI 风格）"
    "本不跑其 JSON round-trip，无浪费，本版未改动。"
    "1.0.23→1.0.24：失败请求可观测性——usage.jsonl 新增 phase=error 记录。"
    "此前只记成功请求，「502 上游连接失败」类故障在网关侧完全无痕；现于全部失败路径"
    "（非流式/流式 4xx5xx、relay 内部错误、流式心跳路径）落错误日志（含 status、错误摘要、"
    "命中账号、attempts），流式失败处原误记 phase=done 亦修正为 error。"
    "1.0.22→1.0.23：按用户要求彻底移除「上游上下文策略（系统提示词）」模式（forward/strip/override/scenario）——"
    "后台控制台不再提供该模式选择卡片，网关对 WorkBuddy 提交的内容一律原样转发（不再改写/剥离 system）；"
    "同时把 #29 的效率优化定为默认开启：发往上游的请求体默认 gzip 压缩（upstream_request_gzip 默认 true，"
    "已验证上游 agnes 兼容解压 gzip 请求体，个别不兼容上游可手动关闭）；"
    "前缀缓存（anthropic_prompt_cache）仍为可开关项。"
    "1.0.21→1.0.22：网关执行效率优化（#29，内容零改动、保留 WorkBuddy 绝大部分提交内容）——新增两项可开关的传输/缓存增强："
    "P0 前缀缓存（anthropic_prompt_cache，Anthropic 路径顶层 system 注入 cache_control:ephemeral，让上游对反复出现的巨大静态前缀做前缀缓存、避免每轮重算 prefill）；"
    "P1 请求体 gzip（upstream_request_gzip，对发往上游的 ~500KB 请求体做 gzip 压缩，削减网关→上游带宽到 1/5~1/8）；"
    "两项默认均关闭，由运维在确认上游兼容后开启（gzip 需上游支持解压 gzip 请求体，否则会 400）；"
    "同时清理 v1.0.21 在 forward 下为死代码的 <content_policy> 全局剥离实现（线上未生效，且与「保留内容」方向冲突）。"
    "1.0.20→1.0.21：按用户要求全局去除 system 提示词中的 <content_policy> 区块——无论何种策略（forward 即生效），进入 ApplySystemPromptPolicy 先剥离该区块，其余区块与 tools/thinking/stream 等能力字段原样保留；"
    "停用 override 等其它精简模式（部署切回 forward），仅做此最小化剥离。"
    "1.0.20：修复控制台用量日志「查看请求」弹窗显示内容——此前直接展示最后一条 user 消息的完整原文（含 WorkBuddy 注入的 <system-reminder> 等多层系统开销）；"
    "现落盘前用 intent.CleanUserMessage 剥离 system-reminder、只取 <user_query> 内的真实用户输入，普通 chat 请求（无包裹）原样透传、零副作用；弹窗因此只显示用户消息本身。"
    "1.0.19：新增「上游上下文策略（系统提示词）」——控制发给上游模型的 system 提示词如何处理，且【只动 system 内容，工具调用与思考能力始终保留】；"
    "默认 forward（原样转发，零回归）；可选 仅剥离 system（最省 token）/ 覆盖为自定义提示词 / 按情景匹配前置提示词；"
    "override 与 scenario 为空时回退内置「代码生成」默认提示词（面向在 WorkBuddy 上写代码优化）；控制台设置页新增对应卡片（模式下拉 + 自定义提示词框）；"
    "用量日志「用户请求」列改为记录用户真正提交的原文（decision.Prompt.Text，已排除 system/assistant/tool 上下文，不再把整段系统提示词当作用户请求）；"
    "修复 requestLog 截断阈值长期硬编码 8192、导致 UsageRequestLogBytes 配置项形同虚设的问题，现接入配置（默认 512）。"
    "1.0.18：修复控制台用量日志「查看请求」弹窗打不开的前端回归（点按钮弹窗不出现、控制台报 Uncaught ReferenceError: trunc is not defined）；"
    "根因为模板表达式误引用了未定义变量 trunc（应为 truncated），现已对齐；"
    "延续 v1.0.17：用量日志「用户请求」列改为只保留一个「查看请求」按钮（点击才弹完整请求原文）。"
    "1.0.17：上游 429「达到最大限制/限流」不再直接透传给客户端中断任务——网关内部自动换号 + 重新排队 + 感知 Retry-After 退避重试，直到成功或耗尽预算（次数/总等待），客户端在途任务不被打断；"
    "429 自动重试预算随情景走：挂机批量最多最久、写代码(WorkBuddy)偏高、生图保守（非幂等防重复扣费）；"
    "新增「自动检测接入方负载 → 一键匹配推荐情景」：后台按实时负载画像（文/图/视频占比、流式比、并发、429 压力）自动切换最贴合情景，控制台可一键开启/关闭、按推荐应用；"
    "新增 WorkBuddy 写代码专用情景预设；"
    "用量日志「用户请求」列新增悬停浮层（可滚动看完整请求原文，替代渲染不全的原生提示）；移除顶部强制改密横幅，修正两个开关项的排版错位。"
    "1.0.16：修复控制台日志页「用户输入(user_request)」不显示的回归（1.0.15 起异步批量落盘后，刚发完的请求要等 30s 才上盘、日志页读不到；现日志页实时合并尚未落盘的内存记录）；"
    "补漏粘性绑定 Bind 的热路径同步写盘（此前每个带会话头的请求都全量写 bindings.json，现改为内存 + 30s 批量落盘，与密钥记账/用量日志同口径，多任务并发尾延迟进一步下降）；"
    "修正控制台「查看全部版本」链接指向（baipiao-hub → agnes-hub-go）。"
    "1.0.15：新增「运行情景模式」——控制台设置页可选 代码/图片/自动/多任务并行/批处理 等情景一键调参（安全系数/文本 RPM/重试/排队/并发等随情景联动，账号级 RPM 覆盖仍优先），热重载即时生效；"
    "热路径异步落盘（密钥记账与用量日志改内存累加 + 维护循环 30s 批量写盘，消除每请求同步磁盘 I/O，多任务高频并发尾延迟显著下降）；"
    "上游 429 透传自动补 Retry-After（按池投影等待计算，免费号无头时也能给客户端可重试间隔），/healthz 新增 text_projected_wait_ms 排队可观测；"
    "文本池两级队列（多任务并行情景默认开启）：非流式工具/短调用优先于流式长回答拿到节拍槽位，RPM 安全节拍不变。"
    "1.0.14：挂机鲁棒性专项——流式空闲看门狗（长回答不断流）、全进程 panic 恢复、用量日志滚动截断防撑盘、优雅停机排空在途、429 单 pacer reload、/healthz 富健康度与 /metrics 4 项新指标、连接池随账号数自适应、视频轮询指数退避、日志「完整」请求查看弹窗。"
    "1.0.13：修复文本池日志「用户提出的完整请求」列恒为空的问题（此前只给媒体端点填了请求体，chat/text 路径漏填，导致日志页看不到用户发送的原始请求）；"
    "现所有路径（文本/生图/生视频，流式与非流式）的 done 记录均携带用户完整请求（最长 8KB，自动截断）。"
    "1.0.12：agnes 免费档文本池标称 RPM 由 20 降至 10，并新增「agnes 免费档文本 RPM」可配置项（控制台设置页，修改立即生效，per-account rpm_overrides 仍优先）；"
    "用量日志新增「用户提出的完整请求」字段（可在日志页查看客户端发送的原始内容）；"
    "新增下游密钥 token 用量统计（按 key 累计/按天滚动，密钥列表页展示今日/累计 token）。"
    "1.0.11：控制台账号编辑改为完整弹窗（可改名称/Key/BaseURL/类型/分组/并发/优先级/兜底模型/各模态清单/各池 RPM）；"
    "新增「账号调用优先级」与「按账号 RPM 覆盖」（不再统一套用 agnes 20rpm），调度按优先级、区域、预计等待排序；"
    "/api/models 与 /chat 拉模型清单合并各账号声明模型（AMD/OpenRouter 等非 agnes 渠道真实模型可直接在对话/生图/生视频下拉选择并测试连通性）；"
    "账号测试改用该账号自身声明模型探测；修复 AMD 经网关返回空体（上游响应体被提前关闭）的问题；修复软粘性会话绑定不校验请求模型、会把 agnes 请求错发给 AMD 账号的缺陷。"
    "1.0.10：新增 AMD（developer.amd.com.cn/radeon）与 OpenRouter 模型接口、Prometheus /metrics 监控、熔断冷却延长、兜底模型、修复添加 AMD 时 null .value 报错。"
    "1.0.5：控制台新增「到达密度 vs 文本池节拍」观测指标（判定多账号是否真正被吃到）；"
    "意图判定 LRU 缓存（热路径 O(1)，规则变更静默失效）；"
    "429 路径改批量落盘（消除同步磁盘 I/O 拖慢换号重试）；"
    "HTTP 客户端单例化（全应用共享连接池，生图/视频轮询建连开销下降）。"
    "1.0.4：飞牛 fnOS 重装/升级/卸载时完整保留账号、设置、聊天记录；"
    "安装向导的管理员密码填写后覆盖原密码、留空则保留原密码；更换品牌 logo；"
    "修复 cmd/main 在部分 fnOS 版本下找不到二进制的问题。"
    "1.0.3：新增聊天对话记录持久化（服务端存储、侧栏可查看/删除/清空、点击回溯历史对话）；"
    "文字模型默认改为 agnes-3.0-flash 优先；飞牛端安装后生成桌面快捷方式（点击打开控制台）。"
    "1.0.1 自更新：内置 GitHub Releases 版本检查与一键更新（SHA256 + 可执行文件魔数双校验）；"
    "Windows 由助手进程在旧进程退出后完成替换并自动重启；修复替换脚本在进程存活时执行导致更新静默失效的问题。"
    "1.0.0 首发：多账号聚合中转、agnes-auto 三模态自动路由、FIFO 严格节拍限流、"
    "软粘性溢出、(账号*池)二维自适应校准、401/403/402 熔断自动复活、生图/视频非幂等不重试。"
)


def _write(path, content, mode=0o755):
    with open(path, "w", encoding="utf-8", newline="\n") as f:
        f.write(content)
    os.chmod(path, mode)


def prepare():
    for d in (APP_DIR, CMD_DIR, WIZARD_DIR, CONFIG_DIR):
        os.makedirs(d, exist_ok=True)

    # 1. 二进制
    for src, dst in (("baipiao-hub-linux-amd64", "baipiao-hub"),
                     ("baipiao-hub-linux-arm64", "baipiao-hub-arm64")):
        s = os.path.join(ROOT, src)
        if not os.path.exists(s):
            sys.exit("[ERROR] 缺少交叉编译产物 %s，请先执行 build_linux.sh 或 go build" % src)
        shutil.copy2(s, os.path.join(APP_DIR, dst))

    # 2. cmd 生命周期脚本
    _write(os.path.join(CMD_DIR, "main"), MAIN_SCRIPT.replace("%PORT%", str(SERVICE_PORT)))
    _write(os.path.join(CMD_DIR, "upgrade_init"), UPGRADE_INIT.replace("%VERSION_TAG%", VERSION_TAG))
    _write(os.path.join(CMD_DIR, "install_init"), INSTALL_INIT.replace("%VERSION_TAG%", VERSION_TAG))
    _write(os.path.join(CMD_DIR, "uninstall_init"), UNINSTALL_INIT)
    for name in ("upgrade_callback", "install_callback", "config_callback",
                 "uninstall_callback"):
        _write(os.path.join(CMD_DIR, name), TRIVIAL)
    _write(os.path.join(CMD_DIR, "config_init"),
           "#!/bin/bash\npkill -f 'baipiao-hub' 2>/dev/null || true\nexit 0\n")

    # 3. wizard（必须是非空数组，且 items 不能为空——fnOS 会报 "wizard items is empty"）
    # 注意：必须有 password 类型字段，否则校验失败（code 10150）
    # 参考 M365/Copilot2API 和 jdbeanbot 的 wizard/install 格式
    wizard_content = json.dumps([{
        "stepTitle": "白嫖 Hub 配置",
        "items": [
            {
                "type": "password",
                "field": "wizard_admin_password",
                "label": "管理员密码",
                "helpText": "设置网页管理后台的管理员密码。安装时若填写，则会将管理员密码设为此值；重新安装时若留空，则保留原有管理员密码。忘记密码时可重新安装并填写新密码覆盖。"
            },
            {
                "type": "tips",
                "helpText": "服务端口固定为 4142（无需填写）：安装完成后通过飞牛桌面图标或浏览器访问 http://NAS的IP:4142 进入管理页，添加 AI 账号并生成 API Key。账号与配置保存在应用数据目录，重装/卸载时自动保留。"
            }
        ]
    }], ensure_ascii=False, indent=2)
    with open(os.path.join(WIZARD_DIR, "install"), "w", encoding="utf-8") as f:
        f.write(wizard_content)

    # 4. config —— 缺失会让应用中心拒绝安装，容易漏
    # 注意：resource 必须输出 `{\\n}`（带换行），不能是 `{}`，否则 fnOS 校验失败
    with open(os.path.join(CONFIG_DIR, "privilege"), "w", encoding="utf-8") as f:
        json.dump({"defaults": {"run-as": "package"},
                   "username": APP_ID, "groupname": APP_ID}, f, ensure_ascii=False, indent=2)
        f.write("\n")
    with open(os.path.join(CONFIG_DIR, "resource"), "w", encoding="utf-8") as f:
        json.dump({}, f, ensure_ascii=False)
        f.write("\n")

    # 5. manifest —— 必须按 fnOS 官方可装包（如 M365-Copilot2API-FNOS）的格式：
    #    key 右填满 22 字符 + 空格 + "=" + 空格 + 值。裸 "key=value" 会让
    #    appcenter 解析失败，报 code 10111。
    #    注意：必须包含 desktop_uidir 和 desktop_applaunchname 字段（即使服务无UI），
    #    否则 appcenter 解析时可能报错。
    KEY_WIDTH = 22
    field_pairs = [
        ("appname", APP_ID),
        ("version", VERSION),
        ("display_name", "白嫖 Hub"),
        ("desc", "白嫖 Hub：免费 API 资源聚合中转网关，支持 agnes / AMD / NVIDIA / M365 / OpenRouter 等多渠道账号统一调度与 RPM 限流排队；统一模型 agnes-auto 自动判定文生/生图/生视频；"
                  "FIFO 严格节拍、软粘性溢出、二维自适应校准、熔断自动复活。内置 /chat 网页对话 UI（对话记录持久化、agnes-3.0-flash 默认模型）；飞牛端安装后生成桌面快捷方式，点击打开控制台。"),
        ("source", "thirdparty"),
        ("platform", "x86"),
        ("arch", "x86_64"),
        ("maintainer", "my788525"),
        ("maintainer_url", "https://github.com/my788525/agnes-hub-go"),
        ("os_min_version", "0.9.0"),
        ("desktop_uidir", "ui"),
        ("desktop_applaunchname", "agnes-hub.main"),
        ("service_port", str(SERVICE_PORT)),
        ("checkport", "false"),
        ("ctl_stop", "true"),
        ("changelog", CHANGELOG),
    ]

    lines = ["%s = %s" % (k.ljust(22), v) for k, v in field_pairs]
    # fnOS manifest 必须使用 CRLF 行尾（M365 官方包均为 CRLF）
    # 注意：不要加注释行（jdbeanbot 的成功包就没有），注释行可能干扰解析
    with open(os.path.join(FPK_DIR, "manifest"), "wb") as f:
        f.write(("\r\n".join(lines) + "\r\n").encode("utf-8"))

    # 6. 图标（源文件提交在 assets/）
    for name in ("ICON.PNG", "ICON_256.PNG"):
        src = os.path.join(ROOT, "assets", name)
        if os.path.exists(src):
            shutil.copy2(src, os.path.join(FPK_DIR, name))
        else:
            _placeholder_png(os.path.join(FPK_DIR, name), 64 if name == "ICON.PNG" else 256)

    # 6.5 桌面快捷方式（ui 目录）：fnOS 安装后于桌面生成图标，点击打开 /console
    # 结构必须对齐 M365 官方可装 fpk：ui/ 平铺在 app.tgz 顶层（fnOS 套 <app_id>/ 后为
    # <app_id>/ui/config）；ui/config 为 JSON，icon 用 images/icon-{0}.png 占位。
    ui_dir = os.path.join(APP_DIR, "ui")
    ui_img_dir = os.path.join(ui_dir, "images")
    os.makedirs(ui_img_dir, exist_ok=True)
    ui_config = {
        ".url": {
            "agnes-hub.main": {
                "title": "Agnes Hub",
                "icon": "images/icon-{0}.png",
                "type": "url",
                "protocol": "http",
                "port": str(SERVICE_PORT),
                "url": "/console",
                "allUsers": True,
            }
        }
    }
    with open(os.path.join(ui_dir, "config"), "w", encoding="utf-8") as f:
        json.dump(ui_config, f, ensure_ascii=False, indent=4)
        f.write("\n")
    _make_square_icon(os.path.join(ROOT, "assets", "ICON_256.PNG"),
                      os.path.join(ui_img_dir, "icon-256.png"), 256)
    _make_square_icon(os.path.join(ROOT, "assets", "ICON_256.PNG"),
                      os.path.join(ui_img_dir, "icon-64.png"), 64)


def _placeholder_png(path, size):
    import zlib
    sig = b"\x89PNG\r\n\x1a\n"

    def chunk(tag, data):
        body = tag + data
        return struct.pack(">I", len(data)) + body + struct.pack(">I", zlib.crc32(body) & 0xFFFFFFFF)

    ihdr = chunk(b"IHDR", struct.pack(">IIBBBBB", size, size, 8, 2, 0, 0, 0))
    raw = b"".join(b"\x00" + b"\xff\xff\xff" * size for _ in range(size))
    with open(path, "wb") as f:
        f.write(sig + ihdr + chunk(b"IDAT", zlib.compress(raw)) + chunk(b"IEND", b""))


def _make_square_icon(src, dst, size):
    """从源图正方形裁剪到 size×size；缺 Pillow 时退回原图拷贝。"""
    try:
        from PIL import Image
        im = Image.open(src).convert("RGBA")
        w, h = im.size
        side = min(w, h)
        box = ((w - side) // 2, (h - side) // 2, (w + side) // 2, (h + side) // 2)
        im.crop(box).resize((size, size), Image.LANCZOS).save(dst)
    except Exception:
        if os.path.exists(src):
            shutil.copy2(src, dst)


def build_inner():
    """内层 app.tgz：对齐已验证可装的 1.0.0 fpk 内层结构。

    实测 fnOS 会把 app.tgz 内容解压到 /var/apps/<app_id>/ 并再套一层 <app_id>/，
    因此内部路径要这样排布（参考 1.0.0 实际产物）：
    - 二进制放在 app/ 下        、 最终 <app_id>/app/<bin>，cmd/main 的 $APP_DIR/app/$BIN_NAME 命中
    - cmd/、wizard/、config/、ui/ 平铺在 app.tgz 顶层
      （ui/ 由 fnOS 套 <app_id>/ 后变成 <app_id>/ui，桌面快捷方式据此读取）
    """
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w:gz", compresslevel=9,
                      format=tarfile.GNU_FORMAT) as tar:
        # app/ 目录条目（二进制所在）
        ti = tarfile.TarInfo(name="app/")
        ti.type = tarfile.DIRTYPE
        ti.mode = 0o755
        ti.uid = ti.gid = 0
        ti.uname = ti.gname = "root"
        ti.mtime = 0
        tar.addfile(ti)

        # 二进制放在 app/ 下
        for src_name, dst_name in (("baipiao-hub-linux-amd64", "baipiao-hub"),
                                    ("baipiao-hub-linux-arm64", "baipiao-hub-arm64")):
            src = os.path.join(ROOT, src_name)
            if not os.path.exists(src):
                sys.exit("[ERROR] 缺少交叉编译产物 %s，请先执行 build_linux.sh 或 go build" % src_name)
            _add_file(tar, src, "app/" + dst_name, mode=0o755)

        # cmd/、wizard/、config/ 平铺在顶层
        for d in ("config", "cmd", "wizard"):
            full = os.path.join(FPK_DIR, d)
            if os.path.isdir(full):
                _add_dir_recursive(tar, full, d)

        # 桌面快捷方式 ui/ 平铺在顶层（fnOS 套 <app_id>/ 后为 <app_id>/ui）
        ui_full = os.path.join(APP_DIR, "ui")
        if os.path.isdir(ui_full):
            _add_dir_recursive(tar, ui_full, "ui")

    data = buf.getvalue()
    md5 = hashlib.md5(data).hexdigest()
    return data, md5


def _add_file(outer, full, arc, mode=0o644):
    ti = outer.gettarinfo(full, arcname=arc)
    ti.uid = ti.gid = 0
    ti.uname = ti.gname = "root"
    ti.mtime = 0
    ti.mode = mode
    with open(full, "rb") as fh:
        outer.addfile(ti, fh)


def _add_dir_recursive(outer, full, arc):
    """把目录递归加进外层 tar，脚本统一 0755、其余 0644。"""
    ti = tarfile.TarInfo(name=arc)
    ti.type = tarfile.DIRTYPE
    ti.mode = 0o755
    ti.uid = ti.gid = 0
    ti.uname = ti.gname = "root"
    ti.mtime = 0
    outer.addfile(ti)
    for child in sorted(os.listdir(full)):
        cfull = os.path.join(full, child)
        carc = arc + "/" + child
        if os.path.isdir(cfull):
            _add_dir_recursive(outer, cfull, carc)
        else:
            mode = 0o755 if (carc.startswith("cmd/") or os.access(cfull, os.X_OK)) else 0o644
            _add_file(outer, cfull, carc, mode)


def build_outer(app_data, md5):
    os.makedirs(OUT_DIR, exist_ok=True)
    out = os.path.join(OUT_DIR, FPK_NAME)
    with open(out, "wb") as fout:
        with tarfile.open(fileobj=fout, mode="w:gz", compresslevel=9,
                          format=tarfile.GNU_FORMAT) as outer:
            # 顺序对齐已知可装的 fnOS fpk（M365-Copilot2API-FNOS）：
            # manifest 、 cmd 、 config 、 wizard 、 icons 、 app.tgz
            # （不单独带 manifest.checksum 文件；完整性由 manifest 内的 checksum= 字段保证）
            # manifest 内每条 key 都右填满 22 字符，对齐 " = " 分隔列。
            _add_file(outer, os.path.join(FPK_DIR, "manifest"), "manifest")
            for d in ("cmd", "config", "wizard"):
                _add_dir_recursive(outer, os.path.join(FPK_DIR, d), d)
            for name in ("ICON.PNG", "ICON_256.PNG"):
                _add_file(outer, os.path.join(FPK_DIR, name), name)

            ti = tarfile.TarInfo(name="app.tgz")
            ti.size = len(app_data)
            ti.uid = ti.gid = 0
            ti.uname = ti.gname = "root"
            ti.mtime = 0
            ti.mode = 0o644
            outer.addfile(ti, io.BytesIO(app_data))
    return out


def _stamp_manifest_checksum(md5):
    """往 manifest 末尾补 checksum= 字段（对齐 fnOS 官方参考 fpk）。
    注意：不要额外写独立的 manifest.checksum 文件——真机可装的官方 fpk
    （m365-copilot2api-1.6.29.fpk 同机实测）外层没有该文件，只靠 manifest 内
    checksum=<app.tgz 的 MD5> 字段；多带一个反而是安装失败的变量。"""
    mp = os.path.join(FPK_DIR, "manifest")
    with open(mp, "r", encoding="utf-8") as f:
        lines = f.read().splitlines()
    if not any(l.split("=", 1)[0].strip() == "checksum" for l in lines):
        lines.append("%s = %s" % ("checksum".ljust(22), md5))
    with open(mp, "w", encoding="utf-8", newline="\r\n") as f:
        f.write("\n".join(lines) + "\n")
    # 清掉历史遗留的 manifest.checksum（若存在），避免被打进外层 tar
    legacy = os.path.join(FPK_DIR, "manifest.checksum")
    if os.path.exists(legacy):
        os.remove(legacy)


def main():
    prepare()
    app_data, md5 = build_inner()
    _stamp_manifest_checksum(md5)
    out = build_outer(app_data, md5)
    size = os.path.getsize(out)
    print("内层 app.tgz : %d bytes  md5=%s" % (len(app_data), md5))
    print("fpk 产物     : %s" % out)
    print("体积         : %.1f MB" % (size / 1024 / 1024))
    print()
    print("安装（需 sudo）：appcenter-cli install-fpk %s" % out)


if __name__ == "__main__":
    main()
