// T-105 浏览器夹具：四步开聊、五模块与能力保存/生效。
//
// 运行：node internal/admin/ai_onboarding_ui_test.cjs
// 需要 Playwright（playwright 或 playwright-core）与 Chromium：
//   RF4_TEST_BROWSER 指向 chrome.exe；未设置时尝试本机 ms-playwright 缓存。
// 夹具只用路由拦截提供后台外壳与 /api/ai/*，不连接真实服务、不改仓库文件。
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const os = require('node:os');

let chromium = null;
try {
  ({ chromium } = require('playwright'));
} catch (e) {
  try {
    ({ chromium } = require('playwright-core'));
  } catch (e2) {
    chromium = null;
  }
}
if (!chromium) {
  throw new Error('需要 playwright 或 playwright-core；本机可用 NODE_PATH 指向安装目录后重跑');
}

const ROOT = __dirname;
const SHELL = fs.readFileSync(path.join(ROOT, 'web/legacy.html'), 'utf8');
const AI_INDEX = fs.readFileSync(path.join(ROOT, 'web/ai/index.html'), 'utf8');
const AI_JS = fs.readFileSync(path.join(ROOT, 'web/ai/ai.js'), 'utf8');
const AI_CSS = fs.readFileSync(path.join(ROOT, 'web/ai/ai.css'), 'utf8');

const BANNED_UI = [
  'aiNewUID', 'aiNewTools', 'aiEditTools', 'aiTestFishingAgent',
  '说话方式', '活跃度', '工具白名单', '上鱼报话', '角色模拟', '发鱼测试',
  '请服主分配角色',
];

function browserPath() {
  if (process.env.RF4_TEST_BROWSER) return process.env.RF4_TEST_BROWSER;
  const base = path.join(os.homedir(), 'AppData/Local/ms-playwright');
  const candidates = [
    path.join(base, 'chromium-1237/chrome-win64/chrome.exe'),
    path.join(base, 'chromium_headless_shell-1237/chrome-headless-shell-win64/chrome-headless-shell.exe'),
  ];
  return candidates.find((item) => fs.existsSync(item)) || undefined;
}

function spec(key, module, group, kind, def, min, max, label, extra) {
  return {
    key, module, group, label: label || key, hint: '', kind, def,
    min: min || 0, max: max || 0, advanced: false, ...(extra || {}),
  };
}

