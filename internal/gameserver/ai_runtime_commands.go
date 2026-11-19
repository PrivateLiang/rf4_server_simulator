package gameserver

// AI 运行态命令消费者（T-020 / DEV-004）。
//
// 唯一活动 gameserver 是 actor / channel_agent / memory 的业务写者：
//   - 每 500ms 从 ai_runtime_command 取最小 pending（每轮 ≤32 条）；
//   - 每条命令一个 BEGIN IMMEDIATE 事务，同事务读 control / 当前配置、
//     校验代次与版本、更新运行态、记终态与 result_revision；
//   - 坏命令标 rejected 继续下一条，不堵队列；瞬时 busy/超时保留 pending。
//
// 消费者挂在 AI 生命周期上：StartAIFishing 已经由 cmd/gameserver 启动，
// 这里在同一个起点启动命令循环，不改 cmd/*。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	mrand "math/rand"
	"strconv"
	"strings"
	"time"

	"rf4server/internal/aiconfig"
	"rf4server/internal/store"
)

const (
	aiRuntimeCommandInterval = 500 * time.Millisecond
	aiRuntimeCommandBatch    = 32
	aiRuntimeCommandBudget   = 50 * time.Millisecond
)

// StartAIRuntimeCommands 启动命令消费者；应与 StartAIFishing 同时机启动。
func (s *Server) StartAIRuntimeCommands(db *store.DB, stop <-chan struct{}) {
	if db == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(aiRuntimeCommandInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				s.aiRuntimeCommandDrain(db)
			}
		}
	}()
}

// aiRuntimeCommandDrain 每轮最多应用 aiRuntimeCommandBatch 条；单条超时即回滚
// 并留给下一轮。
func (s *Server) aiRuntimeCommandDrain(db *store.DB) {
	for i := 0; i < aiRuntimeCommandBatch; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), aiRuntimeCommandBudget)
		cmd, err := db.AIApplyNextRuntimeCommand(ctx, float64(time.Now().Unix()), s.applyAIRuntimeCommand)
		cancel()
		if err != nil {
			s.log.Warn("AI 运行态命令应用失败", "err", err)
			return
		}
		if cmd == nil {
			return
		}
	}
}

// applyAIRuntimeCommand validates origin and applies the command under the same write lock.
func (s *Server) applyAIRuntimeCommand(tx *store.AIRuntimeTx, cmd *store.AIRuntimeCommand) (bool, string, error) {
	now := float64(time.Now().Unix())
	r := store.ReadAIEmergency(tx.Context(), tx.SQL())
	if r.Err != nil {
		return false, "", r.Err
	}
	dec := aiconfig.EvaluateEmergency(r)
	epoch := cmd.IntentEpoch
	if cmd.Source == store.AIRuntimeSourceBridge {
		parent, err := store.AITaskEpoch(tx.Context(), tx.SQL(), cmd.ParentTaskID)
		if err != nil {
			return false, "", err
		}
		if parent != epoch {
			epoch = 0
		}
	}
	if active, reason := dec.IntentActive(epoch); !active {
		return false, reason, nil
	}
	switch cmd.Kind {
	case store.AIRuntimeKindActorSetEnabled:
		return s.applyAIActorSetEnabled(tx, cmd, now)
	case store.AIRuntimeKindActorReroll:
		return s.applyAIActorReroll(tx, cmd, now)
	case store.AIRuntimeKindActorTargetSuggest:
		return s.applyAIActorTargetSuggest(tx, cmd, now)
	case store.AIRuntimeKindChannelSetMember:
		return s.applyAIChannelSetMember(tx, cmd)
	case store.AIRuntimeKindMemoryClear:
		return s.applyAIMemoryClear(tx, cmd)
	case store.AIRuntimeKindAgentDelete:
		return s.applyAIAgentDelete(tx, cmd)
	}
	return false, "unsupported_kind", nil
}

// aiRuntimeGenerationOK 校验后台/BR 看到的代次；0 表示「不约束」，
// 只用于绝对设置意图。相对操作与建议必须带非零代次。
func aiRuntimeGenerationOK(expected, current int64) bool {
	return expected == 0 || expected == current
}

// aiRuntimeRevisionOK 同上，0 表示不约束。
func aiRuntimeRevisionOK(expected, current int64) bool {
	return expected == 0 || expected == current
}

