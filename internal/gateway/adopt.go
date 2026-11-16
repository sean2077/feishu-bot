package gateway

import (
	"strconv"
	"strings"
	"time"

	"github.com/sean2077/feishu-bot/internal/core"
	"github.com/sean2077/feishu-bot/internal/native"
	"github.com/sean2077/feishu-bot/internal/process"
	"github.com/sean2077/feishu-bot/internal/store"
	"github.com/sean2077/feishu-bot/internal/workspace"
)

const (
	adoptedPendingTTL = 2 * time.Minute

	adoptedHolderNotice    = "本地终端仍在使用该会话，这条消息已排队；终端退出后会自动继续。"
	adoptedPendingNotice   = "该话题对应的本地会话认领尚未完成，暂不能启动模型。认领完成后请在本话题重新发送这条消息；不要重复发起认领。"
	adoptedUncertainNotice = "该话题对应的本地会话认领结果未确认，暂不能启动模型。请按根消息 %s 核查后再处理；不会自动重发。"
)

// adoptedRecord resolves the durable adoption behind one inbound DM topic
// message. The returned rejected flag means the message must be dropped; it
// never falls back to the ordinary DM room.
func (g *Gateway) adoptedRecord(m core.Message) (store.AdoptedTopic, bool, bool) {
	reject := func(reason, room string) (store.AdoptedTopic, bool, bool) {
		g.audit(room, "adopted_topic_rejected", core.Args{
			"reason": reason, "message": m.ID, "thread": m.ThreadID, "root": m.RootID,
		})
		return store.AdoptedTopic{}, false, true
	}
	record, found, err := g.store.AdoptedTopicByThread(m.ThreadID)
	if err != nil {
		return reject("lookup_failed", "")
	}
	if !found && m.RootID != "" {
		record, found, err = g.store.AdoptedTopicByRoot(m.RootID)
		if err != nil {
			return reject("lookup_failed", "")
		}
	}
	if !found {
		return store.AdoptedTopic{}, false, false
	}
	if record.Room == "" || record.Root == "" {
		return store.AdoptedTopic{}, false, false
	}
	if record.State == store.AdoptedOpen {
		if m.ThreadID != record.Thread || m.RootID != record.Root {
			return reject("thread_or_root_mismatch", record.Room)
		}
	} else {
		if record.Thread != "" && m.ThreadID != record.Thread {
			return reject("thread_mismatch", record.Room)
		}
		if m.RootID != "" && m.RootID != record.Root {
			return reject("root_mismatch", record.Room)
		}
	}
	return record, true, false
}

// adoptedWork revalidates the recorded working directory as a physical
// directory below the DM's current adoption roots.
func (g *Gateway) adoptedWork(dm, stored string) (string, bool) {
	if stored == "" {
		return "", false
	}
	physical, err := workspace.PhysicalDirectory(stored)
	if err != nil || !core.SamePath(physical, stored) {
		return "", false
	}
	if !g.config.AdoptWorkAllowed(dm, physical) {
		return "", false
	}
	return physical, true
}

func (g *Gateway) adoptedRoom(m core.Message, record store.AdoptedTopic) (core.Room, bool) {
	work, ok := g.adoptedWork(m.SenderID, record.Work)
	if !ok {
		g.audit(record.Room, "adopted_topic_rejected", core.Args{"reason": "work_outside_roots", "message": m.ID})
		return core.Room{}, false
	}
	r, ok := g.config.AdoptedTopicRoom(m.ChatID, m.SenderID, record.Root)
	if !ok || r.Key != record.Room {
		g.audit(record.Room, "adopted_topic_rejected", core.Args{"reason": "configuration_changed", "message": m.ID})
		return core.Room{}, false
	}
	r.WorkspaceCWD = work
	r.AdoptedState = record.State
	return r, true
}

