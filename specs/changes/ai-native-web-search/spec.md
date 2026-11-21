# AI 厂商原生搜索

Status: done

## Problem Statement

现有联网开关需要管理员配置第三方搜索地址。DeepSeek 已支持在对话请求中使用厂商执行的搜索，管理员希望复用现有的模型连接。

## Solution

连接页可选择仅本地、厂商原生搜索或自定义搜索服务。首版支持 DeepSeek 官方 Chat Completions 的 deepseek-flash。其他连接继续支持原有聊天和本地资料。

## User Stories

- 管理员使用符合条件的 DeepSeek 连接时，可以启用原生搜索并留空搜索地址。
- 角色获得联网能力且本地未命中才发起搜索。联网失败时，仍尝试走普通聊天。
- 已配置自定义搜索地址的旧存档继续使用原有方式。

## Implementation Decisions

- 复用 ai_setting 和现有设置 API，保存模式时枚举校验。没有模式记录时，用是否保存过自定义地址推导原有行为。
- DeepSeek 独立搜索回合只发清理后的当前问题，使用当前凭据和出口判定，关闭深度思考并限制时间。正常对话继续保留其游戏工具。根据已完成的服务端搜索记录确认是否联网。
- 连接页显示兼容状态和当前活动修订的最近确认时间。保留全局开关、逐角色授权、急停和任务范围复核，无数据库迁移。厂商参考见 [DeepSeek 联网工具](https://api-docs.deepseek.com/zh-cn/guides/web_tools/#chat-completions-api)。

## Testing Decisions

- 模拟 DeepSeek 验证实际请求体、确认字段、无结果、HTTP 内错误和回退，避免联网参数进入普通厂商请求。
- 验证本地命中、无权限和模式不匹配时不会原生搜索，并检查旧配置推导与页面选择。

## Out of Scope

其他厂商原生搜索、原生搜索与游戏函数同请求混用，以及网页抓取功能单独接入。