function budgetAndFishingSpecs() {
  const list = [
    // conn
    spec('web_search_enabled', 'conn', 'search', 'bool', '0', 0, 0, '联网搜索'),
    spec('emergency_stop', 'conn', 'safety', 'bool', '0', 0, 0, '紧急停止'),
    spec('search_endpoint', 'conn', 'search', 'string', '', 0, 0, '检索端点', { advanced: true }),
    spec('max_tool_rounds', 'conn', 'search', 'int', '2', 1, 4, '工具轮数', { advanced: true }),
    // budget
    spec('ai_profile_channel_context_tokens', 'budget', 'channel', 'int', '10000', 800, 30000, '频道上下文 tokens'),
    spec('ai_profile_channel_history_messages', 'budget', 'channel', 'int', '150', 0, 300, '频道历史条数'),
    spec('ai_profile_channel_history_chars', 'budget', 'channel', 'int', '6000', 200, 20000, '频道历史字数'),
    spec('ai_profile_channel_reply_soft_chars', 'budget', 'channel', 'int', '25', 10, 200, '频道回复软目标'),
    spec('ai_profile_channel_reply_chars', 'budget', 'channel', 'int', '45', 20, 500, '频道回复硬上限'),
    spec('ai_profile_dm_context_tokens', 'budget', 'dm', 'int', '12000', 1000, 30000, '私聊上下文 tokens'),
    spec('ai_profile_dm_history_messages', 'budget', 'dm', 'int', '200', 0, 300, '私聊历史条数'),
    spec('ai_profile_dm_history_chars', 'budget', 'dm', 'int', '8000', 0, 20000, '私聊历史字数'),
    spec('ai_profile_dm_reply_soft_chars', 'budget', 'dm', 'int', '80', 20, 800, '私聊回复软目标'),
    spec('ai_profile_dm_reply_chars', 'budget', 'dm', 'int', '120', 100, 2000, '私聊回复硬上限'),
    spec('ai_profile_roundtable_context_tokens', 'budget', 'roundtable', 'int', '4000', 800, 12000, '圆桌上下文 tokens'),
    spec('ai_profile_roundtable_context_messages', 'budget', 'roundtable', 'int', '2', 1, 4, '圆桌历史条数'),
    spec('ai_profile_roundtable_reply_soft_chars', 'budget', 'roundtable', 'int', '20', 10, 120, '圆桌回复软目标'),
    spec('ai_profile_roundtable_reply_chars', 'budget', 'roundtable', 'int', '30', 30, 300, '圆桌回复硬上限'),
    spec('ai_profile_internal_chars', 'budget', 'profile', 'int', '800', 200, 4000, '内部资料字数', { advanced: true }),
    // fishing
    spec('ai_fishing_enabled', 'fishing', 'sim', 'bool', '0', 0, 0, '虚拟上鱼总开关'),
    spec('ai_fishing_hourly_limit', 'fishing', 'sim', 'int', '12', 1, 24, '每小时发言上限'),
    spec('ai_fishing_base_interval_ms', 'fishing', 'sim', 'int', '300000', 60000, 1800000, '基础节拍 ms'),
    spec('ai_fishing_max_interval_ms', 'fishing', 'sim', 'int', '600000', 60000, 3600000, '节拍封顶 ms'),
    spec('ai_fishing_star_weight', 'fishing', 'rare', 'float', '0.02', 0, 1, '上星占比'),
    spec('ai_fishing_super_weight', 'fishing', 'rare', 'float', '0.002', 0, 1, '上蓝占比'),
    spec('ai_fishing_miss_weight', 'fishing', 'rare', 'float', '0.33', 0, 1, '未达标占比'),
    spec('ai_fishing_near_weight', 'fishing', 'rare', 'float', '0.12', 0, 1, '下饭鱼占比'),
    spec('ai_fishing_miss_ratio', 'fishing', 'rare', 'float', '0.35', 0.05, 0.6, '未达标分界'),
    spec('ai_fishing_near_ratio', 'fishing', 'rare', 'float', '0.9', 0.6, 0.99, '下饭分界'),
    spec('ai_fishing_talk_enabled', 'fishing', 'talk', 'bool', '0', 0, 0, 'AI 上鱼主动说话'),
    spec('ai_fishing_talk_super_chance', 'fishing', 'talk', 'float', '1', 0, 1, '上蓝发言概率'),
    spec('ai_fishing_talk_star_chance', 'fishing', 'talk', 'float', '0.9', 0, 1, '上星发言概率'),
    spec('ai_fishing_talk_near_chance', 'fishing', 'talk', 'float', '0.35', 0, 1, '下饭鱼发言概率'),
    spec('ai_fishing_talk_miss_chance', 'fishing', 'talk', 'float', '0.25', 0, 1, '未达标发言概率'),
    spec('ai_fishing_roundtable_chance', 'fishing', 'talk', 'float', '0.5', 0, 1, '上蓝升级圆桌概率'),
    spec('ai_memory_inject', 'fishing', 'inject', 'bool', '1', 0, 0, '注入战绩摘要'),
    spec('ai_memory_recent', 'fishing', 'inject', 'int', '3', 0, 5, '最近几条'),
    spec('ai_memory_best', 'fishing', 'inject', 'int', '2', 0, 5, '最佳几条'),
    spec('ai_memory_max_rows', 'fishing', 'inject', 'int', '50', 5, 200, '最多保留'),
    spec('ai_catch_brief_inject', 'fishing', 'inject', 'bool', '1', 0, 0, '知道自己上过鱼'),
  ];
  return list;
}

function settingsFrom(specs) {
  const out = {};
  for (const item of specs) out[item.key] = item.def;
  return out;
}

function stepsFor(empty, helloDone) {
  const base = [
    { id: 'connection', title: '保存模型厂商与密钥并复测', budget_s: 90, state: 'done', display: '已保存并通过复测（api.deepseek.com / deepseek-chat / 修订 3）', action: 'retest' },
    { id: 'create', title: '创建 Agent', budget_s: 45, state: empty ? 'current' : 'done', display: empty ? '还没有角色：填 Key 与展示名即可新增' : '已有 2 个角色；联系人 UID 由服务端自动分配', action: 'create' },
    { id: 'join', title: '接入游戏并找到联系人', budget_s: 45, state: empty ? 'blocked' : 'done', display: empty ? '先创建角色' : '1 个角色已有游戏端证据（私聊或邮件）', action: 'join' },
    { id: 'hello', title: '在游戏私聊发「你好」并收到回复', budget_s: 60, state: empty ? 'blocked' : (helloDone ? 'done' : 'current'), display: empty ? '先接入游戏' : (helloDone ? '已收到游戏内回复，游戏服已投递到个人消息' : '私聊已入队，等待 AI 节点回复'), action: 'say_hello' },
  ];
  return base;
}

