package gateway

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sean2077/feishu-bot/internal/config"
	"github.com/sean2077/feishu-bot/internal/core"
)

func profileLabel(name string) string {
	if name == "" {
		return "default"
	}
	return name
}

func (g *Gateway) sessionProfile(s *core.Session) error {
	p, err := g.store.Preferences(s.RoomKey, s.ID)
	if err != nil {
		return err
	}
	if _, err := config.ResolveSessionModel(g.config, p.Profile); err != nil {
		return err
	}
	s.Profile = p.Profile
	return nil
}

func (g *Gateway) currentSession(r core.Room) (string, error) {
	var id string
	err := g.store.DB.QueryRow("SELECT id FROM sessions WHERE room=? AND state='open'", r.Key).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// prepareProfile runs under the room dispatch lock. It stops only a confirmed
// idle generation and never cancels queued inbox items or rotates the native ID.
func (g *Gateway) prepareProfile(r core.Room) (bool, error) {
	id, err := g.currentSession(r)
	if err != nil || id == "" {
		return true, err
	}
	p, err := g.store.Preferences(r.Key, id)
	if err != nil || p.RequestedProfile == nil {
		return true, err
	}
	if _, err := config.ResolveSessionModel(g.config, *p.RequestedProfile); err != nil {
		return false, err
	}
	if *p.RequestedProfile == p.Profile {
		return g.store.ApplyProfile(r.Key, id, p.Revision)
	}
	g.poolMu.Lock()
	defer g.poolMu.Unlock()
	s := g.getSlot(r.Key)
	if s != nil {
		s.events.Lock()
		s.mu.Lock()
		eligible := s.session.ID == id && s.live && s.worker != nil && !s.busy && !s.pinned && s.background == 0 && s.activeRequests == 0 && len(s.deliveries) == 0 && !s.stopUnconfirmed
		s.mu.Unlock()
		var inFlight int
		if eligible {
			err = g.store.DB.QueryRow("SELECT count(*) FROM inbox WHERE room=? AND session=? AND status IN ('sent','consumed')", r.Key, id).Scan(&inFlight)
			eligible = err == nil && inFlight == 0 && g.approvalCount(id) == 0
		}
		if !eligible {
			s.events.Unlock()
			return false, err
		}
		s.mu.Lock()
		s.live = false
		s.cancel()
		worker := s.worker
		s.mu.Unlock()
		s.events.Unlock()
		ctx, cancel := context.WithTimeout(g.ctx, 10*time.Second)
		err = providerVoid(ctx, func() error { return worker.Stop(ctx) })
		cancel()
		if err != nil {
			s.mu.Lock()
			s.stopUnconfirmed = true
			s.mu.Unlock()
			g.audit(r.Key, "profile_stop_unconfirmed", core.Args{"session": id})
			return false, err
		}
		g.removeSlot(s)
		if g.getSlot(r.Key) == s {
			return false, errors.New("profile change is waiting for conversation actions to stop")
		}
	}
	changed, err := g.store.ApplyProfile(r.Key, id, p.Revision)
	if err == nil && !changed {
		g.kick(r)
	}
	if changed {
		g.audit(r.Key, "profile_applied", core.Args{"session": id, "profile": profileLabel(*p.RequestedProfile)})
	}
	return changed, err
}

// A requested profile fences new foreground inputs while the old generation
// finishes. Supplements keep their native stream semantics; compact remains a
// barrier, so a supplement never overtakes a queued control command.
func (g *Gateway) profileInputs(r core.Room, inputs []core.Delivery) ([]core.Delivery, error) {
	if len(inputs) == 0 {
		return inputs, nil
	}
	id, err := g.currentSession(r)
	if err != nil || id == "" {
		return inputs, err
	}
	p, err := g.store.Preferences(r.Key, id)
	if err != nil || p.RequestedProfile == nil {
		return inputs, err
	}
	if s := g.getSlot(r.Key); s != nil {
		s.mu.Lock()
		starting := s.live && !s.stopUnconfirmed && s.worker != nil && s.session.ID == id && len(s.replyOrder) == 0
		s.mu.Unlock()
		if starting {
			// A profile selected during cold startup must not fence the very
			// input that lets native initialize/report a confirmed idle boundary.
			// Finish only that already-starting input (or its complete image batch)
			// with the launched profile; later ordinary inputs remain fenced.
			end := 1
			if batch := inputs[0].ImageBatch; batch != nil {
				for end < len(inputs) && inputs[end].ImageBatch != nil && inputs[end].ImageBatch.ID == batch.ID {
					end++
				}
			}
			return inputs[:end], nil
		}
	}
	allowed := make([]core.Delivery, 0, len(inputs))
	for _, input := range inputs {
		if input.Command != nil {
			break
		}
		if input.SessionID == id && input.SupplementTo != "" && g.supplementAlive(r, input) {
			allowed = append(allowed, input)
		}
	}
	return allowed, nil
}

func (g *Gateway) modelCommand(r core.Room, m core.Message, arg string) error {
	if arg == "" || arg == "--json" {
		names := []string{"default"}
		for name := range g.config.ModelProfiles {
			names = append(names, name)
		}
		sort.Strings(names[1:])
		current, requested := "default", ""
		id, err := g.currentSession(r)
		if err != nil {
			return err
		}
		if id != "" {
			p, err := g.store.Preferences(r.Key, id)
			if err != nil {
				return err
			}
			current = profileLabel(p.Profile)
			if p.RequestedProfile != nil {
				requested = profileLabel(*p.RequestedProfile)
			}
		}
		items := []core.Args{}
		lines := []string{"**本会话模型配置**", "当前选择：" + current}
		if requested != "" {
			lines = append(lines, "下一安全边界："+requested)
		}
		for _, name := range names {
			c, err := config.ResolveSessionModel(g.config, name)
			if err != nil {
				return err
			}
			effort := c.Model.Effort
			if effort == "" {
				effort = "native default"
			}
			items = append(items, core.Args{"name": name, "model": c.Model.Model, "smallModel": c.Model.SmallModel, "effort": effort})
			lines = append(lines, fmt.Sprintf("`%s` · %s · effort %s", name, c.Model.Model, effort))
		}
		lines = append(lines, "", "用 /model 配置名 选择管理员配置。运行、审批和后台工作结束后，下一轮使用新配置并保留原生会话 ID。实际模型与 effort 支持以原生运行时和模型服务为准。")
		text := strings.Join(lines, "\n")
		if arg == "--json" {
			text = jsonPretty(core.Args{"current": current, "requested": requested, "profiles": items})
		}
		g.notify(r, text, &m)
		return nil
	}
	if !canManage(r, m.SenderID) {
		g.notify(r, "切换共享会话模型配置需要项目维护者。", &m)
		return nil
	}
	if arg == "default" {
		arg = ""
	}
	if _, err := config.ResolveSessionModel(g.config, arg); err != nil {
		g.notify(r, "配置不存在；用 /model 查看可选配置。", &m)
		return nil
	}
	s, err := g.store.Session(r.Key)
	if err != nil {
		return err
	}
	if err := g.store.RequestProfile(r.Key, s.ID, arg); err != nil {
		return err
	}
	g.clearProfileWait(r.Key)
	g.kick(r)
	g.notify(r, "已选择 "+profileLabel(arg)+"；将于本会话下一安全轮次边界生效。当前前台、审批和后台工作保留；不会影响其他聊天或更换原生会话 ID。", &m)
	return nil
}

func (g *Gateway) renameCommand(r core.Room, m core.Message, name string) error {
	if !canManage(r, m.SenderID) {
		g.notify(r, "命名共享会话需要项目维护者。", &m)
		return nil
	}
	id, err := g.currentSession(r)
	if err != nil {
		return err
	}
	if id == "" {
		g.notify(r, "尚无当前会话。发送消息建立后可用 /rename 名称。", &m)
		return nil
	}
	if err := g.store.RenameSession(r.Key, id, name); err != nil {
		g.notify(r, err.Error(), &m)
		return nil
	}
	effective := strings.TrimSpace(name)
	if effective == "-" {
		effective = ""
	}
	g.renameNativeSession(r, id, effective)
	g.notify(r, "会话名称已更新。用 /sessions 查看；/resume 支持本聊天的名称或唯一短 ID。", &m)
	return nil
}