func (s *Server) applyAIActorSetEnabled(tx *store.AIRuntimeTx, cmd *store.AIRuntimeCommand, now float64) (bool, string, error) {
	enable, err := store.AIRuntimePayloadActorSetEnabled(cmd.Payload)
	if err != nil {
		return false, "bad_payload", nil
	}
	agentEnabled, _, actorEnabled, found, err := tx.AgentFlags(cmd.AgentKey)
	if err != nil {
		return false, "", err
	}
	ctl, err := tx.Control(cmd.AgentKey)
	if err != nil {
		return false, "", err
	}
	if ctl.Tombstoned || !found {
		return false, "agent_missing", nil
	}
	// 能力保存触发的运行变更：在同一事务里复核能力修订。
	// 旧修订的补派不能把已经保存的新集合反向覆盖；配置缺失/失效也不执行。
	if aiconfig.IsCapabilityRuntimeRequest(cmd.RequestID) {
		caps, capErr := store.AIReadCapabilityState(tx.Context(), tx.SQL(), cmd.AgentKey)
		if capErr != nil {
			return false, "", capErr
		}
		if caps.State != aiconfig.CapabilityStateConfigured &&
			caps.State != aiconfig.CapabilityStateEmpty {
			return false, "capability_missing", nil
		}
		if caps.Revision != cmd.ExpectedRevision {
			return false, "stale_capability", nil
		}
	}
	if !aiRuntimeGenerationOK(cmd.ExpectedGeneration, ctl.Generation) {
		return false, "stale_generation", nil
	}
	if !enable {
		if _, err := tx.SQL().ExecContext(tx.Context(),
			`UPDATE ai_agent SET actor_enabled=0 WHERE key=? AND deleted_ts=0`, cmd.AgentKey); err != nil {
			return false, "", err
		}
		if err := tx.DeleteActor(cmd.AgentKey); err != nil {
			return false, "", err
		}
		// 禁用增加 generation：所有旧代次的建议/相对操作永久失效。
		if _, err := tx.BumpControl(cmd.AgentKey, 1, false); err != nil {
			return false, "", err
		}
		return true, "", nil
	}
	if !agentEnabled {
		return false, "agent_disabled", nil
	}
	if _, err := tx.SQL().ExecContext(tx.Context(),
		`UPDATE ai_agent SET actor_enabled=1 WHERE key=? AND deleted_ts=0`, cmd.AgentKey); err != nil {
		return false, "", err
	}
	// 状态有变化就推进 revision；已是开启态时保留原语义只记终态。
	if !actorEnabled {
		// 打开开关只代表「可以钓鱼」；状态行由下一次 tick 在同事务里初始化。
		if _, err := tx.BumpControl(cmd.AgentKey, 0, false); err != nil {
			return false, "", err
		}
		return true, "", nil
	}
	return true, "already_enabled", nil
}

// Test barrier immediately before actor/catch transactions acquire write ownership.
var aiRuntimeWriteBarrierForTest func()

func aiRuntimeStopped(tx *store.AIRuntimeTx) bool {
	epoch, _ := store.AIIntentEpoch(tx.Context())
	active, _ := aiconfig.EvaluateEmergency(store.ReadAIEmergency(tx.Context(), tx.SQL())).IntentActive(epoch)
	return !active
}

func (s *Server) applyAIActorReroll(tx *store.AIRuntimeTx, cmd *store.AIRuntimeCommand, now float64) (bool, string, error) {
	_, _, actorEnabled, found, err := tx.AgentFlags(cmd.AgentKey)
	if err != nil {
		return false, "", err
	}
	ctl, err := tx.Control(cmd.AgentKey)
	if err != nil {
		return false, "", err
	}
	if ctl.Tombstoned || !found {
		return false, "agent_missing", nil
	}
	if !aiRuntimeGenerationOK(cmd.ExpectedGeneration, ctl.Generation) {
		return false, "stale_generation", nil
	}
	if !aiRuntimeRevisionOK(cmd.ExpectedRevision, ctl.Revision) {
		return false, "stale_revision", nil
	}
	if !actorEnabled {
		return false, "actor_disabled", nil
	}
	a, err := tx.Actor(cmd.AgentKey)
	if err != nil {
		return false, "", err
	}
	if a == nil {
		return false, "no_actor_state", nil
	}
	rng := mrand.New(mrand.NewSource(nowNanoSeed()))
	store.AIActorReroll(a, now, tx.ActorConfig(), rng.Float64)
	if err := tx.SaveActor(a); err != nil {
		return false, "", err
	}
	if _, err := tx.BumpControl(cmd.AgentKey, 0, false); err != nil {
		return false, "", err
	}
	return true, "", nil
}