// route is the gateway's routing entry point. It applies the configured
// message policy first, then replaces the ordinary DM room with the adopted
// topic room when a durable record matches.
func (g *Gateway) route(m core.Message) (core.Room, bool) {
	r, ok := Route(g.config, m)
	if !ok {
		return core.Room{}, false
	}
	if m.ChatType != "p2p" || m.ThreadID == "" {
		return r, true
	}
	record, found, rejected := g.adoptedRecord(m)
	if rejected {
		return core.Room{}, false
	}
	if !found {
		return r, true
	}
	return g.adoptedRoom(m, record)
}

// routeQueued re-authorizes an accepted input for roomKey under the current
// configuration and adoption record.
func (g *Gateway) routeQueued(roomKey string, m core.Message) (core.Room, bool) {
	if m.InteractionMessageID == "" {
		r, ok := g.route(m)
		return r, ok && r.Key == roomKey
	}
	if m.SenderIsBot || m.SenderID == "" || m.ID == "" {
		return core.Room{}, false
	}
	r, ok := g.roomFromKey(roomKey)
	kind := "group"
	if r.DMUser != "" {
		kind = "p2p"
	}
	if !ok || r.ChatID != m.ChatID || m.ChatType != kind {
		return core.Room{}, false
	}
	return r, AuthorizedAuthor(r, m.SenderID)
}

// roomFromKey resolves a stored key only through its durable adoption record
// when the key names a DM topic. A shape-valid key without a record is
// rejected; no gateway entry point may bypass this check.
func (g *Gateway) roomFromKey(roomKey string) (core.Room, bool) {
	r, ok := RoomFromKey(g.config, roomKey)
	if !ok {
		return core.Room{}, false
	}
	if r.DMUser == "" || r.Thread == "" {
		return r, true
	}
	record, found, err := g.store.AdoptedTopicByRoom(roomKey)
	if err != nil || !found || record.Room != roomKey || record.Root != r.Thread {
		return core.Room{}, false
	}
	work, ok := g.adoptedWork(r.DMUser, record.Work)
	if !ok {
		g.audit(roomKey, "adopted_topic_rejected", core.Args{"reason": "work_outside_roots"})
		return core.Room{}, false
	}
	r.WorkspaceCWD = work
	r.AdoptedState = record.State
	return r, true
}

func (g *Gateway) holderAlive(record store.AdoptedTopic) bool {
	if record.HolderPID <= 0 || !process.Alive(record.HolderPID) {
		return false
	}
	if record.HolderStarted != 0 {
		if started, ok := process.StartTime(record.HolderPID); ok && started != record.HolderStarted {
			// A different start time proves PID reuse. If the start time cannot be
			// read, stay conservative and keep gating; only a confirmed identity
			// mismatch releases the session.
			return false
		}
	}
	return true
}

// holderFor reports whether the adopted session is still owned by a live
// local terminal. A dead holder is cleared here so the next dispatch can run.
func (g *Gateway) holderFor(room core.Room, sessionID string) (store.AdoptedTopic, bool) {
	if room.DMUser == "" || room.Thread == "" {
		return store.AdoptedTopic{}, false
	}
	record, found, err := g.store.AdoptedTopicByRoom(room.Key)
	if err != nil || !found || record.State != store.AdoptedOpen || record.Session != sessionID {
		return store.AdoptedTopic{}, false
	}
	if g.holderAlive(record) {
		return record, true
	}
	if record.HolderPID > 0 {
		cleared, err := g.store.ClearAdoptionHolder(record.Native, record.HolderPID, record.HolderStarted)
		if err == nil && cleared {
			g.clearAdoptNotice(room.Key)
			g.removeAdoptEndpoint(room.Key)
			g.failForwarded(room, "holder_released")
		}
	}
	return record, false
}

// adoptionHold is the dispatch-time safety net used by recovery and periodic
// kicks, not only by newly arrived messages.
func (g *Gateway) adoptionHold(room core.Room) bool {
	if room.DMUser == "" || room.Thread == "" {
		return false
	}
	current, found, err := g.store.CurrentSession(room.Key)
	if err != nil || !found {
		return false
	}
	_, held := g.holderFor(room, current.ID)
	return held
}

