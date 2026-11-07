package gateway

import (
	"context"
	"sort"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
)

type capacityWait struct {
	since  time.Time
	reason string
}

func (g *Gateway) capacityNotice(key string) string {
	g.mu.Lock()
	reason := g.capacityWait[key].reason
	g.mu.Unlock()
	switch reason {
	case "chat_limit":
		return "本聊天的原生进程额度已满，输入已保存。已有任务完成、进程可以安全回收后会自动处理。"
	case "memory_limit":
		return "服务内存已达到新进程准入阈值，输入已保存。内存下降或空闲进程回收后会自动处理。"
	case "memory_unavailable":
		return "暂时无法确认服务内存占用，输入已保存，新进程启动已暂停。观测恢复后会自动处理；请维护者检查服务诊断。"
	default:
		return "当前会话都在工作、监控或停止尚未确认，输入已保存。正常空闲后会自动处理；停止未确认时请维护者检查服务诊断。"
	}
}

func idleOrder(slots []*slot) []*slot {
	type observed struct {
		s  *slot
		at int64
	}
	items := make([]observed, 0, len(slots))
	for _, s := range slots {
		s.mu.Lock()
		items = append(items, observed{s, s.lastActivity})
		s.mu.Unlock()
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].at < items[j].at })
	result := make([]*slot, 0, len(items))
	for _, item := range items {
		result = append(result, item.s)
	}
	return result
}

func (g *Gateway) memoryAdmission(ctx context.Context) (bool, int64, error) {
	if g.config.Runtime.MaxRSSMiB == 0 {
		return true, 0, nil
	}
	if err := ctx.Err(); err != nil {
		return false, 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	n, err := g.residentBytes(ctx)
	return err == nil && n < g.config.Runtime.MaxRSSMiB*(1<<20), n, err
}

// admitWorker runs under poolMu. Existing processes and streaming inputs do not
// enter this path; limits never cancel work, approvals, or background tasks.
func (g *Gateway) admitWorker(r core.Room, slots []*slot) bool {
	for {
		chatCount := 0
		for _, s := range slots {
			if s.room.WorkspaceKey() == r.WorkspaceKey() {
				chatCount++
			}
		}
		reason := ""
		if g.config.Runtime.MaxWorkersPerChat > 0 && chatCount >= g.config.Runtime.MaxWorkersPerChat {
			reason = "chat_limit"
		} else if len(slots) >= g.config.Runtime.MaxWorkers {
			reason = "worker_limit"
		}
		var rss int64
		if reason == "" {
			allowed, n, err := g.memoryAdmission(g.ctx)
			rss = n
			if err != nil {
				reason = "memory_unavailable"
			} else if !allowed {
				reason = "memory_limit"
			}
		}
		if reason == "" {
			return true
		}
		released := false
		if reason != "memory_unavailable" {
			for _, s := range idleOrder(slots) {
				if reason == "chat_limit" && s.room.WorkspaceKey() != r.WorkspaceKey() {
					continue
				}
				if g.reclaimable(s) && g.releaseIdle(g.ctx, s) {
					g.audit(s.room.Key, "worker_capacity_reclaimed", core.Args{"reason": reason, "session": s.session.ID})
					released = true
					break
				}
			}
		}
		if released {
			slots = g.allSlots()
			continue
		}
		g.mu.Lock()
		old := g.capacityWait[r.Key]
		first := old.since.IsZero()
		if first {
			old.since = time.Now()
		}
		changed := first || old.reason != reason
		old.reason = reason
		g.capacityWait[r.Key] = old
		g.mu.Unlock()
		if changed {
			g.audit(r.Key, "worker_admission_wait", core.Args{"reason": reason, "workers": len(slots), "chatWorkers": chatCount, "residentBytes": rss, "maxRssMiB": g.config.Runtime.MaxRSSMiB})
		}
		return false
	}
}

func (g *Gateway) idleLifetime(s *slot) int64 {
	maximum, minimum := g.config.Runtime.IdleMS, g.config.Runtime.IdleMinMS
	if minimum == 0 || maximum == 0 {
		return maximum
	}
	s.mu.Lock()
	uses := s.inputsServed
	s.mu.Unlock()
	budget := min(minimum, maximum)
	for i := uint32(1); i < uses && budget < maximum; i++ {
		budget = min(maximum, budget*2)
	}
	return budget
}

func (g *Gateway) recordWorkerInput(s *slot) {
	s.mu.Lock()
	reused := s.inputsServed > 0
	if s.inputsServed < 32 {
		s.inputsServed++
	}
	s.mu.Unlock()
	g.audit(s.room.Key, "worker_input_dispatched", core.Args{"session": s.session.ID, "reused": reused, "idleBudgetMs": g.idleLifetime(s)})
}

func (g *Gateway) reclaimPressure(ctx context.Context) {
	if g.config.Runtime.MaxRSSMiB == 0 || ctx.Err() != nil {
		return
	}
	needsSample := true
	for _, s := range idleOrder(g.allSlots()) {
		if ctx.Err() != nil {
			return
		}
		if !g.reclaimable(s) {
			continue
		}
		// Sweep holds poolMu, which existing input streams also need. Scan
		// the process tree only when there is a possible release, then reuse
		// that observation until a confirmed release changes the tree.
		if needsSample {
			allowed, _, err := g.memoryAdmission(ctx)
			if allowed || err != nil || ctx.Err() != nil {
				return
			}
			needsSample = false
		}
		if g.releaseIdle(ctx, s) {
			g.audit(s.room.Key, "worker_capacity_reclaimed", core.Args{"reason": "memory_limit", "session": s.session.ID})
			needsSample = true
		}
	}
}
