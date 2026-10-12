#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""从 Windows 凭证管理器取出 GitHub PAT（仅内存，绝不落盘），然后跑 release.py。"""
import ctypes
import os
import subprocess
import sys

CRED_TYPE_GENERIC = 1
MAX_PAT = 0x1000


class CREDENTIAL(ctypes.Structure):
    _fields_ = [
        ("Flags", ctypes.c_ulong),
        ("Type", ctypes.c_ulong),
        ("TargetName", ctypes.c_wchar_p),
        ("Comment", ctypes.c_wchar_p),
        ("LastWritten", ctypes.c_uint64),
        ("CredentialBlobSize", ctypes.c_ulong),
        ("CredentialBlob", ctypes.POINTER(ctypes.c_char)),
        ("Persist", ctypes.c_ulong),
        ("AttributeCount", ctypes.c_ulong),
        ("Attributes", ctypes.c_void_p),
        ("TargetAlias", ctypes.c_wchar_p),
        ("UserName", ctypes.c_wchar_p),
    ]


def read_pat():
    adv = ctypes.windll.advapi32
    pcred = ctypes.POINTER(CREDENTIAL)()
    target = "git:https://github.com"
    ok = adv.CredReadW(ctypes.c_wchar_p(target), CRED_TYPE_GENERIC, 0, ctypes.byref(pcred))
    if not ok:
        err = ctypes.GetLastError()
        raise RuntimeError("CredReadW failed, GetLastError=%d" % err)
    try:
        size = pcred.contents.CredentialBlobSize
        blob = ctypes.string_at(pcred.contents.CredentialBlob, size)
        # 凭证二进制可能带尾随 NUL，Windows CreateProcess 拒绝 env 值含 \x00，
        # 取到首个 NUL 为止即可（PAT 本身为 ASCII）。
        return blob.split(b"\x00")[0].decode("ascii", "replace").strip()
    finally:
        adv.CredFree(pcred)


def main():
    pat = read_pat()
    if not pat:
        sys.exit("[FATAL] 凭证管理器里没有 git:https://github.com 的 PAT")
    env = dict(os.environ)
    env["GITHUB_PAT"] = pat
    env["HTTPS_PROXY"] = "http://127.0.0.1:3067"
    env["HTTP_PROXY"] = "http://127.0.0.1:3067"
    env["https_proxy"] = "http://127.0.0.1:3067"
    env["http_proxy"] = "http://127.0.0.1:3067"
    rc = subprocess.run([sys.executable, "tools/release.py"], env=env)
    sys.exit(rc.returncode)


if __name__ == "__main__":
    main()