// adoptedRoomBusy reports why a re-attach must not hand the native session to
// a local terminal. Callers hold the room's input and dispatch locks so a new
// input or worker start cannot slip in between this check and the holder
// update. The returned reason is empty when the room is safe to re-attach.
func (g *Gateway) adoptedRoomBusy(record store.AdoptedTopic) (string, error) {
	var queued, outputs, actions int
	err := g.store.DB.QueryRow(`SELECT
 (SELECT count(*) FROM inbox WHERE room=? AND status IN ('queued','sent','consumed')),
 (SELECT count(*) FROM outbox WHERE room=? AND state NOT IN ('done','cancelled')),
 (SELECT count(*) FROM action_runs WHERE room=? AND state NOT IN ('done','failed','uncertain'))`,
		record.Room, record.Room, record.Room).Scan(&queued, &outputs, &actions)
	if err != nil {
		return "", err
	}
	switch {
	case queued > 0:
		return "pending_inputs", nil
	case outputs > 0:
		return "pending_outputs", nil
	case actions > 0:
		return "pending_actions", nil
	}
	if g.approvalCount(record.Session) > 0 {
		return "pending_approvals", nil
	}
	if g.stopIsBlocked(record.Room) {
		return "stop_blocked", nil
	}
	if s := g.getSlot(record.Room); s != nil {
		s.mu.Lock()
		live := s.live || s.worker != nil || s.busy || s.background != 0 || s.pinned || s.activeRequests > 0 || s.stopUnconfirmed
		s.mu.Unlock()
		if live {
			return "worker_active", nil
		}
	}
	return "", nil
}

func (g *Gateway) notifyAdoptOnce(key string, room core.Room, text string, m *core.Message) {
	g.mu.Lock()
	if g.adoptNotice == nil {
		g.adoptNotice = map[string]bool{}
	}
	first := !g.adoptNotice[key]
	g.adoptNotice[key] = true
	g.mu.Unlock()
	if first {
		g.notify(room, text, m)
	}
}

func (g *Gateway) clearAdoptNotice(roomKey string) {
	g.mu.Lock()
	for key := range g.adoptNotice {
		if key == "holder:"+roomKey || key == "state:"+roomKey || key == "failure:"+roomKey ||
			key == "relay:"+roomKey || key == "relay-failed:"+roomKey || key == "forward-failed:"+roomKey {
			delete(g.adoptNotice, key)
		}
	}
	g.mu.Unlock()
}

// notifyAdoptionFailure explains a rejected adopted-topic input only when the
// durable record itself matches. Thread/root mismatches remain silent so a
// spoofed event cannot turn the bot into an oracle.
func (g *Gateway) notifyAdoptionFailure(m core.Message) {
	if m.ChatType != "p2p" || m.ThreadID == "" {
		return
	}
	record, found, err := g.store.AdoptedTopicByThread(m.ThreadID)
	if err != nil || !found || record.State != store.AdoptedOpen || record.Root != m.RootID || record.Thread != m.ThreadID {
		return
	}
	room, ok := g.config.AdoptedTopicRoom(m.ChatID, m.SenderID, record.Root)
	if !ok || room.Key != record.Room {
		room, ok = RoomFor(g.config, m.ChatID, m.SenderID)
		if !ok {
			return
		}
	}
	g.notifyAdoptOnce("failure:"+record.Room, room, "本地会话的工作目录已不可用或不在 adoptRoots 内，这条消息暂不处理。请管理员恢复目录与配置；原会话和话题记录保留。", &m)
}

func adoptedStateNotice(record store.AdoptedTopic) string {
	if record.State == store.AdoptedUncertain {
		return "该话题对应的本地会话认领结果未确认（根消息 " + record.Root + "），暂不能启动模型。请在私聊核查；不会自动重发。"
	}
	if record.State == store.AdoptedArchived {
		return "该话题已归档，自动恢复失败；请稍后重新发送这条消息。"
	}
	return adoptedPendingNotice
}