function richAgent(key, display, joined, visible, extra) {
  return {
    key, display, role: '钓友', contact_uid: key === 'hutao' ? 2147480001 : 2147480002,
    joined, contact_visible: visible, visible_evidence: visible ? 'dm' : '',
    pending_commands: 0, catch_total: key === 'hutao' ? 12 : 0, catch_today: key === 'hutao' ? 2 : 0,
    catch_total_g: 15000, catch_today_g: 2500, memory_total: 3,
    last_dm_ts: visible ? 1710000000 : 0, last_reply_status: visible ? 'done' : '',
    last_delivered_ts: visible ? 1710000001 : 0, last_mail_ts: visible ? 1710000001 : 0,
    generation: 1, revision: 3, tools: '[]', tools_list: [],
    prompt: '你是本地鱼情顾问，只依据内部资料回答。', thinking_enabled: true, thinking_effort: 'medium',
    reply_mode: 'social', talkativeness: 50, actor_enabled: joined,
    actor: joined ? { activity: 'fishing', loc_name: '拉多加湖群岛', target_name: '茴鱼', since_ts: Date.now() / 1000 - 3600, energy: 70 } : null,
    ...(extra || {}),
  };
}

// 首句证据只认「status=done 且 GS 已投递」，失败提示邮件不参与完成判定。
function replyDisplay(agent) {
  if (!agent.last_reply_status) return '还没有回复记录';
  if (agent.last_reply_status === 'fallback') return '回复失败：' + (agent.last_reply_error || '原因待确认');
  if (Number(agent.last_delivered_ts) > 0) return '已投递';
  return '回复已生成，等待游戏服投递';
}

function statusFixture(opts) {
  const empty = !!opts.empty;
  const specs = budgetAndFishingSpecs();
  const agents = empty ? [] : (opts.agents || [richAgent('hutao', '胡桃', true, true), richAgent('klee', '可莉', false, false)]);
  const deliveredTotal = agents.filter((agent) => Number(agent.last_delivered_ts) > 0).length;
  const onboardingAgents = {};
  for (const agent of agents) {
    onboardingAgents[agent.key] = {
      contact_visible: agent.contact_visible,
      visible_evidence: agent.visible_evidence,
      visible_display: agent.contact_visible ? '玩家已在游戏里私聊过它（联系人可见）' : '管理配置已保存；还没有游戏端证据',
      last_dm: { id: agent.last_dm_ts ? 1 : 0, state: 'done', ts: agent.last_dm_ts },
      last_reply: { status: agent.last_reply_status, error: agent.last_reply_error || '', display: replyDisplay(agent), ts: agent.last_dm_ts, delivered_ts: agent.last_delivered_ts },
      last_mail_ts: agent.last_mail_ts,
      pending_commands: 0,
      catch_total: agent.catch_total, catch_total_g: agent.catch_total_g,
      catch_today: agent.catch_today, catch_today_g: agent.catch_today_g,
      memory_total: agent.memory_total, memory_star: 2, memory_super: 1, unknown: false,
    };
  }
  const priority = opts.emergency ? 'emergency' : (empty ? 'ready' : 'ready');
  return {
    settings: settingsFrom(specs),
    specs,
    modules: [
      { id: 'conn', title: '连接', hint: '' },
      { id: 'agents', title: '角色与订阅', hint: '' },
      { id: 'budget', title: '说话与上下文', hint: '' },
      { id: 'fishing', title: 'AI 虚拟钓鱼', hint: '' },
      { id: 'personality', title: '性格与交互', hint: '' },
      { id: 'legacy', title: '旧参数（保留兼容）', hint: '' },
    ],
    security: { keys: { ai_actor_enabled: { state: 'unknown', display: '未确认' } } },
    provider: 'deepseek', base_url: 'https://api.deepseek.com', model_id: 'deepseek-chat',
    api_key_masked: 'sk-***', agents,
    last_error: {}, bridge: { online: true }, queue: { pending: 0, processing: 0, tool_pending: 0, tool_processing: 0 },
    revision: 3, config_state: 'ok', config_source: 'admin_save',
    destination: { host: 'api.deepseek.com', base_url: 'https://api.deepseek.com', model: 'deepseek-chat' },
    credential: { state: 'configured', masked: 'sk-***', reason: '', display: '凭据已就绪' },
    retest: { revision: 3, ok: true, current: true, display: '此连接已通过复测', attempt: 'a1' },
    emergency_stop: {
      stopped: !!opts.emergency, fault: false,
      display: opts.emergency ? '急停已开启：所有模型调用已切断；已发出的请求可能仍会返回' : '急停已解除',
      epoch: '7',
    },
    saved_display: '此连接已通过复测 · api.deepseek.com / deepseek-chat / 修订 3',
    onboarding: {
      status: {
        priority,
        display: opts.emergency ? '急停已开启：所有模型调用已切断' : '可试说：四步均有证据',
        next: opts.emergency ? '到「连接」页解除紧急停止；解除后重新复测' : '保持游戏在线；就绪不等于玩家可见，以客户端回复为准',
      },
      connection: { state: 'done', display: '', retest_current: true, retest_ok: true },
      steps: stepsFor(empty, deliveredTotal > 0),
      first_sentence: {
        text: '你好',
        agent: empty ? '' : '胡桃',
        display: empty ? '先在「角色与订阅」创建一个角色，再接入游戏。' : '在游戏里打开联系人，找到「胡桃」，私聊发送「你好」；回复会进入个人消息。',
      },
      agents: onboardingAgents,
      agents_total: agents.length,
      joined_total: agents.filter((agent) => agent.joined).length,
      visible_total: agents.filter((agent) => agent.contact_visible).length,
      dm_total: agents.filter((agent) => agent.contact_visible).length,
      delivered_total: deliveredTotal,
      pending_commands: 0,
      buffer_s: 60,
      target_total_s: 300,
    },
  };
}