func (s *Server) applyAIActorTargetSuggest(tx *store.AIRuntimeTx, cmd *store.AIRuntimeCommand, now float64) (bool, string, error) {
	payload, err := store.AIRuntimePayloadTargetSuggestFull(cmd.Payload)
	if err != nil {
		return false, "bad_payload", nil
	}
	fishKey, locKey := payload.FishKey, payload.LocKey
	agentEnabled, _, actorEnabled, found, err := tx.AgentFlags(cmd.AgentKey)
	if err != nil {
		return false, "", err
	}
	ctl, err := tx.Control(cmd.AgentKey)
	if err != nil {
		return false, "", err
	}
	if ctl.Tombstoned || !found {
		return false, "agent_missing", nil
	}
	if !agentEnabled {
		return false, "agent_disabled", nil
	}
	// 建议是相对操作：必须带上 BR 读到的代次/版本，晚到的旧建议直接拒绝。
	if cmd.ExpectedGeneration != ctl.Generation {
		return false, "stale_generation", nil
	}
	if cmd.ExpectedRevision != ctl.Revision {
		return false, "stale_revision", nil
	}
	if !actorEnabled {
		return false, "actor_disabled", nil
	}
	// 写事务内再验当前权限与配置修订（DEV-111 A）：
	// 撤权后旧命令不能再应用；旧修订说明管理员已保存新意图，不能回写。
	caps, err := store.AIReadCapabilityState(tx.Context(), tx.SQL(), cmd.AgentKey)
	if err != nil {
		return false, "", err
	}
	if payload.Capability != aiconfig.CapabilityAcceptTarget ||
		!caps.Has(aiconfig.CapabilityAcceptTarget) {
		return false, "capability_missing", nil
	}
	if payload.CapRevision <= 0 || caps.Revision != payload.CapRevision {
		return false, "stale_capability", nil
	}
	a, err := tx.Actor(cmd.AgentKey)
	if err != nil {
		return false, "", err
	}
	if a == nil {
		return false, "no_actor_state", nil
	}
	// 鱼必须在建议地图的真实 roster 里；不信 BR 的结论。
	var roster int
	if err := tx.SQL().QueryRowContext(tx.Context(),
		`SELECT COUNT(*) FROM map_fish_roster WHERE loc=? AND fish_key=?`,
		locKey, fishKey).Scan(&roster); err != nil {
		return false, "static_unavailable", nil
	}
	if roster == 0 {
		return false, "unknown_fish", nil
	}
	// T-108 第一闸：证据有效（上一步已验 roster）+ 无锁 + 已过硬驻留。
	// 旧 ai_actor_switch_chance 不再参与（行为概率归性格）。
	// 显式非法的性格配置不产出新的派生决定：直接拒绝，不猜一套默认值。
	personality, derivable := activityPersonalityInTx(tx, cmd.AgentKey)
	if !derivable {
		return false, "personality_invalid", nil
	}
	cfg := tx.ActorConfig()
	dwell := activityDwellFloor(cfg, personality)
	cooldown := int64(aiRuntimeIntSetting(tx, "ai_actor_switch_cooldown_s", 1800, 60, 21600))
	locked := a.SwitchTS > 0 && now-a.SwitchTS < float64(cooldown)
	elapsed := now - a.SinceTS
	if ok, reason := activityCanSwitch(true, locked, elapsed, dwell); !ok {
		if reason == "locked" {
			return false, "cooldown", nil
		}
		return false, reason, nil
	}
	// 同玩家同邀请去重：60 秒内最多 3 次；超过即拒绝，不缩硬底线。
	repeats, allowed := activityInvites.note(activityInviteKey(aiTaskUIDInTx(tx, cmd.ParentTaskID), fishKey, locKey), now)
	if !allowed {
		return false, "duplicate_invite", nil
	}
	// T-108 第二闸：W=σ(2s+e−p−1+d×u+min(0.6,0.2n))，W≥0.6 才接受。
	invested := a.TargetFish != "" && locKey != "" && a.Loc == locKey
	w := activitySwitchWeight(personality, invested, elapsed, dwell, activityTunables(cfg).ExtraSeconds, repeats)
	if !activitySwitchAccepted(w) {
		return false, "low_affinity", nil
	}
	name := fishKey
	var locName string
	_ = tx.SQL().QueryRowContext(tx.Context(), `
		SELECT COALESCE(NULLIF(lf.name_zh,''), NULLIF(lw.fish_zh,''), '')
		FROM map_fish_roster r
		LEFT JOIN loc_alias la ON la.alias=r.loc
		LEFT JOIN loc_fish lf ON lf.fish_key=r.fish_key AND lf.loc=la.loc
		LEFT JOIN loc_wiki_fish lw ON lw.fish_key=r.fish_key AND lw.loc=la.loc
		WHERE r.loc=? AND r.fish_key=? LIMIT 1`, locKey, fishKey).Scan(&name)
	if name == "" {
		name = fishKey
	}
	_ = tx.SQL().QueryRowContext(tx.Context(),
		`SELECT loc FROM loc_alias WHERE alias=? LIMIT 1`, locKey).Scan(&locName)
	if locName == "" {
		locName = locKey
	}
	a.TargetFish, a.TargetName = fishKey, name
	if a.Loc != locKey {
		a.Loc, a.LocName, a.LocSinceTS = locKey, locName, now
	}
	a.Activity = "fishing"
	a.SinceTS = now
	a.SwitchTS = now
	if a.ActivityUntilTS <= now {
		a.ActivityUntilTS = now + tx.ActorConfig().FishingMax
	}
	if err := tx.SaveActor(a); err != nil {
		return false, "", err
	}
	if _, err := tx.BumpControl(cmd.AgentKey, 0, false); err != nil {
		return false, "", err
	}
	return true, "", nil
}