// restoreArchivedTopic revives an archived adoption in place for an inbound
// owner message. The room input lock is held by the caller.
func (g *Gateway) restoreArchivedTopic(room core.Room) bool {
	record, found, err := g.store.AdoptedTopicByRoom(room.Key)
	if err != nil || !found {
		return false
	}
	if record.State == store.AdoptedOpen {
		return true
	}
	if record.State != store.AdoptedArchived {
		return false
	}
	restored, err := g.store.RestoreAdoption(record.Native, time.Now().UnixMilli())
	if err != nil {
		if g.logger != nil {
			g.logger("adopt_restore_failed", core.Args{"error": core.Redact(err.Error(), g.secrets)})
		}
		return false
	}
	if !restored {
		// A concurrent notify or attach may have restored it first.
		fresh, ok, readErr := g.store.AdoptedTopicByRoom(room.Key)
		return readErr == nil && ok && fresh.State == store.AdoptedOpen
	}
	g.clearAdoptNotice(room.Key)
	g.audit(room.Key, "adopted_topic_restored", core.Args{"native": record.Native, "cause": "message"})
	return true
}

func idleNewSessionNotice(idleMS int64) string {
	hours := strconv.FormatFloat(float64(idleMS)/3600000, 'f', -1, 64)
	return "距上次对话已超过 " + hours + " 小时，已开新会话；/resume 可回到上一个。"
}

// sweepAdoptions expires abandoned intents and releases holders that no longer
// exist. Pending inputs are resumed by the ordinary pending-room kick.
func (g *Gateway) sweepAdoptions(now int64) {
	if err := g.store.ExpirePendingAdoptions(now-adoptedPendingTTL.Milliseconds(), now); err != nil && g.logger != nil {
		g.logger("adopt_expire_failed", core.Args{"error": core.Redact(err.Error(), g.secrets)})
	}
	records, err := g.store.AdoptedTopics()
	if err != nil && g.logger != nil {
		g.logger("adopt_sweep_failed", core.Args{"error": core.Redact(err.Error(), g.secrets)})
	}
	for _, record := range records {
		if record.State != store.AdoptedOpen || record.HolderPID <= 0 {
			// Never clear the one-shot notice here: a periodic sweep is not a
			// state change and must not turn pending/uncertain notices into a
			// per-sweep reminder.
			continue
		}
		if g.holderAlive(record) {
			continue
		}
		released, ok := g.releaseDeadHolder(record.Native)
		if !ok {
			continue
		}
		if room, ok := g.roomFromKey(released); ok {
			g.failForwarded(room, "holder_released")
		} else {
			g.clearAdoptEndpoint(released)
		}
	}
	// Archive open topics whose terminal is gone and that have had no activity
	// for the configured window. maybeArchive re-reads under the per-native
	// lock, so a holder released above is evaluated with its fresh state.
	records, err = g.store.AdoptedTopics()
	if err != nil {
		if g.logger != nil {
			g.logger("adopt_sweep_failed", core.Args{"error": core.Redact(err.Error(), g.secrets)})
		}
		return
	}
	for _, record := range records {
		if record.State == store.AdoptedOpen {
			g.maybeArchiveAdoption(record.Native, now)
		}
	}
}

// maybeArchiveAdoption archives one inactive adopted topic and then fails any
// forwards left without a holder. Transport work happens after the lock.
func (g *Gateway) maybeArchiveAdoption(native string, now int64) {
	room, archived := g.archiveInactiveAdoption(native, now)
	if !archived {
		return
	}
	if r, ok := g.roomFromKey(room); ok {
		g.failForwarded(r, "archived")
	} else {
		g.clearAdoptEndpoint(room)
	}
}

func (g *Gateway) archiveInactiveAdoption(native string, now int64) (string, bool) {
	window := g.config.AdoptedArchiveMS
	if window <= 0 {
		return "", false
	}
	unlock, err := g.adoptLocks.lockContext(g.ctx, native)
	if err != nil {
		return "", false
	}
	defer unlock()
	record, found, err := g.store.AdoptedTopicByNative(native)
	if err != nil {
		if g.logger != nil {
			g.logger("adopt_archive_failed", core.Args{"error": core.Redact(err.Error(), g.secrets)})
		}
		return "", false
	}
	if !found || record.State != store.AdoptedOpen || g.holderAlive(record) {
		return "", false
	}
	last := record.LastActive
	if last == 0 {
		last = record.Updated
	}
	if last == 0 {
		last = record.Created
	}
	if now-last <= window {
		return "", false
	}
	archived, err := g.store.ArchiveAdoption(native, now)
	if err != nil {
		if g.logger != nil {
			g.logger("adopt_archive_failed", core.Args{"error": core.Redact(err.Error(), g.secrets)})
		}
		return "", false
	}
	if !archived {
		return "", false
	}
	g.clearAdoptNotice(record.Room)
	g.audit(record.Room, "adopted_topic_archived", core.Args{"native": native, "lastActive": last})
	g.removeAdoptEndpoint(record.Room)
	return record.Room, true
}