function catalogFixture() {
  return [
    { id: 'guide_help', label: '攻略帮助', desc: '允许查阅鱼种、饵料、点位、物品、价格、配方。', read_tools: ['species_lookup', 'bait_pref'], write_tools: [] },
    { id: 'reply_star', label: '回应玩家星蓝', desc: '玩家上星或上蓝时在系统频道接一句。', event: 'reply_star' },
    { id: 'join_roundtable', label: '参加圆桌', desc: '允许参加游戏服发起的圆桌。', event: 'roundtable' },
    { id: 'virtual_fishing', label: '虚拟垂钓', desc: '允许按游戏服原有节拍进行虚拟钓鱼。', event: 'virtual_fishing', requires_actor: true },
    { id: 'broadcast_catch', label: '播报自己渔获', desc: '自己已提交渔获后允许报一句。', event: 'broadcast_catch', requires_roundtable: true },
    { id: 'accept_target_suggest', label: '接受目标建议', desc: '允许提交换鱼/换图建议。', write_tools: ['actor_target_suggest'] },
  ];
}

function capabilityView(revision, capabilitySet, availability, command) {
  const caps = capabilitySet || ['guide_help', 'virtual_fishing'];
  const read = caps.includes('guide_help') ? ['fish_query', 'species_lookup', 'bait_pref', 'spot_lookup', 'item_lookup', 'shop_price', 'recipe_lookup', 'map_vibe'] : [];
  const write = caps.includes('accept_target_suggest') ? ['actor_target_suggest'] : [];
  return {
    key: 'hutao', present: true, revision, generation: 1, state: caps.length ? 'configured' : 'empty',
    capabilities: caps, read_tools: read, write_tools: write, legacy: false, reason: '',
    actor_enabled: caps.includes('virtual_fishing'),
    availability,
    runtime: {
      agent_key: 'hutao', revision, generation: 1, state: caps.length ? 'configured' : 'empty',
      desired: caps.includes('virtual_fishing'), actual: availability === 'available',
      availability, reason: availability === 'failed' ? 'stale_capability' : '',
      command_id: command ? command.id : 0, request_id: command ? 'cap1-x-r4-1' : '',
      status: command ? command.status : '', applied: !!(command && command.status === 'applied'),
      applied_ts: command && command.status === 'applied' ? 1710000002 : 0, result_revision: revision,
    },
    commands: command ? [command] : [],
  };
}

function json(route, body, status) {
  return route.fulfill({
    status: status || 200,
    contentType: 'application/json; charset=utf-8',
    body: JSON.stringify(body),
  });
}

