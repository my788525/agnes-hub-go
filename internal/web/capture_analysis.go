package web

// 控制台「RawCapture 全字段分析」导出端点（GET /api/export/capture-analysis）。
//
// 把 tools/analyze_captures.py 的字段盘点逻辑移植到 Go，使报告由网关二进制本身生成，
// 不依赖运行环境上的 Python。读取 Store.RawCaptureDir() 下的 *.json（网关捕获的
// 客户端原始请求体，未截断/未改写），递归盘点每个字段的路径/类型/频次/取值分布/示例，
// 并给出模态与规模画像，最后渲染成可直接在浏览器打开/另存的 HTML 报告。

import (
	"encoding/json"
	"fmt"
	"html"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// capField 单个字段路径的盘点结果。
type capField struct {
	count     int
	types     map[string]int
	values    map[string]int // key: 类型 + "\x00" + 脱敏取值
	sample    string
	hasSample bool
}

// capReport 一份完整分析。
type capReport struct {
	n                  int
	totalBytes         int
	sizes              []int
	modalities         map[string]int
	models             map[string]int
	streams            map[string]int
	roleDist           map[string]int
	hasContentPolicy   int
	hasUserQuery       int
	totalTurns         int
	contentIsString    int
	contentIsArray     int
	acc                map[string]*capField
	files              []string
}

// capMask 对疑似凭证 / 超长内容做脱敏与截断，避免报告泄露。
func capMask(v string) string {
	lower := strings.ToLower(v)
	for _, t := range []string{"sk-", "bearer ", "api_key", "apikey", "token", "secret"} {
		if strings.Contains(lower, t) {
			return "***(疑似凭证已脱敏)***"
		}
	}
	r := []rune(v)
	if len(r) > 80 {
		return string(r[:77]) + "..."
	}
	return v
}

// capType 返回值的类型名（与 Python 端对齐：dict/list/str/float/bool/None）。
func capType(v any) string {
	switch v.(type) {
	case nil:
		return "None"
	case bool:
		return "bool"
	case float64:
		return "float"
	case string:
		return "str"
	case []any:
		return "list"
	case map[string]any:
		return "dict"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// capNumStr 把数值渲染成人类可读字符串（整数去小数）。
func capNumStr(v float64) string {
	if v == math.Trunc(v) && !math.IsInf(v, 0) {
		return fmt.Sprintf("%d", int64(v))
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// capWalk 递归收集字段路径 → 出现次数 / 类型集合 / 取值集合 / 示例。
func capWalk(prefix string, val any, acc map[string]*capField) {
	typ := capType(val)
	fi := acc[prefix]
	if fi == nil {
		fi = &capField{types: map[string]int{}, values: map[string]int{}}
		acc[prefix] = fi
	}
	fi.count++
	fi.types[typ]++

	switch v := val.(type) {
	case string:
		mv := capMask(v)
		fi.values[typ+"\x00"+mv]++
		if !fi.hasSample {
			fi.sample, fi.hasSample = mv, true
		}
	case bool:
		mv := fmt.Sprintf("%v", v)
		fi.values[typ+"\x00"+mv]++
		if !fi.hasSample {
			fi.sample, fi.hasSample = mv, true
		}
	case float64:
		mv := capNumStr(v)
		fi.values[typ+"\x00"+mv]++
		if !fi.hasSample {
			fi.sample, fi.hasSample = mv, true
		}
	case []any:
		if len(v) > 0 {
			// 数组：把元素结构挂在 前缀[] 下，仅展开一次（首个出现），避免多文件重复展开造成组合爆炸。
			sub := prefix + "[]"
			if _, ok := acc[sub]; !ok {
				limit := len(v)
				if limit > 50 {
					limit = 50
				}
				for i := 0; i < limit; i++ {
					capWalk(sub, v[i], acc)
				}
			}
		}
	case map[string]any:
		for k, vv := range v {
			capWalk(prefix+"."+k, vv, acc)
		}
	}
}

// capAnalyze 读取捕获目录，逐文件解析并汇总。
func capAnalyze(dir string) (*capReport, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	files := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".json") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)
	rep := &capReport{
		modalities:       map[string]int{},
		models:           map[string]int{},
		streams:          map[string]int{},
		roleDist:         map[string]int{},
		acc:              map[string]*capField{},
		files:            files,
		hasContentPolicy: 0,
		hasUserQuery:     0,
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		rep.sizes = append(rep.sizes, len(raw))
		rep.totalBytes += len(raw)
		var d any
		if err := json.Unmarshal(raw, &d); err != nil {
			continue
		}
		m, ok := d.(map[string]any)
		if !ok {
			capWalk("(root)", d, rep.acc)
			continue
		}
		// 模态判定
		if _, has := m["messages"]; has {
			rep.modalities["chat(messages)"]++
		} else if _, has := m["prompt"]; has {
			rep.modalities["image/video(prompt)"]++
		} else if _, has := m["image"]; has {
			rep.modalities["image/video(prompt)"]++
		} else if _, has := m["n"]; has {
			rep.modalities["image/video(prompt)"]++
		} else {
			rep.modalities["unknown"]++
		}
		rep.models[fmt.Sprintf("%v", m["model"])]++
		rep.streams[fmt.Sprintf("%v", m["stream"])]++
		body := string(raw)
		if strings.Contains(body, "<content_policy>") {
			rep.hasContentPolicy++
		}
		if strings.Contains(body, "<user_query>") {
			rep.hasUserQuery++
		}
		if msgs, ok := m["messages"].([]any); ok {
			rep.totalTurns += len(msgs)
			for _, mi := range msgs {
				mm, ok := mi.(map[string]any)
				if !ok {
					continue
				}
				rep.roleDist[fmt.Sprintf("%v", mm["role"])]++
				switch c := mm["content"].(type) {
				case []any:
					rep.contentIsArray++
				case string:
					rep.contentIsString++
				default:
					_ = c
				}
			}
		}
		capWalk("(root)", d, rep.acc)
	}
	rep.n = len(files)
	return rep, nil
}

// capRenderHTML 把分析结果渲染成与 Python 版同款样式的 HTML 报告。
func capRenderHTML(r *capReport) string {
	now := time.Now().Format("2006-01-02 15:04:05")
	avg := 0
	if r.n > 0 {
		avg = r.totalBytes / r.n
	}
	modChat := r.modalities["chat(messages)"]

	// 字段清单：按路径排序，保证输出稳定。
	paths := make([]string, 0, len(r.acc))
	for p := range r.acc {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var rows strings.Builder
	for _, p := range paths {
		fi := r.acc[p]
		typs := make([]string, 0, len(fi.types))
		for t := range fi.types {
			typs = append(typs, t)
		}
		sort.Strings(typs)
		// 取值分布前 3（按频次降序）
		type valEntry struct {
			v string
			c int
		}
		vals := make([]valEntry, 0, len(fi.values))
		for k, c := range fi.values {
			parts := strings.SplitN(k, "\x00", 2)
			vals = append(vals, valEntry{v: parts[1], c: c})
		}
		sort.Slice(vals, func(i, j int) bool { return vals[i].c > vals[j].c })
		limit := len(vals)
		if limit > 3 {
			limit = 3
		}
		var dist strings.Builder
		for i := 0; i < limit; i++ {
			if i > 0 {
				dist.WriteString("; ")
			}
			dist.WriteString(html.EscapeString(vals[i].v))
			dist.WriteString("=")
			dist.WriteString(strconv.Itoa(vals[i].c))
		}
		if dist.Len() == 0 {
			dist.WriteString("—")
		}
		sample := "—"
		if fi.hasSample {
			sample = html.EscapeString(fi.sample)
		}
		occ := fmt.Sprintf("%d/%d (%d%%)", fi.count, r.n, int(float64(fi.count)*100/float64(r.n)))
		rows.WriteString("<tr><td><code>")
		rows.WriteString(html.EscapeString(p))
		rows.WriteString("</code></td><td>")
		rows.WriteString(html.EscapeString(strings.Join(typs, ",")))
		rows.WriteString("</td><td>")
		rows.WriteString(html.EscapeString(occ))
		rows.WriteString("</td><td>")
		rows.WriteString(dist.String())
		rows.WriteString("</td><td>")
		rows.WriteString(sample)
		rows.WriteString("</td></tr>\n")
	}

	kv := func(m map[string]int) string {
		type kvEntry struct {
			k string
			c int
		}
		es := make([]kvEntry, 0, len(m))
		for k, c := range m {
			es = append(es, kvEntry{k: k, c: c})
		}
		sort.Slice(es, func(i, j int) bool { return es[i].c > es[j].c })
		limit := len(es)
		if limit > 10 {
			limit = 10
		}
		var b strings.Builder
		for i := 0; i < limit; i++ {
			if i > 0 {
				b.WriteString("; ")
			}
			b.WriteString(html.EscapeString(es[i].k))
			b.WriteString("×")
			b.WriteString(strconv.Itoa(es[i].c))
		}
		if b.Len() == 0 {
			return "—"
		}
		return b.String()
	}

	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>RawCapture 全字段分析报告</title>
<style>
 body{font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;max-width:980px;margin:24px auto;padding:0 18px;color:#1f2329;line-height:1.6}
 h1{font-size:24px;border-bottom:2px solid #2f6fed;padding-bottom:8px}
 h2{font-size:18px;margin-top:28px;color:#2f6fed}
 .cards{display:flex;flex-wrap:wrap;gap:12px;margin:16px 0}
 .card{background:#f4f7ff;border:1px solid #d6e2ff;border-radius:10px;padding:12px 16px;min-width:120px}
 .card .n{font-size:22px;font-weight:700;color:#2f6fed}
 .card .l{font-size:12px;color:#5b6470}
 table{border-collapse:collapse;width:100%%;font-size:13px;margin:8px 0}
 th,td{border:1px solid #e3e8ef;padding:6px 8px;text-align:left;vertical-align:top}
 th{background:#f4f7ff;color:#2f6fed}
 code{background:#f0f2f5;padding:1px 4px;border-radius:4px;font-size:12px}
 .warn{background:#fff4e6;border:1px solid #ffd8a8;padding:10px 14px;border-radius:8px;color:#8a5300}
 .good{background:#e9f9ee;border:1px solid #a3e3bb;padding:10px 14px;border-radius:8px;color:#1c7a3e}
</style></head><body>
<h1>RawCapture 全字段分析报告</h1>
<p>生成时间：%s ｜ 样本数：%d ｜ 数据源：网关 data/raw_capture/*.json（客户端原始请求体，未截断/未改写）</p>
<div class="cards">
 <div class="card"><div class="n">%d</div><div class="l">捕获请求数</div></div>
 <div class="card"><div class="n">%d</div><div class="l">总字节</div></div>
 <div class="card"><div class="n">%d</div><div class="l">平均字节/请求</div></div>
 <div class="card"><div class="n">%d</div><div class="l">对话类</div></div>
 <div class="card"><div class="n">%d</div><div class="l">消息轮次合计</div></div>
 <div class="card"><div class="n">%d</div><div class="l">含 content_policy</div></div>
 <div class="card"><div class="n">%d</div><div class="l">含 user_query</div></div>
</div>

<h2>1. 模态与规模画像</h2>
<table>
<tr><th>维度</th><th>分布</th></tr>
<tr><td>模态（按 body 推断）</td><td>%s</td></tr>
<tr><td>model 字段</td><td>%s</td></tr>
<tr><td>stream 字段</td><td>%s</td></tr>
<tr><td>messages.role 分布</td><td>%s</td></tr>
<tr><td>content 形态</td><td>字符串 %d 次 ｜ 数组（多模态块） %d 次</td></tr>
</table>

<h2>2. 全字段清单（路径 / 类型 / 出现频次 / 取值分布 / 示例）</h2>
<table>
<tr><th>字段路径</th><th>类型</th><th>出现</th><th>取值分布(前3)</th><th>示例</th></tr>
%s</table>

<h2>3. 结论与建议</h2>
<div class="good">网关 RawCapture 已能稳定捕获客户端完整原始请求体；本报告字段清单即为「接入方实际发送」的真实结构，可作为意图判定、content_policy 剥离、content 数组归一化等逻辑的回归基准。</div>
<div class="warn">提示：本报告字段清单即为「接入方实际发送」的真实结构（此处为网关捕获的原始请求体，未截断/未改写）。如需对任意时段真实流量定期出报告，可把「导出 RawCapture 分析」挂到定时任务。</div>
</body></html>`,
		html.EscapeString(now), r.n, r.n, r.totalBytes, avg, modChat, r.totalTurns,
		r.hasContentPolicy, r.hasUserQuery,
		kv(r.modalities), kv(r.models), kv(r.streams), kv(r.roleDist),
		r.contentIsString, r.contentIsArray, rows.String())
}

// apiExportCaptureAnalysis 控制台「导出 RawCapture 分析」端点：实时读取捕获目录并渲染 HTML 报告。
func (s *Server) apiExportCaptureAnalysis(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	dir := s.Store.RawCaptureDir()
	rep, err := capAnalyze(dir)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()}, nil)
		return
	}
	if rep.n == 0 {
		writeJSON(w, 404, map[string]any{
			"error": "data/raw_capture 下没有 *.json 捕获文件（请先在设置中开启 raw_capture 并发送请求后再导出）",
		}, nil)
		return
	}
	doc := capRenderHTML(rep)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Disposition", "inline; filename=capture_analysis.html")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(doc))
}