// releaseDeadHolder clears one dead terminal holder and its relay endpoint
// under the per-native adoption lock. attach and notify hold the same lock
// while they replace a holder and register a new endpoint, so a sweep working
// from a stale snapshot can never delete a registration that was just
// refreshed. Callers notify affected forwards after the lock is released.
func (g *Gateway) releaseDeadHolder(native string) (string, bool) {
	unlock, err := g.adoptLocks.lockContext(g.ctx, native)
	if err != nil {
		return "", false
	}
	defer unlock()
	record, found, err := g.store.AdoptedTopicByNative(native)
	if err != nil {
		if g.logger != nil {
			g.logger("adopt_holder_clear_failed", core.Args{"error": core.Redact(err.Error(), g.secrets)})
		}
		return "", false
	}
	if !found || record.State != store.AdoptedOpen || record.HolderPID <= 0 || g.holderAlive(record) {
		return "", false
	}
	cleared, err := g.store.ClearAdoptionHolder(record.Native, record.HolderPID, record.HolderStarted)
	if err != nil {
		if g.logger != nil {
			g.logger("adopt_holder_clear_failed", core.Args{"error": core.Redact(err.Error(), g.secrets)})
		}
		return "", false
	}
	if !cleared {
		// A newer holder replaced this snapshot; leave its endpoint intact.
		return "", false
	}
	g.clearAdoptNotice(record.Room)
	g.audit(record.Room, "adopted_holder_released", core.Args{"native": record.Native, "pid": record.HolderPID})
	g.removeAdoptEndpoint(record.Room)
	return record.Room, true
}

// adoptedModelWarning explains the model the gateway will use after the local
// holder exits. It stays silent when the transcript has no model or matches the
// gateway's model.
func adoptedModelWarning(gatewayModel, transcriptModel string) string {
	transcriptModel = strings.TrimSpace(transcriptModel)
	if !core.ValidModelIdentifier(transcriptModel) {
		return ""
	}
	normalize := func(value string) string {
		if at := strings.LastIndexByte(value, '['); at >= 0 && len(value) > at+1 && value[len(value)-1] == ']' {
			return value[:at]
		}
		return value
	}
	if normalize(gatewayModel) == normalize(transcriptModel) {
		return ""
	}
	return "终端退出后将沿用 " + transcriptModel + " 续接。"
}

// adoptedTranscriptModel returns the last assistant model recorded in the
// adopted session's transcript. It applies only when the room still selects
// that adopted session and the user has not explicitly selected a model
// profile; every other launch keeps the project/deployment model.
func (g *Gateway) adoptedTranscriptModel(r core.Room, sessionID, profile string) (string, bool) {
	if r.DMUser == "" || r.Thread == "" || r.AdoptedState != store.AdoptedOpen || sessionID == "" || strings.TrimSpace(profile) != "" {
		return "", false
	}
	record, found, err := g.store.AdoptedTopicByRoom(r.Key)
	if err != nil || !found || record.State != store.AdoptedOpen || record.Session != sessionID {
		return "", false
	}
	_, configDir, _, err := workspace.InheritedConfig()
	if err != nil {
		return "", false
	}
	transcript, err := native.LocateTranscript(configDir, record.Native)
	if err != nil {
		return "", false
	}
	model := strings.TrimSpace(transcript.Model)
	if !core.ValidModelIdentifier(model) {
		return "", false
	}
	return model, true
}
