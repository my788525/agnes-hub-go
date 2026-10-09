#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""agnes-hub 飞牛 NAS 运维脚本（热更 / RawCapture 开关 / 抓取 / 健康检查）。

固化流程（见项目记忆）：
  - 停服：普通 SSH 会话跑 appcenter-cli stop（panic 无害）+ pkill -9 -f baipiao-hub 兜底。
  - 替换：SFTP 上传到 /tmp，普通会话 cp -f 覆盖（admin==root 可直接覆盖 agnes-hub 属主文件，
    无需 sudo）+ chown + chmod 755。
  - 启动：必须用 PTY 方式 sudo 调 appcenter-cli start（非 PTY 的 echo PW|sudo -S 在 fnOS 上
    不返回退出状态会挂死 120s）。
  - 数据目录：/vol1/@appdata/agnes-hub/data ；二进制：/vol1/@appcenter/agnes-hub/app/baipiao-hub
    （脚本自动 find 定位，不写死）。

依赖：paramiko（managed venv 已装）。运行：
  python tools/nas_op.py deploy [local_bin]
  python tools/nas_op.py capture on|off
  python tools/nas_op.py fetch
  python tools/nas_op.py health
"""
import os
import sys
import time
import json

try:
    import paramiko
except ImportError:
    sys.exit("[ERROR] 需要 paramiko")

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
APP_ID = "agnes-hub"
PORT = 4142
DATA_DIR = "/vol1/@appdata/%s/data" % APP_ID
DEFAULT_BIN = os.path.join(ROOT, ".gotmp", "baipiao-hub-linux-amd64")
FALLBACK_PASS = os.path.join(ROOT, ".nas_pass")
HOST = "192.168.100.3"


def load_password():
    pw = os.environ.get("NAS_PASS")
    if pw:
        return pw.strip()
    if os.path.exists(FALLBACK_PASS):
        with open(FALLBACK_PASS, encoding="utf-8") as f:
            return f.read().strip()
    raise SystemExit(
        "未找到 NAS 口令：请设置环境变量 NAS_PASS，或在项目根创建 .nas_pass（已被 .gitignore 忽略，勿入库）。")


class Nas:
    def __init__(self, host, password, timeout=45):
        self.password = password
        self.client = paramiko.SSHClient()
        self.client.set_missing_host_key_policy(paramiko.AutoAddPolicy())
        self.client.connect(host, username="admin", password=password, port=22,
                            timeout=timeout, look_for_keys=False, allow_agent=False)

    # 普通（非 PTY）会话：用于 cp / 读文件 / appcenter-cli stop（panic 无害）。
    def run(self, cmd, timeout=60):
        chan = self.client.get_transport().open_session()
        chan.set_combine_stderr(True)
        chan.exec_command(cmd)
        out, start = "", time.time()
        while True:
            if chan.recv_ready():
                out += chan.recv(4096).decode("utf-8", "replace")
            if chan.exit_status_ready():
                while chan.recv_ready():
                    out += chan.recv(4096).decode("utf-8", "replace")
                break
            if time.time() - start > timeout:
                out += "\n[TIMEOUT]"
                break
            time.sleep(0.05)
        try:
            chan.close()
        except Exception:
            pass
        return out

    # PTY sudo：fnOS 启动 appcenter-cli 必须用 PTY，否则退出状态不返回会挂死。
    def sudo_pty(self, cmd, timeout=60):
        chan = self.client.get_transport().open_session()
        chan.set_combine_stderr(True)
        chan.get_pty()
        chan.exec_command("sudo -S " + cmd)
        chan.send(self.password + "\n")
        out, start = "", time.time()
        while True:
            if chan.recv_ready():
                out += chan.recv(4096).decode("utf-8", "replace")
            if chan.exit_status_ready():
                while chan.recv_ready():
                    out += chan.recv(4096).decode("utf-8", "replace")
                break
            if time.time() - start > timeout:
                out += "\n[TIMEOUT]"
                break
            time.sleep(0.05)
        try:
            chan.close()
        except Exception:
            pass
        return "\n".join(l for l in out.splitlines()
                         if "password for" not in l and "[sudo]" not in l)

    def put(self, local, remote):
        sftp = self.client.open_sftp()
        try:
            sftp.put(local, remote)
        finally:
            sftp.close()

    def get(self, remote, local):
        sftp = self.client.open_sftp()
        try:
            sftp.get(remote, local)
        finally:
            sftp.close()

    def listdir(self, path):
        sftp = self.client.open_sftp()
        try:
            return sftp.listdir(path)
        except Exception as e:
            return ["[ERR] %s" % e]
        finally:
            sftp.close()

    def close(self):
        self.client.close()


def locate_exe(nas):
    out = nas.run("find /vol1/@appcenter/%s -maxdepth 4 -name baipiao-hub -type f 2>/dev/null; "
                  "ls -l /vol1/@appcenter/%s/app/baipiao-hub 2>/dev/null" % (APP_ID, APP_ID))
    for line in out.splitlines():
        line = line.strip()
        if line.startswith("/") and "baipiao-hub" in line and "No such" not in line:
            # 取 find 输出的纯路径行（不以权限串开头）
            if line.endswith("baipiao-hub"):
                return line
    return "/vol1/@appcenter/%s/app/baipiao-hub" % APP_ID


def deploy(nas, local_bin):
    if not os.path.exists(local_bin):
        sys.exit("[ERROR] 本地二进制不存在: %s" % local_bin)
    print("[1] 定位现有二进制 + 当前进程")
    exe = locate_exe(nas)
    print("    EXE =", exe)
    print("    PROC:", nas.run("pgrep -af baipiao-hub || echo NONE").strip())
    print("[2] 停服（appcenter-cli stop panic 无害；PTY sudo 精确杀残留进程，"
          "普通 pkill 无权限，pkill -f 又会自杀式匹配自身）")
    nas.run("appcenter-cli stop %s 2>&1" % APP_ID)
    print(nas.sudo_pty("pkill -9 -x baipiao-hub; true", timeout=30).strip())
    time.sleep(2)
    print("    PROC:", nas.run("pgrep -af baipiao-hub || echo NO_PROC").strip())
    print("[3] 上传并覆盖二进制")
    nas.put(local_bin, "/tmp/baipiao-hub")
    print(nas.run("cp -f /tmp/baipiao-hub %s && chown %s:%s %s && chmod 755 %s && "
                  "ls -l %s && echo TRUNC=$(grep -ac 'raw_capture' %s)"
                  % (exe, APP_ID, APP_ID, exe, exe, exe, exe)).strip())
    print("[4] PTY sudo 启动（先 stop 清状态，避免 appcenter 误判 already started）")
    nas.sudo_pty("appcenter-cli stop %s 2>&1; true" % APP_ID, timeout=60)
    time.sleep(2)
    out = nas.sudo_pty("appcenter-cli start %s" % APP_ID, timeout=60)
    print(out.strip())
    if "already started" in out:
        print("[4b] 兜底：nohup 直启新二进制")
        print(nas.sudo_pty("nohup %s -host 0.0.0.0 -port %d -data %s >> %s/app.log 2>&1 &"
                           % (exe, PORT, DATA_DIR, DATA_DIR), timeout=30).strip())
    time.sleep(4)
    print("[5] 健康检查")
    print(nas.run("curl -s http://127.0.0.1:%d/healthz" % PORT).strip())


def set_capture(nas, enabled):
    print("[1] 停服")
    nas.run("appcenter-cli stop %s 2>&1" % APP_ID)
    print(nas.sudo_pty("pkill -9 -x baipiao-hub; true", timeout=30).strip())
    time.sleep(2)
    sp = DATA_DIR + "/settings.json"  # 远端 Linux 路径，必须用正斜杠拼接（Windows 上 os.path.join 会出反斜杠）
    local_tmp = os.path.join(ROOT, ".gotmp", "settings.json")
    os.makedirs(os.path.dirname(local_tmp), exist_ok=True)
    # data 目录仅 agnes-hub 可读、admin 经 SFTP 无权限；用 PTY sudo cat 读取（root），
    # 并从输出中提取纯 JSON（过滤 sudo/chdir 等干扰行）。
    raw = nas.sudo_pty("cat %s" % sp, timeout=30)
    i = raw.find('{')
    j = raw.rfind('}')
    if i == -1 or j == -1:
        sys.stderr.write("DBG raw=%r\n" % raw)
        sys.exit("[ERROR] 无法从 sudo cat 输出解析 settings.json")
    js = raw[i:j + 1]
    cfg = json.loads(js)
    cfg["raw_capture_enabled"] = bool(enabled)
    with open(local_tmp, "w", encoding="utf-8") as f:
        json.dump(cfg, f, ensure_ascii=False, indent=2)
    # 写回：先 put 到 /tmp（admin 可写），再 sudo cp 进 data 目录并修正属主
    nas.put(local_tmp, "/tmp/settings.json")
    print(nas.sudo_pty("cp -f /tmp/settings.json %s && chown agnes-hub:agnes-hub %s && echo WRITTEN"
                       % (sp, sp), timeout=30).strip())
    print("    raw_capture_enabled ->", enabled)
    print("[2] PTY sudo 启动（先 stop 清状态，避免 appcenter 误判 already started）")
    nas.sudo_pty("appcenter-cli stop %s 2>&1; true" % APP_ID, timeout=60)
    time.sleep(2)
    out = nas.sudo_pty("appcenter-cli start %s" % APP_ID, timeout=60)
    print(out.strip())
    if "already started" in out:
        exe = "/vol1/@appcenter/%s/app/baipiao-hub" % APP_ID
        print("[2b] 兜底：nohup 直启")
        print(nas.sudo_pty("nohup %s -host 0.0.0.0 -port %d -data %s >> %s/app.log 2>&1 &"
                           % (exe, PORT, DATA_DIR, DATA_DIR), timeout=30).strip())
    time.sleep(4)
    print("[3] 健康检查")
    print(nas.run("curl -s http://127.0.0.1:%d/healthz" % PORT).strip())


def fetch(nas):
    local_dir = os.path.join(ROOT, ".gotmp", "raw_capture")
    os.makedirs(local_dir, exist_ok=True)
    # data 目录 admin 经 SFTP 无权限，改用 sudo 打包到 /tmp 再 SFTP 下载
    nas.sudo_pty("tar czf /tmp/raw_capture.tgz -C %s raw_capture && echo PACKED" % DATA_DIR, timeout=60)
    tgz = os.path.join(local_dir, "raw_capture.tgz")
    try:
        nas.get("/tmp/raw_capture.tgz", tgz)
    except Exception as e:
        print("[fetch] 下载失败（可能尚未产生捕获文件，请先在 WorkBuddy 发一条消息）:", e)
        return 0
    import tarfile
    import glob
    with tarfile.open(tgz) as t:
        t.extractall(local_dir)
    files = sorted(glob.glob(os.path.join(local_dir, "raw_capture", "*")))
    print("已下载并解压文件数:", len(files), "->", os.path.join(local_dir, "raw_capture"))
    return len(files)


def health(nas):
    print(nas.run("curl -s http://127.0.0.1:%d/healthz" % PORT).strip())
    print("PROC:", nas.run("pgrep -af baipiao-hub || echo NONE").strip())


def set_opt(nas, key, value):
    """通用 settings.json 开关：把某个布尔/字符串设置改为 value 并重启服务。
    用于 #29 的效率开关（upstream_request_gzip / anthropic_prompt_cache）等。"""
    enabled = value.lower() in ("on", "1", "true", "yes")
    print("[1] 停服")
    nas.run("appcenter-cli stop %s 2>&1" % APP_ID)
    print(nas.sudo_pty("pkill -9 -x baipiao-hub; true", timeout=30).strip())
    time.sleep(2)
    sp = DATA_DIR + "/settings.json"
    local_tmp = os.path.join(ROOT, ".gotmp", "settings.json")
    os.makedirs(os.path.dirname(local_tmp), exist_ok=True)
    raw = nas.sudo_pty("cat %s" % sp, timeout=30)
    i = raw.find('{')
    j = raw.rfind('}')
    if i == -1 or j == -1:
        sys.stderr.write("DBG raw=%r\n" % raw)
        sys.exit("[ERROR] 无法从 sudo cat 输出解析 settings.json")
    cfg = json.loads(raw[i:j + 1])
    old = cfg.get(key)
    cfg[key] = bool(enabled)
    with open(local_tmp, "w", encoding="utf-8") as f:
        json.dump(cfg, f, ensure_ascii=False, indent=2)
    nas.put(local_tmp, "/tmp/settings.json")
    print(nas.sudo_pty("cp -f /tmp/settings.json %s && chown agnes-hub:agnes-hub %s && echo WRITTEN"
                       % (sp, sp), timeout=30).strip())
    print("    %s: %r -> %r" % (key, old, bool(enabled)))
    print("[2] PTY sudo 启动")
    nas.sudo_pty("appcenter-cli stop %s 2>&1; true" % APP_ID, timeout=60)
    time.sleep(2)
    out = nas.sudo_pty("appcenter-cli start %s" % APP_ID, timeout=60)
    print(out.strip())
    if "already started" in out:
        exe = "/vol1/@appcenter/%s/app/baipiao-hub" % APP_ID
        print("[2b] 兜底：nohup 直启")
        print(nas.sudo_pty("nohup %s -host 0.0.0.0 -port %d -data %s >> %s/app.log 2>&1 &"
                           % (exe, PORT, DATA_DIR, DATA_DIR), timeout=30).strip())
    time.sleep(4)
    print("[3] 健康检查")
    print(nas.run("curl -s http://127.0.0.1:%d/healthz" % PORT).strip())


def main():
    mode = sys.argv[1] if len(sys.argv) > 1 else "health"
    nas = Nas(HOST, load_password())
    try:
        if mode == "deploy":
            local_bin = sys.argv[2] if len(sys.argv) > 2 else DEFAULT_BIN
            deploy(nas, local_bin)
        elif mode == "capture":
            sub = sys.argv[2] if len(sys.argv) > 2 else "on"
            set_capture(nas, sub.lower() in ("on", "1", "true"))
        elif mode == "fetch":
            fetch(nas)
        elif mode == "setopt":
            if len(sys.argv) < 4:
                sys.exit("用法: setopt <key> <on|off>")
            set_opt(nas, sys.argv[2], sys.argv[3])
        elif mode == "health":
            health(nas)
        else:
            print("未知模式；用法: deploy|capture on|off|setopt <key> <on|off>|fetch|health")
    finally:
        nas.close()


if __name__ == "__main__":
    main()
