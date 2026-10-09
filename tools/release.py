#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""agences-hub-go 一键发版脚本。

用法：
    python tools/release.py            # 完整发版（编译 + 打包 + tag + GitHub Release）
    python tools/release.py --no-push  # 只做本地产物，不推 tag 与 Release

设计要点：
- **版本单一来源**：从 main.go 的 `var version = "x.y.z"` 读取，
  不再手改 build_fpk.py / manifest / tag 多处，杜绝版本漂移。
- **前置校验**：编译出的二进制嵌入版本必须等于 main.go 的 version，
  不一致就终止（防止再发旧版，如 1.0.3 的 windows exe 误发事故）。
- **产物 5 件**（对齐历史 Release 结构）：
    fpk + linux-amd64 + linux-arm64 + windows-zip + 裸 windows exe
- **GitHub 经 REST API**（本机无 gh CLI）：
    用 PAT + 代理建 Release、传资源。

前置条件：
    1. Go 工具链（goroot/gopath/goproxy 见 MEMORY）
    2. dist/README.txt 与 agnes-hub-go.bat 存在
    3. 代理 127.0.0.1:3067 在监听
    4. GITHUB_PAT 环境变量或默认 PAT 可用
"""
import os
import re
import sys
import subprocess
import zipfile
import shutil

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DIST = os.path.join(ROOT, "dist")
GO = os.path.expandvars(r"C:\Users\pguoy\go\bin\go.exe")
GOROOT = os.path.expandvars(r"C:\Users\pguoy\go")
GOPATH = os.path.expandvars(r"C:\Users\pguoy\gopath")
GOPROXY = "https://goproxy.cn,direct"
GITHUB_PAT = os.environ.get("GITHUB_PAT", "")  # 必须经环境变量传入，绝不入 git
PROXY = "http://127.0.0.1:3067"
REPO = "my788525/agnes-hub-go"


def version_from_main_go() -> str:
    """从 main.go 读取 `var version = "x.y.z"`。"""
    main_go = os.path.join(ROOT, "main.go")
    with open(main_go, "r", encoding="utf-8") as f:
        src = f.read()
    m = re.search(r'var\s+version\s*=\s*"(\d+\.\d+\.\d+)"', src)
    if not m:
        sys.exit("[FATAL] 无法从 main.go 解析版本号")
    return m.group(1)


def go_build(env, goos, goarch, out):
    """交叉编译单个目标。"""
    args = [GO, "build", "-ldflags", "-s -w", "-o", out, "."]
    cmd_env = {**os.environ, **env, "GOOS": goos, "GOARCH": goarch}
    print(f"  building {goos}/{goarch} -> {os.path.basename(out)}")
    subprocess.run(args, cwd=ROOT, env=cmd_env, check=True)


def embedded_version(binary_path: str) -> set:
    """提取二进制里嵌入的版本字符串（用于前置校验）。"""
    with open(binary_path, "rb") as f:
        data = f.read()
    return set(m.decode() for m in re.findall(rb"1\.0\.\d+", data))


def check_version(binary_path: str, expected: str):
    """前置校验：二进制嵌入版本必须包含 expected。"""
    vers = embedded_version(binary_path)
    if expected not in vers:
        print(f"  [ERROR] {os.path.basename(binary_path)} 嵌入版本 {vers} 不含 {expected}")
        sys.exit("[FATAL] 版本校验失败 —— 请确认 main.go 已 bump 且重新编译")
    print(f"  [OK] {os.path.basename(binary_path)} 嵌入版本 {expected}")


def build_zip(exe, bat, readme, out):
    if os.path.exists(out):
        os.remove(out)
    with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as z:
        z.write(exe, "agnes-hub-go.exe")
        z.write(bat, "agnes-hub-go.bat")
        z.write(readme, "README.txt")
    print(f"  zip  {out} ({os.path.getsize(out)} bytes)")


def build_fpk(version: str) -> str:
    """调用 build_fpk.py 生成 fpk。

    build_fpk.py 的版本号已改为从 main.go 单一来源读取（_version_from_main_go），
    无需再临时 sed 替换 VERSION/VERSION_TAG。产出文件名现为 baipiao-hub-{version}.fpk。
    返回 fpk 的完整路径，供上层组装 Release 资源列表。
    """
    bf = os.path.join(ROOT, "tools", "build_fpk.py")
    subprocess.run([sys.executable, bf], cwd=ROOT, check=True)
    fpk = os.path.join(DIST, f"baipiao-hub-{version}.fpk")
    if not os.path.exists(fpk):
        sys.exit(f"[FATAL] 未生成 {fpk}")
    print(f"  fpk  {fpk} ({os.path.getsize(fpk)} bytes)")
    return fpk


def git_push_tag(version: str):
    tag = f"v{version}"
    cmd = ["git", "tag", "-a", tag, "-m", f"v{version}"]
    subprocess.run(cmd, cwd=ROOT, check=True)
    print(f"  git tag {tag}")
    push = subprocess.run(
        ["git", "push", f"https://{GITHUB_PAT}@github.com/{REPO}.git", tag],
        cwd=ROOT,
        env={**os.environ, "HTTPS_PROXY": PROXY, "HTTP_PROXY": PROXY,
             "https_proxy": PROXY, "http_proxy": PROXY},
        capture_output=True, text=True,
    )
    if push.returncode != 0:
        print(f"  [WARN] git push tag 失败: {push.stderr.strip()}")


def github_release(version: str, assets):
    """经 REST API 建 Release + 上传资源。"""
    import requests
    h = {"Authorization": f"Bearer {GITHUB_PAT}",
         "Accept": "application/vnd.github+json",
         "X-GitHub-Api-Version": "2022-11-28"}
    proxies = {"https": PROXY, "http": PROXY}

    # 删除已存在同名 tag 的旧 release（重发场景）
    r0 = requests.get(f"https://api.github.com/repos/{REPO}/releases/tags/v{version}",
                      headers=h, proxies=proxies, timeout=30)
    if r0.status_code == 200:
        rel_id = r0.json()["id"]
        requests.delete(f"https://api.github.com/repos/{REPO}/releases/{rel_id}",
                        headers=h, proxies=proxies, timeout=30)
        print(f"  [del] 旧 release v{version} (id={rel_id})")

    body = (
        f"## 白嫖 Hub (Agnes Hub) v{version}\n\n"
        "多账号聚合中转网关，支持 agnes / AMD / NVIDIA / M365 / OpenRouter 等渠道统一调度与 RPM 限流排队。\n\n"
        "### 本版主要变更（情景模式 + 文本热路径提速 + 两级队列）\n"
        "- **运行情景模式**：控制台设置页新增「运行情景」，可选 默认 / 代码编写 / 图片生成 / 自动判断 / 多任务并行 / 批处理 六种情景，一键应用整套调参包（安全系数、文本 RPM、重试次数与退避、排队深度与等待、图片/视频并发、流式空闲、两级队列、区域优先级随情景联动），热重载即时生效；账号级 RPM 覆盖始终优先\n"
        "- **热路径异步落盘（P0）**：密钥记账 `ChargeKey` 与用量日志 `AppendUsage` 由「每请求同步全量写盘」改为「内存累加 + 维护循环 30s 批量写盘」，多任务高频并发下消除请求 goroutine 的磁盘 I/O 尾延迟；优雅停机前自动 flush，挂机丢 ≤30s 可接受（与 429 不写盘同口径）\n"
        "- **429 Retry-After 透传（P2）**：上游 429 未带 Retry-After（免费号 agnes 常见）时，按该池投影等待自动补兜底头（1–60s），客户端据此精确退避；`/healthz` 新增 `text_projected_wait_ms`（此刻发起文本请求预计排队时长），外部探活可预判是否会堵\n"
        "- **文本池两级队列（多任务并行情景默认开）**：文本池内非流式（工具/短调用）优先于流式（长生成）领槽，只改出队顺序、不破坏 RPM 安全节拍；`Acquire` 透传 stream 标志，`TextTwoLevelQueue` 开关控制\n"
        "- 既有挂机鲁棒性能力延续（v1.0.14）：流式空闲看门狗、全进程 panic 恢复、日志滚转、优雅停机、429 单 pacer reload、/healthz 富健康度、连接池自适应、视频轮询退避\n\n"
        "### 下载\n"
        f"- `baipiao-hub-{version}.fpk` — 飞牛 fnOS 安装包（应用中心安装，应用名「白嫖 Hub」，内部 appname 保持 agnes-hub 以保留已装应用数据）\n"
        f"- `agnes-hub-go-windows-{version}.zip` — Windows 绿色版（含 exe + 启动脚本 + README）\n"
        f"- `baipiao-hub-linux-amd64` / `baipiao-hub-linux-arm64` — Linux 二进制（x86_64 / aarch64）\n"
        f"- `agnes-hub-go.exe` — Windows 裸可执行文件\n"
    )
    r = requests.post(f"https://api.github.com/repos/{REPO}/releases", headers=h,
                      proxies=proxies, json={"tag_name": f"v{version}", "name": f"v{version}",
                                             "body": body, "draft": False, "prerelease": False},
                      timeout=60)
    r.raise_for_status()
    rel = r.json()
    print(f"  release {rel['html_url']}")
    for path, name, ct in assets:
        with open(path, "rb") as f:
            data = f.read()
        ur = requests.post(
            f"https://uploads.github.com/repos/{REPO}/releases/{rel['id']}/assets?name={name}",
            headers={**h, "Content-Type": ct}, proxies=proxies, data=data, timeout=300)
        if ur.status_code >= 300:
            sys.exit(f"[FATAL] 上传 {name} 失败: {ur.status_code} {ur.text[:200]}")
        print(f"  + {name} ({len(data)} bytes)")
    print(f"  [done] {rel['html_url']}")


def main():
    import argparse
    ap = argparse.ArgumentParser()
    ap.add_argument("--no-push", action="store_true", help="只本地产物，不推 tag/Release")
    args = ap.parse_args()

    if not args.no_push and not GITHUB_PAT:
        sys.exit("[FATAL] 推 tag/Release 需要 GITHUB_PAT 环境变量：\n"
                 "    set GITHUB_PAT=ghp_xxxx   (Windows)\n"
                 "    export GITHUB_PAT=ghp_xxxx  (Linux/macOS)\n"
                 "PAT 绝不写入代码或 git，只能经环境变量传入。")

    version = version_from_main_go()
    print(f"[release] 版本 {version}（来自 main.go）")
    os.makedirs(DIST, exist_ok=True)

    go_env = {"GOROOT": GOROOT, "GOPATH": GOPATH, "GOPROXY": GOPROXY, "GOFLAGS": "-mod=mod"}

    # 1. 交叉编译
    #    注意：build_fpk.py 的 prepare() 要求 ROOT 下存在 baipiao-hub-linux-amd64 /
    #    baipiao-hub-linux-arm64（与 fnOS 内部二进制同名），故此处产出名必须与之一致。
    linux_amd64 = os.path.join(ROOT, "baipiao-hub-linux-amd64")
    linux_arm64 = os.path.join(ROOT, "baipiao-hub-linux-arm64")
    win_exe = os.path.join(ROOT, "agnes-hub-go.exe")
    go_build(go_env, "linux", "amd64", linux_amd64)
    go_build(go_env, "linux", "arm64", linux_arm64)
    go_build(go_env, "windows", "amd64", win_exe)

    # 2. 前置校验
    check_version(linux_amd64, version)
    check_version(linux_arm64, version)
    check_version(win_exe, version)

    # 3. Windows 包
    bat = os.path.join(ROOT, "agnes-hub-go.bat")
    readme = os.path.join(DIST, "README.txt")
    win_zip = os.path.join(DIST, f"agnes-hub-go-windows-{version}.zip")
    build_zip(win_exe, bat, readme, win_zip)

    # 4. fpk
    fpk = build_fpk(version)

    if args.no_push:
        print("[done --no-push] 本地产物完成，未推 tag/Release")
        return

    # 5. tag + GitHub Release
    git_push_tag(version)
    assets = [
        (fpk, f"baipiao-hub-{version}.fpk", "application/octet-stream"),
        (linux_amd64, "baipiao-hub-linux-amd64", "application/octet-stream"),
        (linux_arm64, "baipiao-hub-linux-arm64", "application/octet-stream"),
        (win_zip, f"agnes-hub-go-windows-{version}.zip", "application/zip"),
        (win_exe, "agnes-hub-go.exe", "application/octet-stream"),
    ]
    github_release(version, assets)


if __name__ == "__main__":
    main()
