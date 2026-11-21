package gameserver

// 频道级「说话预算」：上鱼播报、闲时搭话、圆桌共用同一份额度。
//
// 为什么不再只靠上鱼配额：上鱼是稀疏事件，真正决定频道体感的是「一小时总共
// 听到几句」。预算把「说多少」从「钓多少」里拆出来，管理员只选一个氛围档。
//
// 形状用整点小时的四段权重累积：每个时刻有一个「到此刻为止允许说几句」的
// 曲线，突发容量 C 允许超前 C 句。这样既不会在整点后饿死，也不会把一小时的
// 话全挤在前十分钟。
//
// 边界：频道级、内存态、进程重启清零；不写数据库、不碰协议。

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"rf4server/internal/aiconfig"
)

const (
	// 闲时搭话最多给上鱼播报留 2 句；实际预留按突发容量推导，
	// 小预算档（冷清 B=3/C=1）不可能留 2 句，否则闲时永远开不了口。
	aiTalkReserveMax   = 2
	aiTalkCustomBudget = 6
	aiTalkCustomGapS   = 480
	aiTalkCustomBurst  = 2
)

// aiTalkReserveFor 返回闲时搭话必须给上鱼播报留下的句数。
func aiTalkReserveFor(cfg aiTalkConfig) int {
	r := cfg.Burst - 1
	if r < 1 {
		r = 1
	}
	if r > aiTalkReserveMax {
		r = aiTalkReserveMax
	}
	if limit := cfg.Budget - 1; r > limit {
		r = limit
	}
	if r < 0 {
		r = 0
	}
	return r
}

// aiTalkConfig 是某一刻解析后的预算参数。
type aiTalkConfig struct {
	Mood    string
	Budget  int
	MinGap  time.Duration
	Burst   int
	Weights [4]float64
}

type aiTalkState struct {
	mu     sync.Mutex
	hour   time.Time
	spent  int
	lastAt time.Time
}

var aiTalkStates sync.Map // channel -> *aiTalkState

func aiTalkStateFor(channel string) *aiTalkState {
	if v, ok := aiTalkStates.Load(channel); ok {
		return v.(*aiTalkState)
	}
	st := &aiTalkState{}
	v, _ := aiTalkStates.LoadOrStore(channel, st)
	return v.(*aiTalkState)
}

// aiTalkConfig 读当前氛围；未知/缺失/损坏一律按「正常」，不放大音量。
func (s *Server) aiTalkConfig(ctx context.Context) aiTalkConfig {
	id := s.DB.AISetting(ctx, "ai_fishing_mood", "normal")
	if id == aiconfig.TalkMoodCustom {
		b := s.aiIntSetting(ctx, "ai_talk_hourly_limit", aiTalkCustomBudget, 1, 24)
		gap := time.Duration(s.aiIntSetting(ctx, "ai_talk_min_gap_s", aiTalkCustomGapS, 30, 900)) * time.Second
		burst := s.aiIntSetting(ctx, "ai_talk_burst", aiTalkCustomBurst, 1, 4)
		if burst > b {
			burst = b
		}
		var w [4]float64
		for i := range w {
			w[i] = s.aiFloatSetting(ctx, fmt.Sprintf("ai_talk_weight_%d", i), 1, 0, 3)
		}
		return aiTalkConfig{Mood: aiconfig.TalkMoodCustom, Budget: b, MinGap: gap, Burst: burst, Weights: w}
	}
	m, ok := aiconfig.TalkMoodByID(id)
	if !ok {
		m, _ = aiconfig.TalkMoodByID("normal")
	}
	return aiTalkConfig{
		Mood:    m.ID,
		Budget:  m.Budget,
		MinGap:  time.Duration(m.MinGapSec) * time.Second,
		Burst:   m.Burst,
		Weights: m.Weights,
	}
}

// aiTalkHourStart 返回当前整点（按本地时区），用于四段形状与小时重置。
// 不用 Time.Truncate：它对半小时时区会截到 :30，形状会整体错位。
func aiTalkHourStart(now time.Time) time.Time {
	return time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, now.Location())
}