func (s *Server) applyAIChannelSetMember(tx *store.AIRuntimeTx, cmd *store.AIRuntimeCommand) (bool, string, error) {
	channel, enable, err := store.AIRuntimePayloadChannel(cmd.Payload)
	if err != nil {
		return false, "bad_payload", nil
	}
	agentEnabled, joined, _, found, err := tx.AgentFlags(cmd.AgentKey)
	if err != nil {
		return false, "", err
	}
	ctl, err := tx.Control(cmd.AgentKey)
	if err != nil {
		return false, "", err
	}
	if ctl.Tombstoned || !found {
		return false, "agent_missing", nil
	}
	if !aiRuntimeGenerationOK(cmd.ExpectedGeneration, ctl.Generation) {
		return false, "stale_generation", nil
	}
	if !agentEnabled {
		return false, "agent_disabled", nil
	}
	// 公聊来源复刻 T-013 的口径：只有已接入游戏的 Agent 才能被拉进/移出频道。
	// 后台来源是显式管理动作，允许清理未接入的成员登记。
	if cmd.Source == store.AIRuntimeSourceGameServer && !joined {
		return false, "not_joined", nil
	}
	if err := tx.SetChannelAgent(channel, cmd.AgentKey, enable); err != nil {
		return false, "", err
	}
	if _, err := tx.BumpControl(cmd.AgentKey, 0, false); err != nil {
		return false, "", err
	}
	return true, "", nil
}

func (s *Server) applyAIMemoryClear(tx *store.AIRuntimeTx, cmd *store.AIRuntimeCommand) (bool, string, error) {
	if err := tx.ClearMemories(cmd.AgentKey); err != nil {
		return false, "", err
	}
	if _, err := tx.BumpControl(cmd.AgentKey, 0, false); err != nil {
		return false, "", err
	}
	return true, "", nil
}

func (s *Server) applyAIAgentDelete(tx *store.AIRuntimeTx, cmd *store.AIRuntimeCommand) (bool, string, error) {
	deleted, err := tx.DeleteAgent(cmd.AgentKey)
	if err != nil {
		return false, "", err
	}
	// 删除永远推进代次并留墓碑；control 行保留，禁止删除后代次归零。
	if _, err := tx.BumpControl(cmd.AgentKey, 1, true); err != nil {
		return false, "", err
	}
	if !deleted {
		return true, "already_deleted", nil
	}
	return true, "", nil
}

