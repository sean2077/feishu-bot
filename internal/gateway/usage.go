package gateway

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/sean2077/feishu-bot/internal/core"
	"regexp"
	"sort"
	"strings"
	"time"
)

var tokenFields = []string{"input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"}
var resultIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,200}$`)

func tokens(a core.Args) core.Args {
	out := core.Args{}
	for _, key := range tokenFields {
		n, ok := numberArg(a, key)
		if ok && n <= 9007199254740991 {
			out[key] = n
		} else {
			out[key] = nil
		}
	}
	return out
}

var modelNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_.:/-]{1,200}$`)

func reportedModels(models []string) []string {
	out := []string{}
	for _, m := range models {
		if modelNamePattern.MatchString(m) && !contains(out, m) {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	if len(out) > 32 {
		out = out[:32]
	}
	return out
}
func (g *Gateway) recordUsage(tx *sql.Tx, s *slot, ev core.WorkerEvent) (string, error) {
	_, e := tx.Exec("INSERT OR IGNORE INTO usage_coverage(session,room,started,prior_unknown) VALUES(?,?,?,1)", s.session.ID, s.room.Key, time.Now().UnixMilli())
	if e != nil {
		return "", e
	}
	if !resultIDPattern.MatchString(ev.ResultID) {
		_, e = tx.Exec("UPDATE usage_coverage SET unidentified=1 WHERE room=? AND session=?", s.room.Key, s.session.ID)
		return "unidentified", e
	}
	usage := tokens(ev.Usage)
	kind := "turn"
	if ev.Compact != "" {
		kind = "compact"
	}
	res, e := tx.Exec("INSERT OR IGNORE INTO usage_results(session,result_id,room,at,kind,success,models,input_tokens,output_tokens,cache_read_input_tokens,cache_creation_input_tokens) VALUES(?,?,?,?,?,?,?,?,?,?,?)", s.session.ID, ev.ResultID, s.room.Key, time.Now().UnixMilli(), kind, ev.Success, jsonText(reportedModels(ev.Models)), usage[tokenFields[0]], usage[tokenFields[1]], usage[tokenFields[2]], usage[tokenFields[3]])
	if e != nil {
		return "", e
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return "duplicate", nil
	}
	return "new", nil
}
func (g *Gateway) usageReport(r core.Room) (core.Args, error) {
	session := ""
	e := g.store.DB.QueryRow("SELECT id FROM sessions WHERE room=? AND state='open'", r.Key).Scan(&session)
	if e != nil && e != sql.ErrNoRows {
		return nil, e
	}
	var since, prior, unidentified sql.NullInt64
	if session != "" {
		e = g.store.DB.QueryRow("SELECT started,prior_unknown,unidentified FROM usage_coverage WHERE room=? AND session=?", r.Key, session).Scan(&since, &prior, &unidentified)
		if e != nil && e != sql.ErrNoRows {
			return nil, e
		}
	}
	var latest any
	missing := map[string]int64{}
	subtotal := core.Args{}
	usage := core.Args{}
	results := int64(0)
	var sums [4]float64
	var reports [4]int64
	names := []string{}
	if session != "" {
		// Let SQLite aggregate persisted history. Do not materialize every old
		// result or model name in the long-lived gateway heap.
		fields := []string{"count(*)"}
		dest := []any{&results}
		for i, field := range tokenFields {
			fields = append(fields, "count("+field+")", "total("+field+")")
			dest = append(dest, &reports[i], &sums[i])
		}
		if e = g.store.DB.QueryRow("SELECT "+strings.Join(fields, ",")+" FROM usage_results WHERE room=? AND session=?", r.Key, session).Scan(dest...); e != nil {
			return nil, e
		}
		var id, kind, rawModels string
		var at int64
		var success bool
		var values [4]sql.NullInt64
		e = g.store.DB.QueryRow("SELECT result_id,at,kind,success,models,input_tokens,output_tokens,cache_read_input_tokens,cache_creation_input_tokens FROM usage_results WHERE room=? AND session=? ORDER BY at DESC,rowid DESC LIMIT 1", r.Key, session).Scan(&id, &at, &kind, &success, &rawModels, &values[0], &values[1], &values[2], &values[3])
		if e != nil && e != sql.ErrNoRows {
			return nil, e
		}
		if e == nil {
			one := core.Args{}
			for i, key := range tokenFields {
				one[key] = nil
				if values[i].Valid && values[i].Int64 >= 0 && values[i].Int64 <= 9007199254740991 {
					one[key] = values[i].Int64
				}
			}
			var models []string
			if e = json.Unmarshal([]byte(rawModels), &models); e != nil {
				return nil, e
			}
			var modelValue any
			if len(models) > 0 {
				modelValue = models
			}
			latest = core.Args{"resultId": id, "at": at, "kind": kind, "success": success, "models": modelValue, "usage": one}
		}
		rows, e := g.store.DB.Query("SELECT DISTINCT j.value FROM usage_results u,json_each(u.models) j WHERE u.room=? AND u.session=? ORDER BY j.value LIMIT 65", r.Key, session)
		if e != nil {
			return nil, e
		}
		for rows.Next() {
			var name string
			if e = rows.Scan(&name); e != nil {
				rows.Close()
				return nil, e
			}
			names = append(names, name)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return nil, e
		}
	}
	for i, key := range tokenFields {
		missing[key] = results - reports[i]
		subtotal[key] = nil
		usage[key] = nil
		var sum any
		if sums[i] >= 0 && sums[i] <= 9007199254740991 && sums[i] == float64(int64(sums[i])) {
			sum = int64(sums[i])
		}
		if reports[i] > 0 {
			subtotal[key] = sum
		}
		if results > 0 && reports[i] == results && unidentified.Int64 == 0 {
			usage[key] = sum
		}
	}
	truncated := len(names) > 64
	if truncated {
		names = names[:64]
	}
	var modelValue any
	if len(names) > 0 {
		modelValue = names
	}
	var start any
	if since.Valid {
		start = since.Int64
	}
	return core.Args{"session": nullString(session), "scope": "sdk_result_main_loop", "since": start, "historyIncomplete": session != "" && (!since.Valid || prior.Int64 != 0), "unidentifiedResults": unidentified.Int64 != 0, "latest": latest, "cumulative": core.Args{"results": results, "usage": usage, "reportedSubtotal": subtotal, "missing": missing, "models": modelValue, "modelsTruncated": truncated}, "cost": nil, "contextOccupancy": nil}, nil
}
func (g *Gateway) usageReply(r core.Room, arg string) (string, error) {
	if arg != "" && arg != "--json" {
		return "用法：/usage [--json]", nil
	}
	report, e := g.usageReport(r)
	if e != nil {
		return "", e
	}
	if arg == "--json" {
		b, _ := json.MarshalIndent(report, "", "  ")
		return string(b), nil
	}
	if report["session"] == nil {
		return "本聊天尚无会话或用量记录。查询不会创建会话、启动 Worker 或调用模型。", nil
	}
	lines := []string{"**本会话使用量**", "会话：`" + fmt.Sprint(report["session"]) + "`", "统计起点：" + timeLabel(report["since"]), "口径：SDK result.usage，主循环每轮的已报告用量，不含子代理；不是当前上下文占用或账单。"}
	if boolArg(report, "historyIncomplete") {
		lines = append(lines, "旧历史未完整采集，下面仅累计统计起点后的观测，不能当作完整历史总量。")
	}
	if boolArg(report, "unidentifiedResults") {
		lines = append(lines, "存在未提供稳定结果 ID 的结果，未纳入累计；总量未知，已报告小计仅供核对。")
	}
	labels := []string{"输入（不含缓存读写）", "输出", "缓存读取", "缓存创建"}
	amount := func(v any) string {
		if v == nil {
			return "未知"
		}
		return fmt.Sprint(v)
	}
	models := func(v any) string {
		if v == nil {
			return "未知"
		}
		if s, ok := v.([]string); ok {
			return strings.Join(s, "、")
		}
		return fmt.Sprint(v)
	}
	if report["latest"] == nil {
		lines = append(lines, "", "尚无可去重的原生结果用量。未报告字段保持未知，不填零。")
	} else {
		latest := argsOf(report["latest"])
		state := "原生结果失败，已消耗用量仍保留"
		if boolArg(latest, "success") {
			state = "原生结果成功（交付另见 /outputs）"
		}
		lines = append(lines, "", "**最近一轮** · "+timeLabel(latest["at"]), "状态："+state, "SDK 报告的主循环模型："+models(latest["models"]))
		u := argsOf(latest["usage"])
		for i, key := range tokenFields {
			lines = append(lines, labels[i]+"："+amount(u[key]))
		}
	}
	cumulative := argsOf(report["cumulative"])
	lines = append(lines, "", fmt.Sprintf("**累计观测** · %v 个去重结果", cumulative["results"]), "涉及的主循环模型："+models(cumulative["models"]))
	u, sub, miss := argsOf(cumulative["usage"]), argsOf(cumulative["reportedSubtotal"]), argsOf(cumulative["missing"])
	for i, key := range tokenFields {
		line := labels[i] + "：" + amount(u[key])
		n, _ := numberArg(miss, key)
		if n > 0 {
			line += fmt.Sprintf("；%d 轮未报告，已报告小计 %s", n, amount(sub[key]))
		}
		lines = append(lines, line)
	}
	lines = append(lines, "", "同轮多模型只列出归属，不把混合用量猜分到单个模型。不累计 modelUsage 的累计快照，不估造费用；进行中或未返回结果的消耗尚不可知。")
	return strings.Join(lines, "\n"), nil
}
func timeLabel(value any) string {
	var at int64
	switch v := value.(type) {
	case int64:
		at = v
	case float64:
		at = int64(v)
	default:
		return "未知"
	}
	return time.UnixMilli(at).UTC().Format("2006-01-02 15:04:05 UTC")
}