// aiTalkAllowedAt 返回从整点起累计允许说出的事件数（含突发容量）。
//
// 曲线：weight 归一化后乘以预算；突发容量 C 表示可以超前 C 句，
// 因此小时开头至少能说 C 句，整点前后也不会因为曲线归零而卡住。
func aiTalkAllowedAt(now time.Time, cfg aiTalkConfig) int {
	b := cfg.Budget
	if b <= 0 {
		return 0
	}
	c := cfg.Burst
	if c < 1 {
		c = 1
	}
	if c > b {
		c = b
	}
	frac := now.Sub(aiTalkHourStart(now)).Seconds() / 3600
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	total := 0.0
	for _, w := range cfg.Weights {
		if w > 0 {
			total += w
		}
	}
	weights := cfg.Weights
	if total <= 0 {
		// 全 0 视作均匀，而不是永久沉默。
		total = 4
		weights = [4]float64{1, 1, 1, 1}
	}
	cum := 0.0
	for i := 0; i < 4; i++ {
		segStart := float64(i) * 0.25
		if frac <= segStart {
			break
		}
		w := weights[i]
		if w < 0 {
			w = 0
		}
		covered := math.Min(frac, segStart+0.25) - segStart
		cum += w * covered / 0.25
	}
	allowed := int(math.Floor(float64(b)*cum/total)) + c
	if allowed > b {
		allowed = b
	}
	if allowed < c {
		allowed = c
	}
	return allowed
}

// aiTalkTake 是纯判定：够预算/够间隔就扣一格。priority 表示上鱼播报这类
// 高光事件——它不看最小间隔，但同样受小时总额约束；闲时搭话要给上鱼留一句。
func aiTalkTake(now time.Time, st *aiTalkState, cfg aiTalkConfig, priority bool) bool {
	if cfg.Budget <= 0 {
		// 不限档：没有小时上限，只保留最小间隔；上鱼播报可以插队。
		if !priority && !st.lastAt.IsZero() && now.Sub(st.lastAt) < cfg.MinGap {
			return false
		}
		st.lastAt = now
		return true
	}
	hour := aiTalkHourStart(now)
	if !st.hour.Equal(hour) {
		st.hour = hour
		st.spent = 0
	}
	if st.spent >= cfg.Budget {
		return false
	}
	allowed := aiTalkAllowedAt(now, cfg)
	if priority {
		if st.spent >= allowed {
			return false
		}
	} else {
		if !st.lastAt.IsZero() && now.Sub(st.lastAt) < cfg.MinGap {
			return false
		}
		if st.spent+aiTalkReserveFor(cfg) >= allowed {
			return false
		}
	}
	st.spent++
	st.lastAt = now
	return true
}

// aiRoundtableProfile 把氛围档解析成圆桌人数/条数/冷却/时限。
// custom 档只多读一个「最多几人接话」，其余沿用正常档。
func (s *Server) aiRoundtableProfile(ctx context.Context) aiconfig.RoundtableProfile {
	mood := s.DB.AISetting(ctx, "ai_fishing_mood", "normal")
	if mood == aiconfig.TalkMoodCustom {
		speakers := s.aiIntSetting(ctx, "ai_talk_roundtable_speakers", 5, 2, aiconfig.RoundtableMaxParticipants)
		return aiconfig.CustomRoundtableProfile(speakers)
	}
	if p, ok := aiconfig.TalkMoodRoundtable(mood); ok {
		return p
	}
	p, _ := aiconfig.TalkMoodRoundtable("normal")
	return p
}

// aiTalkAllow 是运行期唯一入口：读配置、取频道状态、扣一格。
func (s *Server) aiTalkAllow(ctx context.Context, channel string, priority bool) bool {
	if s == nil || s.DB == nil || channel == "" {
		return false
	}
	cfg := s.aiTalkConfig(ctx)
	st := aiTalkStateFor(channel)
	st.mu.Lock()
	defer st.mu.Unlock()
	return aiTalkTake(time.Now(), st, cfg, priority)
}