async function openPage(opts) {
  const browser = await chromium.launch({ headless: true, executablePath: browserPath() });
  const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  await context.addInitScript(() => localStorage.setItem('rf4_admin_token', 'fixture'));
  const page = await context.newPage();
  const errors = [];
  page.on('pageerror', (error) => { errors.push(error.message); console.error('PAGEERROR', error.message); });
  page.on('console', (message) => { if (message.type() === 'error') console.error('CONSOLE', message.text()); });
  const state = {
    status: statusFixture(opts),
    capRevision: 3,
    capSet: ['guide_help', 'virtual_fishing'],
    capAvailability: 'available',
    capCommand: { id: 9, status: 'applied', enabled: true, revision: 3, reason: '', applied_ts: 1710000002 },
    capPosts: [],
    settingsPosts: [],
    memoryPosts: [],
    phase: 'initial',
  };
  await page.route('**/*', async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const pathname = url.pathname;
    if (pathname === '/') return route.fulfill({ contentType: 'text/html; charset=utf-8', body: SHELL });
    if (pathname === '/ai/ai.js') return route.fulfill({ contentType: 'text/javascript; charset=utf-8', body: AI_JS });
    if (pathname === '/ai/ai.css') return route.fulfill({ contentType: 'text/css; charset=utf-8', body: AI_CSS });
    if (pathname === '/ai/index.html') return route.fulfill({ contentType: 'text/html; charset=utf-8', body: AI_INDEX });
    if (pathname === '/api/me') return json(route, { username: 'admin' });
    if (pathname === '/api/ai/status') return json(route, state.status);
    if (pathname === '/api/ai/channels') return json(route, opts.empty
      ? { channel: 'nav1_03', agents: [], subscribed: [], channels: [], maps: [] }
      : {
        channel: 'nav1_03', agents: state.status.agents, subscribed: ['hutao'],
        channels: [
          { name: 'nav1_03', label: '', count: 1, agents: ['胡桃'], agent_keys: ['hutao'] },
          { name: 'nav1_04', label: '', count: 1, agents: ['胡桃'], agent_keys: ['hutao'] },
        ],
        maps: [
          { map_key: '*', label: '全局', count: 1, agents: ['胡桃'], agent_keys: ['hutao'] },
          { map_key: 'level_011_ladoga', label: '拉多加湖', count: 1, agents: ['胡桃'], agent_keys: ['hutao'] },
        ],
        channel_map_key: 'level_011_ladoga', channel_map_label: '拉多加湖',
      });
    if (pathname === '/api/ai/capabilities' && request.method() === 'GET') {
      // 首次保存后的回读：模拟 GS 已消费并拒绝该运行命令，页面必须显示原因。
      if (state.phase === 'first') {
        state.phase = 'rejected';
        state.capAvailability = 'failed';
        state.capCommand = { id: 10, status: 'rejected', enabled: true, revision: 4, reason: 'stale_capability', applied_ts: 0 };
      }
      const view = capabilityView(state.capRevision, state.capSet, state.capAvailability,
        state.capCommand.status ? state.capCommand : null);
      return json(route, { catalog: catalogFixture(), tools: [], agents: opts.empty ? [] : [view] });
    }
    if (pathname === '/api/ai/capabilities' && request.method() === 'POST') {
      const body = JSON.parse(request.postData() || '{}');
      state.capPosts.push(body);
      if (state.phase === 'initial') {
        state.capRevision = 4;
        state.capSet = body.capabilities || [];
        state.capAvailability = 'pending';
        state.capCommand = { id: 10, status: 'pending', enabled: state.capSet.includes('virtual_fishing'), revision: 4, reason: '' };
        state.phase = 'first';
        const view = capabilityView(4, state.capSet, 'pending', state.capCommand);
        return json(route, {
          ok: true, agent_key: 'hutao', revision: 4, changed: true, state: view.state, generation: 1,
          capabilities: state.capSet, read_tools: view.read_tools, write_tools: view.write_tools,
          availability: 'pending', runtime: view.runtime, commands: view.commands,
          command_id: 10, request_id: 'cap1-x-r4-1', command_status: 'pending',
        });
      }
      // 重试未应用部分：按已保存集合补派，修订不变。
      state.capAvailability = 'available';
      state.capCommand = { id: 11, status: 'applied', enabled: state.capSet.includes('virtual_fishing'), revision: state.capRevision, reason: '', applied_ts: 1710000003 };
      state.phase = 'retried';
      const view = capabilityView(state.capRevision, state.capSet, 'available', state.capCommand);
      return json(route, {
        ok: true, agent_key: 'hutao', revision: state.capRevision, changed: false, state: view.state, generation: 1,
        capabilities: state.capSet, read_tools: view.read_tools, write_tools: view.write_tools,
        availability: 'available', runtime: view.runtime, commands: view.commands,
        command_id: 11, request_id: 'cap1-x-r4-2', command_status: 'applied',
      });
    }
    if (pathname === '/api/ai/commands/10') {
      // 第一次查询：等待；之后：被拒绝，验证失败原因与重试入口。
      if (state.phase === 'first') {
        state.phase = 'rejected';
        state.capAvailability = 'failed';
        state.capCommand = { id: 10, status: 'rejected', enabled: true, revision: 4, reason: 'stale_capability', applied_ts: 0 };
        return json(route, { id: 10, status: 'pending', kind: 'actor_set_enabled', agent_key: 'hutao' }, 202);
      }
      return json(route, { id: 10, status: 'rejected', kind: 'actor_set_enabled', agent_key: 'hutao', reason: 'stale_capability' });
    }
    if (pathname === '/api/ai/settings' && request.method() === 'POST') {
      state.settingsPosts.push(JSON.parse(request.postData() || '{}'));
      return json(route, { ok: true, updated: 1 });
    }
    if (pathname === '/api/ai/memory/clear' && request.method() === 'POST') {
      state.memoryPosts.push(JSON.parse(request.postData() || '{}'));
      return json(route, { ok: true, status: 'pending', command_id: 12 }, 202);
    }
    if (pathname === '/api/ai/commands/12') {
      return json(route, { id: 12, status: 'applied', kind: 'memory_clear', agent_key: 'hutao' });
    }
    return json(route, { ok: true });
  });
  await page.goto('http://127.0.0.1:19216/#ai');
  await page.locator('#aiSteps .ai-step').first().waitFor();
  return { browser, context, page, errors, state };
}

