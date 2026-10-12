#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""agnes-hub 飞牛 NAS 三步热更（保留数据）：
   1) 备份旧二进制到 @appdata 可回滚
   2) PTY sudo appcenter-cli stop + kill -9 残留进程（admin 非 root，普通 pkill 杀不掉）
   3) SFTP 上传新二进制 -> cp -f 覆盖 + chown + chmod
   4) PTY sudo appcenter-cli start
   5) 校验 healthz version
按记忆「稳健热更三步」实现；appcenter-cli 命令一律 PTY sudo，普通 cp/chown 用 admin 直写。
"""
import os, sys, time
try:
    import paramiko
except ImportError:
    sys.exit("[ERROR] 需要 paramiko")

HOST = "192.168.100.3"
USER = "admin"
PORT = 22
FALLBACK_PASS = "D:/work/M365-fpk/.nas_pass"
APP_ID = "agnes-hub"
APP_BIN = "/vol1/@appcenter/%s/app/baipiao-hub" % APP_ID
DATA_DIR = "/vol1/@appdata/%s" % APP_ID
PORT_NUM = 4142
EXPECT_VER = "1.0.36"

def load_password():
    if os.path.exists(FALLBACK_PASS):
        with open(FALLBACK_PASS, encoding="utf-8") as f:
            return f.read().strip()
    sys.exit("[ERROR] 未找到口令")

PW = load_password()

def log(s): print("[%s] %s" % (time.strftime("%H:%M:%S"), s)); sys.stdout.flush()

class Nas:
    def __init__(self):
        self.client = paramiko.SSHClient()
        self.client.set_missing_host_key_policy(paramiko.AutoAddPolicy())
        self.client.connect(HOST, username=USER, password=PW, port=PORT, timeout=30,
                            look_for_keys=False, allow_agent=False)
    def _drain(self, chan, timeout, label):
        out, start = "", time.time()
        while True:
            if chan.recv_ready(): out += chan.recv(8192).decode("utf-8", "replace")
            if chan.exit_status_ready():
                while chan.recv_ready(): out += chan.recv(8192).decode("utf-8", "replace")
                break
            if time.time() - start > timeout:
                out += "\n[TIMEOUT %s]" % label; break
            time.sleep(0.05)
        return out
    def plain(self, cmd, timeout=30, label="plain"):
        chan = self.client.get_transport().open_session()
        chan.set_combine_stderr(True)
        chan.exec_command(cmd)
        out = self._drain(chan, timeout, label)
        try: chan.close()
        except Exception: pass
        return out
    def pty_sudo(self, cmd, timeout=120, label="sudo"):
        chan = self.client.get_transport().open_session()
        chan.set_combine_stderr(True)
        chan.get_pty()
        chan.exec_command("sudo -S " + cmd)
        chan.send(PW + "\n")
        out = self._drain(chan, timeout, label)
        try: chan.close()
        except Exception: pass
        return out
    def put(self, local, remote):
        sftp = self.client.open_sftp()
        try: sftp.put(local, remote)
        finally: sftp.close()
    def close(self): self.client.close()

def running_pids(nas):
    out = nas.plain("pgrep -f '%s/app/baipiao-hub' || true" % APP_ID, label="pgrep")
    pids = [p.strip() for p in out.split() if p.strip().isdigit()]
    return pids

def main():
    local_bin = "D:/work/GitHub/agnes-hub-go/dist/baipiao-hub-linux-amd64"
    if not os.path.exists(local_bin):
        sys.exit("[ERROR] 本地二进制缺失: %s" % local_bin)

    nas = Nas()
    log("已连接 %s" % HOST)

    # 0) 校验当前版本
    cur = nas.plain("curl -s http://127.0.0.1:%d/healthz || true" % PORT_NUM, label="healthz-before").strip()
    log("部署前 healthz: %s" % cur)

    # 1) 备份旧二进制（可回滚）
    ts = time.strftime("%Y%m%d%H%M%S")
    bak = "%s/app.PRE%s.bak" % (DATA_DIR, ts)
    log("备份旧二进制 -> %s" % bak)
    r = nas.plain("cp -f %s %s && echo BACKEDUP || echo BAKFAIL" % (APP_BIN, bak), label="backup")
    log("备份结果: %s" % r.strip())

    # 2) 停服（PTY sudo）
    log("停服: appcenter-cli stop %s" % APP_ID)
    r = nas.pty_sudo("appcenter-cli stop %s" % APP_ID, timeout=90, label="stop")
    log("stop 返回: %s" % r.strip()[-300:])

    # 2b) 杀残留进程（admin 非 root，必须 PTY sudo kill -9）
    pids = running_pids(nas)
    log("停服后残留进程: %s" % (pids if pids else "NONE"))
    for _ in range(3):
        pids = running_pids(nas)
        if not pids:
            break
        for pid in pids:
            log("kill -9 %s" % pid)
            nas.pty_sudo("kill -9 %s" % pid, timeout=30, label="kill")
        time.sleep(2)
    pids = running_pids(nas)
    if pids:
        log("[WARN] 仍有残留进程未杀掉: %s" % pids)
    else:
        log("确认 NO_PROC（进程已清空）")

    # 3) 上传 + 覆盖
    remote_tmp = "/tmp/baipiao-hub.new"
    log("上传新二进制 -> %s" % remote_tmp)
    nas.put(local_bin, remote_tmp)
    log("覆盖二进制 + chown + chmod 755")
    r = nas.plain("cp -f %s %s && chown %s:%s %s && chmod 755 %s && echo REPLACED || echo REPLACEFAIL"
                  % (remote_tmp, APP_BIN, APP_ID, APP_ID, APP_BIN, APP_BIN), label="replace")
    log("覆盖结果: %s" % r.strip())
    # 校验新文件大小/魔数
    r = nas.plain("ls -la %s && head -c 4 %s | xxd" % (APP_BIN, APP_BIN), label="verify-bin").strip()
    log("新二进制: %s" % r)

    # 4) 启动（PTY sudo）
    log("启动: appcenter-cli start %s" % APP_ID)
    r = nas.pty_sudo("appcenter-cli start %s" % APP_ID, timeout=90, label="start")
    log("start 返回: %s" % r.strip()[-300:])

    # 5) 校验
    time.sleep(4)
    for attempt in range(5):
        hz = nas.plain("curl -s http://127.0.0.1:%d/healthz || true" % PORT_NUM, label="healthz-after").strip()
        log("healthz(%d): %s" % (attempt+1, hz))
        if EXPECT_VER in hz:
            log("✅ 部署成功：版本已为 %s" % EXPECT_VER)
            # 再确认进程是新的
            log("新进程: %s" % running_pids(nas))
            nas.close()
            return
        time.sleep(3)
    log("[FAIL] 部署后 healthz 未出现期望版本 %s，请人工排查。最后 healthz: %s" % (EXPECT_VER, hz))
    nas.close()
    sys.exit(1)

if __name__ == "__main__":
    main()
