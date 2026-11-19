package gameserver

// 跨进程工具执行：aibridge 是独立进程，无法直接调用 gameserver 的 QueryFish。
// 工具请求写进 ai_tool_job，由 gameserver 在这里执行，结果写回同一张表。
// 这样 AI 工具与「查鱼助手」共用同一条产线权重链，不另写一套公式。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"rf4server/internal/aiconfig"
	"rf4server/internal/store"
)

const aiToolJobBudget = 3 * time.Second

var aiToolJobBarrierForTest func(stage string)

// gsReadTool 是 GS 侧只读工具的唯一声明表：名字 → 执行函数。
// 与 aibridge 注册表同名同路由，未知名字返回「未知工具」，不再用 switch。
type gsReadTool func(ctx context.Context, s *Server, job store.AIToolJob) string

var gsReadTools = map[string]gsReadTool{
	aiconfig.ToolFishQuery: func(ctx context.Context, s *Server, j store.AIToolJob) string {
		return s.fishQueryForUID(ctx, j.UID, j.Args)
	},
	aiconfig.ToolMapVibe: func(ctx context.Context, s *Server, j store.AIToolJob) string {
		return s.mapVibeText(ctx, j.Args)
	},
}

// fishQueryForUID 复用现有查鱼回复函数；它只读取 sess.Userid，
// 用一个轻量 Session 即可，无需玩家连接在线。
func (s *Server) fishQueryForUID(ctx context.Context, uid int64, text string) string {
	cctx, cancel := context.WithTimeout(ctx, aiToolJobBudget)
	defer cancel()
	return s.fishQueryReply(cctx, &Session{Srv: s, Userid: uid}, text)
}

// pollAIToolJobs 由 StartAdminSubscriber 的 2 秒 ticker 调用。
func (s *Server) pollAIToolJobs(ctx context.Context, db *store.DB) {
	if db == nil || s.DB == nil {
		return
	}
	jobs, err := db.AIPendingToolJobs(ctx, 8)
	if err != nil {
		s.log.Warn("AI 工具请求读取失败", "err", err)
		return
	}
	for _, j := range jobs {
		if aiToolJobBarrierForTest != nil {
			aiToolJobBarrierForTest("claimed")
		}
		finish := func(result string) {
			if aiToolJobBarrierForTest != nil {
				aiToolJobBarrierForTest("writeback")
			}
			if err := db.AICompleteToolJob(ctx, j.ID, result); err != nil {
				s.log.Warn("AI 工具结果写入失败", "id", j.ID, "err", err)
			}
		}
		active, err := db.AIToolJobActive(ctx, j.ID)
		if err != nil {
			s.log.Warn("AI 工具代次读取失败", "id", j.ID, "err", err)
			continue
		}
		if !active {
			finish("")
			continue
		}
		// 未知工具先判定：它不是权限问题，不能被误报成「授权已撤销」。
		run, ok := gsReadTools[j.Tool]
		if !ok {
			finish("未知工具")
			continue
		}
		// 执行前按当前能力复核：撤权后旧作业不能再拿到数据。
		state, capErr := store.AIReadCapabilityState(ctx, db.SQL, j.Agent)
		if capErr != nil {
			s.log.Warn("AI 能力读取失败", "id", j.ID, "agent", j.Agent, "err", capErr)
			finish("")
			continue
		}
		if !state.ToolAllowed(j.Tool) {
			finish("（工具不可用：授权已撤销）")
			continue
		}
		finish(run(ctx, s, j))
	}
}

// weatherKindZh 把天气档转成中文短语。
func weatherKindZh(kind int) string {
	switch kind {
	case fishingWeatherClear:
		return "晴"
	case fishingWeatherFewClouds:
		return "少云"
	case fishingWeatherCloudy:
		return "多云"
	case fishingWeatherRain:
		return "小雨"
	case fishingWeatherHeavyRain:
		return "大雨"
	case fishingWeatherThunderstorm:
		return "雷雨"
	}
	return ""
}

// mapWeatherLine 取某张图当前天气的一句话描述；取不到返回空串。
// 复用客户端天气计划的同一套 hour 序列，只是临时建一个不挂在连接上的 Session。
func (s *Server) mapWeatherLine(loc string) string {
	if loc == "" {
		return ""
	}
	sess := &Session{Srv: s}
	now := time.Now()
	sess.rememberWeather(loc, now.Unix())
	w := sess.weatherAt(loc, now)
	if !w.known {
		return ""
	}
	desc := weatherKindZh(w.kind)
	if desc == "" {
		return ""
	}
	return fmt.Sprintf("%s %.1f℃，风 %.1f m/s", desc, w.airTemp, w.wind)
}

// mapVibeText 是「陪玩」工具：本图天气 + 有哪些鱼。
// 故意不返回饵料：一旦给了饵，所有角色都会退化成查饵机器人，聊天就没了。
func (s *Server) mapVibeText(ctx context.Context, loc string) string {
	if s.DB == nil || loc == "" {
		return ""
	}
	// 只认能解析成中文水域名的 key，避免拿 level_xxx 去查空表。
	var locName string
	_ = s.DB.SQL.QueryRowContext(ctx,
		`SELECT loc FROM loc_alias WHERE alias=?`, loc).Scan(&locName)
	if locName == "" {
		for _, r := range loc {
			if r >= 0x4e00 && r <= 0x9fff {
				locName = loc
				break
			}
		}
	}
	if locName == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("地图：" + locName + "\n")
	if w := s.mapWeatherLine(loc); w != "" {
		b.WriteString("此刻天气：" + w + "\n")
	}
	rows, err := s.DB.SQL.QueryContext(ctx, `
		SELECT w.fish_zh
		FROM loc_wiki_fish w
		LEFT JOIN species_grade g ON g.fish_key=w.fish_key
		WHERE w.loc=? AND w.in_map_roster=1 AND w.fish_zh<>''
		ORDER BY COALESCE(g.catches,0) DESC
		LIMIT 12`, locName)
	if err == nil {
		fish := make([]string, 0, 12)
		for rows.Next() {
			var n string
			if rows.Scan(&n) == nil && n != "" {
				fish = append(fish, n)
			}
		}
		_ = rows.Close()
		if len(fish) > 0 {
			b.WriteString("这片水域有：" + strings.Join(fish, "、") + "\n")
		}
	}
	b.WriteString("（只到鱼种和天气这一层，别背数据，就当闲聊。）")
	return b.String()
}