function hasBannedUI(text) {
  return BANNED_UI.filter((item) => text.includes(item));
}

for (const item of BANNED_UI) {
  assert.ok(!AI_INDEX.includes(item), `静态片段不应包含旧术语 ${item}`);
}

test('T-105 空工作区：总览主动作、空列表与预算校验', async () => {
  const { browser, page, errors, state } = await openPage({ empty: true });
  try {
    assert.equal(await page.locator('#aiSteps .ai-step').count(), 4);
    assert.match(await page.locator('#aiOverviewPrimary').textContent(), /创建角色/);
    assert.equal(await page.locator('#aiHelloText').textContent(), '你好');
    assert.match(await page.locator('#aiHelloGuide').textContent(), /先在「角色与订阅」/);

    await page.locator('#aiOverviewPrimary').click();
    await page.locator('#aiPane-agents.on').waitFor();
    assert.match(await page.locator('#aiAgentList').textContent(), /还没有 Agent/);
    assert.equal(await page.locator('#aiNewUID').count(), 0);
    assert.equal(await page.locator('#aiNewTools').count(), 0);

    await page.locator('[data-ai-tab="talk"]').click();
    await page.locator('#aiPane-talk.on').waitFor();
    assert.equal(await page.locator('#aiPane-talk .ai-budget-card').count(), 4);
    assert.equal(await page.locator('#aiMapList').count(), 1);

    // 超域：字段旁提示，聚焦首错，整组拒绝提交。
    await page.locator('#aiSet_ai_profile_channel_reply_soft_chars').fill('999');
    await page.locator('[data-action="save-settings"][data-group="channel"]').click();
    assert.equal(state.settingsPosts.length, 0, '超域组不得提交');
    assert.match(await page.locator('#aiErr_ai_profile_channel_reply_soft_chars').textContent(), /需在 10–200 之间/);

    // 软目标 > 硬上限：字段旁提示、聚焦首错、整组拒绝提交。
    await page.locator('#aiSet_ai_profile_channel_reply_soft_chars').fill('100');
    await page.locator('#aiSet_ai_profile_channel_reply_chars').fill('45');
    await page.locator('[data-action="save-settings"][data-group="channel"]').click();
    assert.equal(state.settingsPosts.length, 0, '错误组不得提交');
    assert.match(await page.locator('#aiErr_ai_profile_channel_reply_soft_chars').textContent(), /软目标不能大于硬上限/);
    assert.equal(await page.evaluate(() => document.activeElement && document.activeElement.id), 'aiSet_ai_profile_channel_reply_soft_chars');

    // 非整数：同样不提交，聚焦到该字段。
    await page.locator('#aiSet_ai_profile_channel_reply_soft_chars').fill('25');
    await page.locator('#aiSet_ai_profile_channel_reply_chars').fill('45.5');
    await page.locator('[data-action="save-settings"][data-group="channel"]').click();
    assert.equal(state.settingsPosts.length, 0, '非整数组不得提交');
    assert.match(await page.locator('#aiErr_ai_profile_channel_reply_chars').textContent(), /必须是整数/);
    assert.equal(await page.evaluate(() => document.activeElement && document.activeElement.id), 'aiSet_ai_profile_channel_reply_chars');

    // 修正后可保存，页面按回读值显示（夹具返回 updated=1）。
    await page.locator('#aiSet_ai_profile_channel_reply_soft_chars').fill('25');
    await page.locator('#aiSet_ai_profile_channel_reply_chars').fill('45');
    await page.locator('[data-action="save-settings"][data-group="channel"]').click();
    await page.waitForTimeout(800);
    assert.equal(state.settingsPosts.length, 1);
    assert.equal(state.settingsPosts[0].ai_profile_channel_reply_soft_chars, 25);
    assert.deepEqual(errors, []);
  } finally {
    await browser.close();
  }
});

