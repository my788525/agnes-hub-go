#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""RawCapture 全字段分析器。

读取 data/raw_capture/*.json（网关捕获的客户端原始请求体，未截断/未改写），
逐字段盘点：字段路径、类型、出现频次、取值分布、示例，并给出模态/规模画像与建议。
输出 HTML 报告（默认 .gotmp/capture_analysis.html），同时在 stdout 打印摘要。

用法：
    python tools/analyze_captures.py [captures_dir] [out_html]
"""
import json
import os
import sys
import html
import collections
import datetime

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def mask(v: str) -> str:
    """对疑似密钥/超长内容做脱敏与截断，避免报告泄露。"""
    s = str(v)
    if any(t in s.lower() for t in ("sk-", "bearer ", "api_key", "apikey", "token", "secret")):
        return "***(疑似凭证已脱敏)***"
    if len(s) > 80:
        return s[:77] + "..."
    return s


def walk(prefix, val, acc):
    """递归收集字段路径 → 出现次数 / 类型集合 / 取值集合 / 示例。"""
    typ = type(val).__name__
    if isinstance(val, bool):
        typ = "bool"
    key = prefix
    e = acc.get(key)
    if e is None:
        e = acc[key] = {"count": 0, "types": collections.Counter(), "values": collections.Counter(), "sample": None}
    e["count"] += 1
    e["types"][typ] += 1
    if isinstance(val, (str, bool, int, float)) and not isinstance(val, dict) and not isinstance(val, list):
        e["values"][typ_val := (typ, mask(val))] += 1
        if e["sample"] is None:
            e["sample"] = mask(val)
    elif isinstance(val, list):
        if val:
            # 数组：把元素结构挂在 前缀[] 下（只展开一层元素结构，避免组合爆炸）
            if prefix not in acc or "[]" not in prefix:
                sub = prefix + "[]"
                for item in val[:50]:
                    walk(sub, item, acc)
    elif isinstance(val, dict):
        for k, v in val.items():
            walk(prefix + "." + str(k), v, acc)


def analyze(files):
    acc = collections.OrderedDict()
    sizes = []
    modalities = collections.Counter()
    models = collections.Counter()
    streams = collections.Counter()
    has_content_policy = 0
    has_user_query = 0
    role_dist = collections.Counter()
    total_turns = 0
    content_is_array = 0
    content_is_string = 0
    ts_list = []
    for f in files:
        raw = open(f, encoding="utf-8").read()
        sizes.append(len(raw.encode("utf-8")))
        try:
            d = json.loads(raw)
        except Exception:
            continue
        ts_list.append(os.path.basename(f))
        # 模态判定
        if "messages" in d:
            modalities["chat(messages)"] += 1
        elif "prompt" in d or "image" in d or "n" in d:
            modalities["image/video(prompt)"] += 1
        else:
            modalities["unknown"] += 1
        models[str(d.get("model"))] += 1
        streams[str(d.get("stream"))] += 1
        body = json.dumps(d, ensure_ascii=False)
        if "<content_policy>" in body:
            has_content_policy += 1
        if "<user_query>" in body:
            has_user_query += 1
        msgs = d.get("messages")
        if isinstance(msgs, list):
            total_turns += len(msgs)
            for m in msgs:
                if isinstance(m, dict):
                    role_dist[str(m.get("role"))] += 1
                    c = m.get("content")
                    if isinstance(c, list):
                        content_is_array += 1
                    elif isinstance(c, str):
                        content_is_string += 1
        walk("(root)", d, acc)
    return {
        "acc": acc, "sizes": sizes, "modalities": modalities, "models": models,
        "streams": streams, "has_content_policy": has_content_policy,
        "has_user_query": has_user_query, "role_dist": role_dist,
        "total_turns": total_turns, "content_is_array": content_is_array,
        "content_is_string": content_is_string, "ts_list": ts_list, "files": files,
    }


def render_html(r, out_path):
    n = len(r["files"])
    total_bytes = sum(r["sizes"])
    avg = int(total_bytes / n) if n else 0
    now = datetime.datetime.now().strftime("%Y-%m-%d %H:%M:%S")

    def row(path, e):
        types = ",".join(e["types"].keys())
        vals = "; ".join("%s=%d" % (mask(v), c) for (_, v), c in e["values"].most_common(3)) if e["values"] else "—"
        occ = "%d/%d (%d%%)" % (e["count"], n, round(100 * e["count"] / n))
        return "<tr><td><code>%s</code></td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>" % (
            html.escape(path), html.escape(types), occ, html.escape(vals), html.escape(str(e["sample"] or "—")))

    rows = "\n".join(row(p, e) for p, e in r["acc"].items())

    def kv(counter):
        return "; ".join("%s×%d" % (html.escape(str(k)), v) for k, v in counter.most_common(10)) or "—"

    html_doc = """<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>RawCapture 全字段分析报告</title>
<style>
 body{{font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;max-width:980px;margin:24px auto;padding:0 18px;color:#1f2329;line-height:1.6}}
 h1{{font-size:24px;border-bottom:2px solid #2f6fed;padding-bottom:8px}}
 h2{{font-size:18px;margin-top:28px;color:#2f6fed}}
 .cards{{display:flex;flex-wrap:wrap;gap:12px;margin:16px 0}}
 .card{{background:#f4f7ff;border:1px solid #d6e2ff;border-radius:10px;padding:12px 16px;min-width:120px}}
 .card .n{{font-size:22px;font-weight:700;color:#2f6fed}}
 .card .l{{font-size:12px;color:#5b6470}}
 table{{border-collapse:collapse;width:100%;font-size:13px;margin:8px 0}}
 th,td{{border:1px solid #e3e8ef;padding:6px 8px;text-align:left;vertical-align:top}}
 th{{background:#f4f7ff;color:#2f6fed}}
 code{{background:#f0f2f5;padding:1px 4px;border-radius:4px;font-size:12px}}
 .warn{{background:#fff4e6;border:1px solid #ffd8a8;padding:10px 14px;border-radius:8px;color:#8a5300}}
 .good{{background:#e9f9ee;border:1px solid #a3e3bb;padding:10px 14px;border-radius:8px;color:#1c7a3e}}
</style></head><body>
<h1>RawCapture 全字段分析报告</h1>
<p>生成时间：{now} ｜ 样本数：{n} ｜ 数据源：网关 data/raw_capture/*.json（客户端原始请求体，未截断/未改写）</p>
<div class="cards">
 <div class="card"><div class="n">{n}</div><div class="l">捕获请求数</div></div>
 <div class="card"><div class="n">{total_bytes}</div><div class="l">总字节</div></div>
 <div class="card"><div class="n">{avg}</div><div class="l">平均字节/请求</div></div>
 <div class="card"><div class="n">{mod_chat}</div><div class="l">对话类</div></div>
 <div class="card"><div class="n">{turns}</div><div class="l">消息轮次合计</div></div>
 <div class="card"><div class="n">{cp}</div><div class="l">含 content_policy</div></div>
 <div class="card"><div class="n">{uq}</div><div class="l">含 user_query</div></div>
</div>

<h2>1. 模态与规模画像</h2>
<table>
<tr><th>维度</th><th>分布</th></tr>
<tr><td>模态（按 body 推断）</td><td>{modalities}</td></tr>
<tr><td>model 字段</td><td>{models}</td></tr>
<tr><td>stream 字段</td><td>{streams}</td></tr>
<tr><td>messages.role 分布</td><td>{roles}</td></tr>
<tr><td>content 形态</td><td>字符串 {cis} 次 ｜ 数组（多模态块） {cia} 次</td></tr>
</table>

<h2>2. 全字段清单（路径 / 类型 / 出现频次 / 取值分布 / 示例）</h2>
<table>
<tr><th>字段路径</th><th>类型</th><th>出现</th><th>取值分布(前3)</th><th>示例</th></tr>
{rows}
</table>

<h2>3. 结论与建议</h2>
<div class="good">网关 RawCapture 已能稳定捕获客户端完整原始请求体；本报告字段清单即为「接入方实际发送」的真实结构，可作为意图判定、content_policy 剥离、content 数组归一化等逻辑的回归基准。</div>
<div class="warn">提示：本报告字段清单即为「接入方实际发送」的真实结构（此处为网关捕获的原始请求体，未截断/未改写）。如需对任意时段真实流量定期出报告，可把本工具挂到定时任务或控制台「导出分析」。</div>
<p>复用命令：<code>python tools/analyze_captures.py &lt;captures_dir&gt; &lt;out.html&gt;</code></p>
</body></html>""".format(
        now=now, n=n, total_bytes=total_bytes, avg=avg,
        mod_chat=r["modalities"].get("chat(messages)", 0), turns=r["total_turns"],
        cp=r["has_content_policy"], uq=r["has_user_query"],
        modalities=kv(r["modalities"]), models=kv(r["models"]), streams=kv(r["streams"]),
        roles=kv(r["role_dist"]), cis=r["content_is_string"], cia=r["content_is_array"],
        rows=rows,
    )
    with open(out_path, "w", encoding="utf-8") as f:
        f.write(html_doc)
    return out_path


def main():
    cap_dir = sys.argv[1] if len(sys.argv) > 1 else os.path.join(ROOT, "data", "raw_capture")
    out = sys.argv[2] if len(sys.argv) > 2 else os.path.join(ROOT, ".gotmp", "capture_analysis.html")
    files = sorted(
        os.path.join(cap_dir, f) for f in os.listdir(cap_dir)
        if f.endswith(".json") and os.path.isfile(os.path.join(cap_dir, f))
    ) if os.path.isdir(cap_dir) else []
    if not files:
        sys.exit("未在 %s 找到 *.json 捕获文件" % cap_dir)
    r = analyze(files)
    out = render_html(r, out)
    n = len(r["files"])
    print("样本数:", n, "| 总字节:", sum(r["sizes"]), "| 平均:", int(sum(r["sizes"]) / n), "B")
    print("模态:", dict(r["modalities"]))
    print("字段数:", len(r["acc"]))
    print("报告已写入:", out)


if __name__ == "__main__":
    main()