// submitAIChannelCommand 入队并立即应用一条公聊频道命令，返回终态 status。
func (s *Server) submitAIChannelCommand(ctx context.Context, hctx *Ctx, channel, agentKey string, enable bool) (string, error) {
	payload, err := json.Marshal(map[string]any{"channel": channel, "enabled": enable})
	if err != nil {
		return "", err
	}
	cmd, _, err := s.DB.AIEnqueueRuntimeCommand(ctx, store.AIRuntimeEnqueueRequest{
		RequestID: aiChannelCommandRequestID(hctx, channel, agentKey, enable),
		Source:    store.AIRuntimeSourceGameServer,
		Kind:      store.AIRuntimeKindChannelSetMember,
		AgentKey:  agentKey,
		Payload:   payload,
		TTL:       30 * time.Second,
	})
	if err != nil {
		return "", err
	}
	if cmd.Status != store.AIRuntimeStatusPending {
		return cmd.Status, nil
	}
	applied, err := s.DB.AIApplyRuntimeCommand(ctx, cmd.ID, 0, s.applyAIRuntimeCommand)
	if err != nil {
		// 瞬时失败/超时：命令仍是 pending，下一轮消费者会应用；回执保持“已受理”。
		return store.AIRuntimeStatusPending, err
	}
	return applied.Status, nil
}

// aiChannelCommandRequestID 复用官方请求身份（token 哈希 + main/sub/call_id）
// 构成稳定 request_id：同一次客户端请求重放不会重复改成员。
func aiChannelCommandRequestID(hctx *Ctx, channel, agentKey string, enable bool) string {
	tokenSum := sha256.Sum256([]byte(hctx.Sess.Token))
	raw := "gs-channel\x00" + hex.EncodeToString(tokenSum[:]) + "\x00" +
		strconv.FormatUint(uint64(hctx.Main), 10) + "\x00" +
		strconv.FormatUint(uint64(hctx.Sub), 10) + "\x00" +
		strconv.FormatUint(uint64(hctx.CallID), 10) + "\x00" +
		channel + "\x00" + agentKey + "\x00" + strconv.FormatBool(enable)
	sum := sha256.Sum256([]byte(raw))
	return "gs:" + hex.EncodeToString(sum[:16])
}

// nowNanoSeed 给应用事务内的掷骰提供独立随机源（不复用游戏 RNG）。
func nowNanoSeed() int64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		var seed int64
		for _, x := range b {
			seed = seed<<8 | int64(x)
		}
		return seed
	}
	return time.Now().UnixNano()
}

// activityPersonalityInTx 在运行态事务里读角色四维。
//
// 连接池只有一条连接，事务内不能开第二条连接，所以必须用 tx.SettingRaw；
// 缺值 → 中性默认且可派生；显式非法 → 不可派生（不产出新的派生决定）。
func activityPersonalityInTx(tx *store.AIRuntimeTx, agentKey string) (aiconfig.PersonalityConfig, bool) {
	raw, ok, err := tx.SettingRaw(personalitySettingKey(agentKey))
	if err != nil {
		return aiconfig.DefaultPersonality(), false
	}
	if !ok {
		return aiconfig.DefaultPersonality(), true
	}
	return parsePersonalitySetting(raw)
}

// aiTaskUIDInTx 读父任务上的玩家 UID：同玩家同邀请的去重键需要它。
// 读不到就返回 0（去重退化为按目标 + 地图），不阻断建议本身。
func aiTaskUIDInTx(tx *store.AIRuntimeTx, taskID int64) int64 {
	if taskID <= 0 {
		return 0
	}
	var uid int64
	if err := tx.SQL().QueryRowContext(tx.Context(),
		`SELECT COALESCE(uid,0) FROM ai_task WHERE id=?`, taskID).Scan(&uid); err != nil {
		return 0
	}
	return uid
}

// aiRuntimeIntSetting 在事务内读取并夹紧整数设置。
func aiRuntimeIntSetting(tx *store.AIRuntimeTx, key string, def, lo, hi int) int {
	raw := tx.Setting(key, "")
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