test('T-105 四步证据、订阅与能力保存/拒绝重试、窄屏键盘', async () => {
  const { browser, context, page, errors, state } = await openPage({ empty: false });
  try {
    await context.grantPermissions(['clipboard-read', 'clipboard-write']);
    assert.equal(await page.locator('#aiSteps .ai-step').count(), 4);
    assert.match(await page.locator('#aiOverviewPriority').textContent(), /可试说/);
    assert.match(await page.locator('#aiSelectedVisible').textContent(), /玩家可见/);
    assert.match(await page.locator('#aiSelectedActivity').textContent(), /在钓鱼/);
    assert.match(await page.locator('#aiHelloGuide').textContent(), /胡桃/);
    await page.locator('[data-action="copy-hello"]').first().click();
    assert.equal(await page.evaluate(() => navigator.clipboard.readText()), '你好');

    // 角色卡：UID 只读；编辑无工具；Escape 后焦点回到触发点。
    await page.locator('[data-ai-tab="agents"]').click();
    const card = page.locator('.ai-agent-item').first();
    assert.match(await card.textContent(), /2147480001/);
    const edit = card.locator('[data-action="edit-ai"]').first();
    await edit.click();
    await page.locator('#aiModal').waitFor();
    assert.equal(await page.locator('#aiEditTools').count(), 0);
    assert.ok(hasBannedUI(await page.locator('#aiModal').innerText()).length === 0);
    await page.keyboard.press('Escape');
    const activeAction = await page.evaluate(() => document.activeElement && document.activeElement.dataset ? document.activeElement.dataset.action : '');
    assert.equal(activeAction, 'edit-ai');

    // 订阅来源：普通地图/全局与精确频道分开显示，多命中全部列出。
    assert.match(await page.locator('#aiMapList').textContent(), /全局/);
    assert.match(await page.locator('#aiMapList').textContent(), /拉多加湖/);
    assert.match(await page.locator('#aiChannelList').textContent(), /nav1_03/);
    assert.match(await page.locator('#aiChannelList').textContent(), /nav1_04/);

    // 能力：仅一次保存；首次 pending → 拒绝；再按已保存集合重试 → 已生效。
    await page.locator('[data-ai-tab="personality"]').click();
    await page.locator('#aiCapList input[data-cap="guide_help"]').waitFor();
    assert.equal(await page.locator('#aiCapList input[data-cap="guide_help"]').isChecked(), true);
    assert.equal(await page.locator('#aiCapList input[data-cap="broadcast_catch"]').isChecked(), false);
    await page.locator('[data-action="save-capabilities"]').click();
    await page.waitForFunction(() => document.querySelector('#aiCapResult').textContent.includes('已保存修订 4'));
    assert.equal(state.capPosts.length, 1);
    assert.equal(state.capPosts[0].base_revision, 3);
    assert.deepEqual(state.capPosts[0].capabilities, ['guide_help', 'virtual_fishing']);
    assert.match(await page.locator('#aiCapResult').textContent(), /已保存修订 4/);
    assert.match(await page.locator('#aiCapCommands').textContent(), /命令 #10：被拒绝/);
    await page.locator('[data-action="retry-capabilities"]').click();
    await page.waitForFunction(() => document.querySelector('#aiCapResult').textContent.includes('已生效'));
    assert.equal(state.capPosts.length, 2);
    assert.equal(state.capPosts[1].base_revision, 4);
    assert.deepEqual(state.capPosts[1].capabilities, ['guide_help', 'virtual_fishing']);
    assert.match(await page.locator('#aiCapState').textContent(), /已生效/);

    // 窄屏：单列、无横向滚动、标签可键盘到达。
    await page.setViewportSize({ width: 420, height: 900 });
    await page.locator('[data-ai-tab="talk"]').click();
    const cards = page.locator('#aiPane-talk .ai-budget-card');
    assert.equal(await cards.count(), 4);
    const boxes = await cards.evaluateAll((nodes) => nodes.map((node) => node.getBoundingClientRect()));
    assert.ok(boxes[1].top > boxes[0].top, '窄屏预算卡应纵向排列');
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
    assert.ok(overflow <= 1, `窄屏横向溢出 ${overflow}px`);
    const tabFocusable = await page.locator('#aiTabs .params-tab').evaluateAll((nodes) => nodes.every((node) => node.tabIndex >= 0));
    assert.equal(tabFocusable, true);

    // 钓鱼：历史/今日分开；清除按钮文案；没有发鱼测试入口。
    await page.locator('[data-ai-tab="fishing"]').click();
    assert.match(await page.locator('#aiFishingStats').textContent(), /历史累计 12 条/);
    assert.match(await page.locator('#aiFishingStats').textContent(), /今日（0 点起）2 条/);
    assert.equal(await page.locator('#aiTestFishingAgent').count(), 0);
    assert.ok(hasBannedUI(await page.locator('#view-ai').innerText()).length === 0, hasBannedUI(await page.locator('#view-ai').innerText()).join(','));
    assert.deepEqual(errors, []);
  } finally {
    await browser.close();
  }
});

test('T-105 失败提示邮件不判完成、GS 投递后才算完成', async () => {
  // 有邮件时间（失败提示已被 GS 投进个人消息），但没有 status=done 的成功投递证据：
  // 第 4 步必须仍未完成，并给出可重试说明。
  const failed = richAgent('hutao', '胡桃', true, true, {
    contact_visible: true, visible_evidence: 'mail',
    last_dm_ts: 1710000010, last_reply_status: 'fallback', last_reply_error: '厂商返回 502',
    last_delivered_ts: 0, last_mail_ts: 1710000011,
  });
  const { browser, page, errors, state } = await openPage({ empty: false, agents: [failed] });
  try {
    const helloStep = page.locator('#aiSteps .ai-step').nth(3);
    assert.equal(await helloStep.locator('.pill').textContent(), '当前');
    assert.match(await helloStep.locator('.ai-step-detail').textContent(), /AI 回复失败：厂商返回 502/);
    assert.match(await helloStep.locator('.ai-step-detail').textContent(), /再发一句|复测/);
    assert.match(await page.locator('#aiSelectedVisible').textContent(), /玩家可见/);
    assert.doesNotMatch(await page.locator('#aiSteps').textContent(), /已收到游戏内回复/);

    // GS 成功投递后的形态：正文已清空，只留 done 状态与投递时间。
    const delivered = richAgent('hutao', '胡桃', true, true, {
      contact_visible: true, visible_evidence: 'mail',
      last_dm_ts: 1710000010, last_reply_status: 'done', last_reply_error: '',
      last_delivered_ts: 1710000012, last_mail_ts: 1710000012,
    });
    state.status = statusFixture({ empty: false, agents: [delivered] });
    await page.reload();
    await page.locator('#aiSteps .ai-step').first().waitFor();
    assert.equal(await page.locator('#aiSteps .ai-step').nth(3).locator('.pill').textContent(), '完成');
    assert.match(await page.locator('#aiSteps .ai-step').nth(3).locator('.ai-step-detail').textContent(), /已收到游戏内回复/);

    // 之后再失败一次：首句仍完成，最近失败仍可查并保留重试说明。
    const later = richAgent('hutao', '胡桃', true, true, {
      contact_visible: true, visible_evidence: 'mail',
      last_dm_ts: 1710000020, last_reply_status: 'fallback', last_reply_error: '上游 502',
      last_delivered_ts: 1710000012, last_mail_ts: 1710000021,
    });
    state.status = statusFixture({ empty: false, agents: [later] });
    await page.reload();
    await page.locator('#aiSteps .ai-step').first().waitFor();
    const last = page.locator('#aiSteps .ai-step').nth(3);
    assert.equal(await last.locator('.pill').textContent(), '完成');
    assert.match(await last.locator('.ai-step-detail').textContent(), /最近一次回复失败：上游 502/);
    assert.deepEqual(errors, []);
  } finally {
    await browser.close();
  }
});
