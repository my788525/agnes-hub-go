# -*- coding: utf-8 -*-
"""幂等补传资产到当前 main.go 版本对应的 GitHub Release。

解决的问题：release.py 上传资产时无重试，经代理走 uploads.github.com 时
SSL 瞬断（SSLEOFError / UNEXPECTED_EOF）会让发版卡在中途，已传的部分留在
Release 里、缺的没传。本脚本可安全重跑：

    GITHUB_PAT=ghp_xxx python tools/publish_assets.py

行为：
- 版本从 main.go 单一来源读取；
- GET 已建好的 Release（若不存在则提示先跑 release.py 建 Release+tag）；
- 查已传资产清单，只补「尚未存在」的资产（已存在则跳过，重跑不会 409）；
- 每个资产 5 次重试，容忍 SSL 瞬断 / 连接错误 / 超时。

产物路径约定（与 release.py 一致）：
    dist/baipiao-hub-{v}.fpk, dist/agnes-hub-go-windows-{v}.zip,
    baipiao-hub-linux-amd64, baipiao-hub-linux-arm64, agnes-hub-go.exe
"""
import os
import re
import sys
import time

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DIST = os.path.join(ROOT, "dist")


def version_from_main_go() -> str:
    with open(os.path.join(ROOT, "main.go"), "r", encoding="utf-8") as f:
        src = f.read()
    m = re.search(r'var\s+version\s*=\s*"(\d+\.\d+\.\d+)"', src)
    if not m:
        sys.exit("[FATAL] 无法从 main.go 解析版本号")
    return m.group(1)


def main():
    import requests

    PAT = os.environ.get("GITHUB_PAT", "")
    if not PAT:
        sys.exit("[FATAL] 需要 GITHUB_PAT 环境变量")
    PROXY = "http://127.0.0.1:3067"
    REPO = "my788525/agnes-hub-go"
    v = version_from_main_go()
    print(f"[publish_assets] 目标 v{v}（来自 main.go）")

    H = {"Authorization": f"Bearer {PAT}",
         "Accept": "application/vnd.github+json",
         "X-GitHub-Api-Version": "2022-11-28"}
    PX = {"https": PROXY, "http": PROXY}

    def get_release():
        for i in range(6):
            try:
                r = requests.get(f"https://api.github.com/repos/{REPO}/releases/tags/v{v}",
                                 headers=H, proxies=PX, timeout=30)
                if r.status_code == 200:
                    return r.json()
                if r.status_code == 404:
                    print(f"  [404] Release v{v} 不存在 —— 请先跑 tools/release.py 建 Release+tag")
                    sys.exit(2)
                print("  release status", r.status_code, r.text[:120])
            except Exception as e:
                print(f"  release GET retry {i + 1}: {type(e).__name__}")
            time.sleep(3)
        sys.exit("[FATAL] 取不到 release，放弃")

    def existing(rel):
        return {a["name"] for a in rel.get("assets", [])}

    def upload(rel_id, path, name, ctype):
        with open(path, "rb") as f:
            data = f.read()
        url = f"https://uploads.github.com/repos/{REPO}/releases/{rel_id}/assets?name={name}"
        for i in range(5):
            try:
                ur = requests.post(url, headers={**H, "Content-Type": ctype},
                                   proxies=PX, data=data, timeout=300)
                if ur.status_code < 300:
                    print(f"  + {name} ({len(data)} bytes) [{ur.status_code}]")
                    return True
                print(f"  {name} upload {ur.status_code}: {ur.text[:160]}")
            except Exception as e:
                print(f"  {name} upload retry {i + 1}: {type(e).__name__} {str(e)[:90]}")
            time.sleep(4)
        return False

    rel = get_release()
    rel_id = rel["id"]
    have = existing(rel)
    print(f"[release] id={rel_id} 已传 {len(have)} 件：{sorted(have)}")

    assets = [
        ("baipiao-hub-%s.fpk" % v, os.path.join(DIST, "baipiao-hub-%s.fpk" % v), "application/octet-stream"),
        ("baipiao-hub-linux-amd64", os.path.join(ROOT, "baipiao-hub-linux-amd64"), "application/octet-stream"),
        ("baipiao-hub-linux-arm64", os.path.join(ROOT, "baipiao-hub-linux-arm64"), "application/octet-stream"),
        ("agnes-hub-go-windows-%s.zip" % v, os.path.join(DIST, "agnes-hub-go-windows-%s.zip" % v), "application/zip"),
        ("agnes-hub-go.exe", os.path.join(ROOT, "agnes-hub-go.exe"), "application/octet-stream"),
    ]

    ok = True
    for name, path, ctype in assets:
        if name in have:
            print(f"  = {name} 已存在，跳过")
            continue
        if not os.path.exists(path):
            print(f"  [MISSING LOCAL] {path}")
            ok = False
            continue
        if not upload(rel_id, path, name, ctype):
            ok = False
            print(f"  [FAIL] {name}")

    rel2 = get_release()
    final = sorted(existing(rel2))
    print(f"[final] 现有 {len(final)} 件：{final}")
    if ok and len(final) == 5:
        print(f"[done] {rel2['html_url']}  5/5 资产齐全")
    else:
        print("[incomplete] 仍有缺失，可重跑本脚本继续补传")
        sys.exit(1)


if __name__ == "__main__":
    main()
