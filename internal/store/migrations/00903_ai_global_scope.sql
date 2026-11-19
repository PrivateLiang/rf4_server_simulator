-- 全局回复改成「逐个添加」而不是一键开关。
--
-- 原设计：ai_global_subscribe=1 时所有已接入 Agent 在任何频道都能说话。
-- 问题：范围太大 —— 一键就把全部助手拉进来，没法只放一两个常驻的。
--
-- 新设计：全局也是一个「范围」，用 ai_map_agent 的特殊 map_key='*' 表示。
-- 想让某个助手常驻，就把它加进全局范围；不加就还是只在订阅的地图/频道出现。
-- 表结构不变，读写复用同一套函数，只是多一种 map_key 取值。
--
-- 清掉旧的两个开关设置：include_silent 在新模型下不再需要 ——
-- 全局范围里的 Agent 是人工挑的，要不要它说话由 reply_mode 决定，
-- 不再由一个全局开关统一决定。

DELETE FROM ai_setting WHERE key IN ('ai_global_subscribe', 'ai_global_subscribe_include_silent');
