# -*- coding: utf-8 -*-
"""幂等补传 v1.0.15 剩余资产到已建好的 GitHub Release。

用法：
    GITHUB_PAT=ghp_xxx python publish_1015.py

- 先 GET release 取 id + 已传资产清单；
- 只补「尚未存在」的资产，已存在则跳过（重跑不会 409）；
- 每个资产 4 次重试，容忍 uploads.github.com 经代理的 SSL 瞬断
  （SSLEOFError / ConnectionError / Timeout）。
"""
import os
import sys
import time

import requests

PAT = os.environ.get("GITHUB_PAT", "")
PROXY = "http://127.0.0.1:3067"
REPO = "my788525/agnes-hub-go"
VERSION = "1.0.15"
ROOT = os.path.dirname(os.path.abspath(__file__))

if not PAT:
    sys.exit("[FATAL] 需要 GITHUB_PAT 环境变量")

H = {"Authorization": f"Bearer {PAT}",
     "Accept": "application/vnd.github+json",
     "X-GitHub-Api-Version": "2022-11-28"}
PX = {"https": PROXY, "http": PROXY}


def get_release():
    for i in range(6):
        try:
            r = requests.get(f"https://api.github.com/repos/{REPO}/releases/tags/v{VERSION}",
                             headers=H, proxies=PX, timeout=30)
            if r.status_code == 200:
                return r.json()
            print("  release status", r.status_code, r.text[:120])
        except Exception as e:
            print(f"  release GET retry {i+1}: {type(e).__name__}")
        time.sleep(3)
    sys.exit("[FATAL] 取不到 release，放弃")


def existing_asset_names(rel):
    return {a["name"] for a in rel.get("assets", [])}


def upload(rel_id, path, name, ctype):
    with open(path, "rb") as f:
        data = f.read()
    url = (f"https://uploads.github.com/repos/{REPO}/releases/{rel_id}/assets?name={name}")
    for i in range(5):
        try:
            ur = requests.post(url, headers={**H, "Content-Type": ctype},
                               proxies=PX, data=data, timeout=300)
            if ur.status_code < 300:
                print(f"  + {name} ({len(data)} bytes) [{ur.status_code}]")
                return True
            print(f"  {name} upload {ur.status_code}: {ur.text[:160]}")
        except Exception as e:
            print(f"  {name} upload retry {i+1}: {type(e).__name__} {str(e)[:90]}")
        time.sleep(4)
    return False


def main():
    rel = get_release()
    rel_id = rel["id"]
    have = existing_asset_names(rel)
    print(f"[release] id={rel_id} 已传 {len(have)} 件：{sorted(have)}")

    assets = [
        ("baipiao-hub-%s.fpk" % VERSION, "baipiao-hub-%s.fpk" % VERSION,
         os.path.join(ROOT, "dist", "baipiao-hub-%s.fpk" % VERSION),
         "application/octet-stream"),
        ("baipiao-hub-linux-amd64", "baipiao-hub-linux-amd64",
         os.path.join(ROOT, "baipiao-hub-linux-amd64"), "application/octet-stream"),
        ("baipiao-hub-linux-arm64", "baipiao-hub-linux-arm64",
         os.path.join(ROOT, "baipiao-hub-linux-arm64"), "application/octet-stream"),
        ("agnes-hub-go-windows-%s.zip" % VERSION, "agnes-hub-go-windows-%s.zip" % VERSION,
         os.path.join(ROOT, "dist", "agnes-hub-go-windows-%s.zip" % VERSION), "application/zip"),
        ("agnes-hub-go.exe", "agnes-hub-go.exe",
         os.path.join(ROOT, "agnes-hub-go.exe"), "application/octet-stream"),
    ]

    ok = True
    for display, remote, path, ctype in assets:
        if remote in have:
            print(f"  = {remote} 已存在，跳过")
            continue
        if not os.path.exists(path):
            print(f"  [MISSING LOCAL] {path}")
            ok = False
            continue
        if not upload(rel_id, path, remote, ctype):
            ok = False
            print(f"  [FAIL] {remote}")

    # 复核
    rel2 = get_release()
    final = sorted(existing_asset_names(rel2))
    print(f"[final] 现有 {len(final)} 件：{final}")
    if ok and len(final) == 5:
        print(f"[done] {rel2['html_url']}  5/5 资产齐全")
    else:
        print("[incomplete] 仍有缺失，可重跑本脚本继续补传")
        sys.exit(1)


if __name__ == "__main__":
    main()
