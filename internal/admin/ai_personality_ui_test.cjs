// T-107 浏览器夹具：四维手调、键盘、保存/回读、评分草稿、撤销、两角色隔离与失败路径；
// P4 另覆盖未保存只见草稿、已生效带修订号、迟到响应不覆盖新版本/另一角色。
//
// 运行：node internal/admin/ai_personality_ui_test.cjs
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
const SHELL = fs.readFileSync(path.join(ROOT, 'web/index.html'), 'utf8');
const AI_INDEX = fs.readFileSync(path.join(ROOT, 'web/ai/index.html'), 'utf8');
const AI_JS = fs.readFileSync(path.join(ROOT, 'web/ai/ai.js'), 'utf8');
const AI_CSS = fs.readFileSync(path.join(ROOT, 'web/ai/ai.css'), 'utf8');

function browserPath() {
  if (process.env.RF4_TEST_BROWSER) return process.env.RF4_TEST_BROWSER;
  const base = path.join(os.homedir(), 'AppData/Local/ms-playwright');
  const candidates = [
    path.join(base, 'chromium-1237/chrome-win64/chrome.exe'),
    path.join(base, 'chromium_headless_shell-1237/chrome-headless-shell-win64/chrome-headless-shell.exe'),
  ];
  return candidates.find((item) => fs.existsSync(item)) || undefined;
}

// 与服务端 T-103 定义同形的夹具：标签与端点只用于渲染断言，真源仍在 Go 侧。
const DIMENSIONS = [
  {
    id: 'sociability', label: '交往意愿', low_end: '独处优先', high_end: '乐于回应邀请',
    low_band: '交往意愿偏低：更愿意一个人待着，不主动约人。', mid_band: '交往意愿适中：有人叫就一起。',
    high_band: '交往意愿偏高：倾向和钓友一起行动。',
  },
  {
    id: 'exploration', label: '尝新意愿', low_end: '熟悉目标', high_end: '探索新目标',
    low_band: '尝新意愿偏低：守着熟悉的目标。', mid_band: '尝新意愿适中：有合适机会才换。',
    high_band: '尝新意愿偏高：愿意尝试新钓点。',
  },
  {
    id: 'persistence', label: '持续投入', low_end: '做够就换', high_end: '再多留一会',
    low_band: '持续投入偏低：做到够就收。', mid_band: '持续投入适中：差不多再决定。',
    high_band: '持续投入偏高：愿意多留一会。',
  },
  {
    id: 'regularity', label: '作息规律', low_end: '灵活休整', high_end: '偏好固定作息',
    low_band: '作息灵活：累了就休整。', mid_band: '作息大致有节奏。',
    high_band: '作息偏好固定：倾向固定时段。',
  },
];
const BANDS = [
  { band: 'low', label: '偏低', min: 0, max: 32 },
  { band: 'mid', label: '适中', min: 33, max: 67 },
  { band: 'high', label: '偏高', min: 68, max: 100 },
];
const DIM_IDS = DIMENSIONS.map((dim) => dim.id);

const OK_REPLY = {
  status: 'ok',
  values: { sociability: 60, exploration: 40, persistence: 70, regularity: 55 },
  ranges: {
    sociability: [45, 70], exploration: [30, 55], persistence: [55, 80], regularity: [40, 70],
  },
  reasons: {
    sociability: '描述里爱主动约人', exploration: '愿意尝试新水域',
    persistence: '做事有耐心', regularity: '提到固定作息',
  },
};

function bandOf(value) {
  return BANDS.find((band) => value >= band.min && value <= band.max) || BANDS[1];
}

function bandText(dim, value) {
  const band = bandOf(value).band;
  if (band === 'low') return dim.low_band;
  if (band === 'high') return dim.high_band;
  return dim.mid_band;
}

function initialState(opts) {
  const emergency = !!(opts && opts.emergency);
  const agents = {
    hutao: {
      key: 'hutao', display: '胡桃', description: '喜欢安静钓鱼，愿意尝试新水域，做事有耐心。',
      rev: 3, values: { sociability: 80, exploration: 20, persistence: 60, regularity: 40 },
      savedAt: 1710000000, tasks: 0, generation: 1,
      effective: false, effectiveTask: 0, effectiveResult: 0,
    },
    klee: {
      key: 'klee', display: '可莉', description: '喜欢热闹，说走就走。',
      rev: 1, values: { sociability: 10, exploration: 90, persistence: 30, regularity: 70 },
      savedAt: 1710000000, tasks: 2, generation: 1,
      effective: false, effectiveTask: 0, effectiveResult: 0,
    },
  };
  if (opts && opts.unsaved) agents.hutao.unsaved = true;
  return {
    emergency,
    order: ['hutao', 'klee'],
    agents,
    // P4：未保存角色只有页面草稿；服务端只回中性 50 与 not_saved。
    savePosts: [],
    scorePosts: [],
    saveBehaviors: [],
    scoreBehaviors: [],
    // P4：可延迟的性格回读（请求时点快照）与保存回执，用于迟到响应反例。
    personalityBehaviors: [],
    personalityGets: 0,
  };
}

function agentView(state, agent) {
  if (agent.unsaved) {
    // 未保存：服务端只有中性 50；页面上的改动只是草稿。
    return {
      agent_key: agent.key, display: agent.display, role: '钓友', description: agent.description,
      schema_version: 1, revision: 0, present: false, current_generation: 1,
      configured: true, needs_repair: false, repair_reason: '', inactive: '',
      dimensions: DIMENSIONS,
      bands: DIMENSIONS.map((dim) => ({
        ...dim, value: 50, band: bandOf(50).band, text: bandText(dim, 50),
      })),
      values: {
        schema_version: 1, revision: 0, generation: 1,
        sociability: 50, exploration: 50, persistence: 50, regularity: 50,
      },
      saved_at: 0,
      save_display: '尚未保存',
      derived: { schema_version: 1, revision: 0, dwell_floor_seconds: 2100, rest_interval_seconds: 600 },
      effect: {
        effective: false,
        reason: 'not_saved',
        display: '尚未保存性格：当前按中性 50 参与派生',
        tasks_since_save: -1,
        last_task_at: 0,
      },
    };
  }
  const effect = agent.effective
    ? {
      effective: true,
      reason: 'matched',
      display: '已生效 · 修订 ' + agent.rev + '（同版运行消费已取证：任务 ' + agent.effectiveTask +
        ' / 结果 ' + agent.effectiveResult + '）',
      tasks_since_save: 1,
      last_task_at: 1710000200,
    }
    : {
      effective: false,
      reason: agent.effectReason || 'br_expression_missing',
      display: '已保存修订 ' + agent.rev + '；等待同版运行证据（保存后还没有按这一版完成表达）',
      tasks_since_save: 0,
      last_task_at: 0,
    };
  return {
    agent_key: agent.key, display: agent.display, role: '钓友', description: agent.description,
    schema_version: 1, revision: agent.rev, present: true,
    current_generation: agent.generation || 1,
    configured: true, needs_repair: false, repair_reason: '', inactive: '',
    dimensions: DIMENSIONS,
    bands: DIMENSIONS.map((dim) => ({
      ...dim, value: agent.values[dim.id], band: bandOf(agent.values[dim.id]).band,
      text: bandText(dim, agent.values[dim.id]),
    })),
    values: {
      schema_version: 1, revision: agent.rev, generation: agent.generation || 1,
      sociability: agent.values.sociability, exploration: agent.values.exploration,
      persistence: agent.values.persistence, regularity: agent.values.regularity,
    },
    saved_at: agent.savedAt,
    save_display: '已保存 · 修订 ' + agent.rev,
    derived: { schema_version: 1, revision: agent.rev, dwell_floor_seconds: 2100, rest_interval_seconds: 600 },
    effect: effect,
  };
}

function personalityView(state) {
  return {
    schema_version: 1,
    dimensions: DIMENSIONS,
    bands: BANDS,
    destination: { host: 'api.deepseek.com', model: 'deepseek-chat', display: 'api.deepseek.com / deepseek-chat' },
    emergency: {
      stopped: state.emergency, fault: false, blocked: state.emergency,
      reason: state.emergency ? 'emergency_stop' : '',
      display: state.emergency
        ? '急停已开启（已发出的请求可能仍会返回）'
        : '急停已解除（可发起新对话，旧回复不会补发）',
    },
    agents: state.order.map((key) => agentView(state, state.agents[key])),
  };
}

function agentStatus(agent) {
  return {
    key: agent.key, display: agent.display, role: '钓友',
    contact_uid: agent.key === 'hutao' ? 2147480001 : 2147480002,
    joined: agent.key === 'hutao', contact_visible: agent.key === 'hutao',
    visible_evidence: agent.key === 'hutao' ? 'dm' : '',
    pending_commands: 0, catch_total: 0, catch_today: 0, memory_total: 0,
    last_dm_ts: 0, last_reply_status: '', last_reply_error: '',
    last_delivered_ts: 0, last_mail_ts: 0,
    generation: 1, revision: 1, tools: '[]', tools_list: [],
    prompt: agent.description, thinking_enabled: true, thinking_effort: 'medium',
    reply_mode: 'social', talkativeness: 50, actor_enabled: false, actor: null,
    catch_total_g: 0, catch_today_g: 0,
  };
}

function statusFixture(state) {
  return {
    settings: {}, specs: [], modules: [], security: { keys: {} },
    provider: 'deepseek', base_url: 'https://api.deepseek.com', model_id: 'deepseek-chat',
    agents: state.order.map((key) => agentStatus(state.agents[key])),
    last_error: {}, bridge: { online: true }, queue: { pending: 0, processing: 0, tool_pending: 0, tool_processing: 0 },
    revision: 3, config_state: 'ok', config_source: 'admin_save',
    destination: { host: 'api.deepseek.com', base_url: 'https://api.deepseek.com', model: 'deepseek-chat' },
    credential: { state: 'configured', masked: 'sk-***', reason: '', display: '凭据已就绪' },
    retest: { revision: 3, ok: true, current: true, display: '此连接已通过复测', attempt: 'a1' },
    emergency_stop: {
      stopped: state.emergency, fault: false, epoch: '7',
      display: state.emergency ? '急停已开启：所有模型调用已切断' : '急停已解除',
    },
    saved_display: '此连接已通过复测 · api.deepseek.com / deepseek-chat / 修订 3',
    onboarding: {
      status: { priority: state.emergency ? 'emergency' : 'ready', display: '可试说：四步均有证据', next: '保持游戏在线' },
      connection: { state: 'done', display: '', retest_current: true, retest_ok: true },
      steps: [
        { id: 'connection', title: '保存模型厂商与密钥并复测', budget_s: 90, state: 'done', display: '', action: 'retest' },
        { id: 'create', title: '创建 Agent', budget_s: 45, state: 'done', display: '', action: 'create' },
        { id: 'join', title: '接入游戏并找到联系人', budget_s: 45, state: 'done', display: '', action: 'join' },
        { id: 'hello', title: '在游戏私聊发「你好」并收到回复', budget_s: 60, state: 'done', display: '', action: 'say_hello' },
      ],
      first_sentence: { text: '你好', agent: '胡桃', display: '在游戏里私聊「胡桃」发送「你好」' },
      agents: {}, agents_total: state.order.length, joined_total: 1, visible_total: 1,
      dm_total: 1, delivered_total: 1, pending_commands: 0, buffer_s: 60, target_total_s: 300,
    },
  };
}

function json(route, body, status) {
  return route.fulfill({
    status: status || 200,
    contentType: 'application/json; charset=utf-8',
    body: JSON.stringify(body),
  }).catch(() => {});
}

async function openPage(opts) {
  const browser = await chromium.launch({ headless: true, executablePath: browserPath() });
  const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  await context.addInitScript(() => localStorage.setItem('rf4_admin_token', 'fixture'));
  const page = await context.newPage();
  const errors = [];
  page.on('pageerror', (error) => { errors.push(error.message); console.error('PAGEERROR', error.message); });
  page.on('console', (message) => {
    // 失败路径用例会故意收到 4xx/5xx；浏览器把这类响应记成 console error，
    // 这里只保留真正的前端脚本错误，避免把预期失败当成噪声。
    if (message.type() !== 'error') return;
    if (/Failed to load resource/.test(message.text())) return;
    console.error('CONSOLE', message.text());
  });
  const state = initialState(opts);
  await page.route('**/*', async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const pathname = url.pathname;
    if (pathname === '/') return route.fulfill({ contentType: 'text/html; charset=utf-8', body: SHELL });
    if (pathname === '/ai/ai.js') return route.fulfill({ contentType: 'text/javascript; charset=utf-8', body: AI_JS });
    if (pathname === '/ai/ai.css') return route.fulfill({ contentType: 'text/css; charset=utf-8', body: AI_CSS });
    if (pathname === '/ai/index.html') return route.fulfill({ contentType: 'text/html; charset=utf-8', body: AI_INDEX });
    if (pathname === '/api/me') return json(route, { username: 'admin' });
    if (pathname === '/api/ai/status') return json(route, statusFixture(state));
    if (pathname === '/api/ai/capabilities') return json(route, { catalog: [], tools: [], agents: [] });
    if (pathname === '/api/ai/channels') {
      return json(route, { channel: 'nav1_03', agents: [], subscribed: [], channels: [], maps: [] });
    }
    if (pathname === '/api/ai/personality' && request.method() === 'GET') {
      // 请求时点快照：延迟返回时必须带回「发出时」的版本，才能验证迟到响应不覆盖新版本。
      const snapshot = personalityView(state);
      state.personalityGets += 1;
      const behavior = state.personalityBehaviors.shift();
      if (behavior && behavior.delay) await new Promise((resolve) => setTimeout(resolve, behavior.delay));
      return json(route, snapshot);
    }
    if (pathname === '/api/ai/personality/score') {
      const body = JSON.parse(request.postData() || '{}');
      state.scorePosts.push(body);
      const behavior = state.scoreBehaviors.shift() || { status: 200, reply: OK_REPLY };
      if (behavior.delay) await new Promise((resolve) => setTimeout(resolve, behavior.delay));
      if (behavior.status && behavior.status !== 200) {
        return json(route, { error: behavior.error, state: behavior.state }, behavior.status);
      }
      return json(route, {
        request_id: body.request_id,
        base_revision: body.base_revision,
        schema_version: 1,
        ...(behavior.reply || OK_REPLY),
      });
    }
    if (pathname === '/api/ai/personality' && request.method() === 'POST') {
      const body = JSON.parse(request.postData() || '{}');
      state.savePosts.push(body);
      const behavior = state.saveBehaviors.shift();
      if (behavior && behavior.status && behavior.status !== 200) {
        return json(route, { error: behavior.error, state: behavior.state }, behavior.status);
      }
      const agent = state.agents[body.agent_key];
      if (!agent) return json(route, { error: '角色不存在' }, 404);
      const baseRevision = agent.unsaved ? 0 : agent.rev;
      if (body.base_revision !== baseRevision) {
        return json(route, { error: '性格配置已被其他管理员更新，请重新载入后再保存', state: 'conflict' }, 409);
      }
      agent.values = {
        sociability: body.values.sociability, exploration: body.values.exploration,
        persistence: body.values.persistence, regularity: body.values.regularity,
      };
      agent.rev = baseRevision + 1;
      agent.unsaved = false;
      agent.savedAt = 1710000100;
      // 保存回执按「保存时点」取视图；延迟返回时它可能已被更新的版本超过。
      agent.effective = false;
      const savedView = agentView(state, agent);
      if (behavior && behavior.delay) await new Promise((resolve) => setTimeout(resolve, behavior.delay));
      return json(route, {
        ok: true, agent_key: agent.key, revision: savedView.revision, changed: true,
        view: savedView,
      });
    }
    return json(route, { ok: true });
  });
  await page.goto('http://127.0.0.1:19217/#ai');
  await page.locator('#aiTabs').waitFor();
  return { browser, page, errors, state };
}

function row(page, dim) {
  return page.locator('#aiPersonRows .ai-person-row[data-dim="' + dim + '"]');
}

async function valueOf(page, dim) {
  return row(page, dim).locator('[data-role="number"]').inputValue();
}

async function openPersonality(page) {
  await page.locator('[data-ai-tab="personality"]').click();
  await page.locator('#aiPersonRows .ai-person-row[data-dim="sociability"]').waitFor();
}

function assertFourRows(values) {
  assert.deepEqual(Object.keys(values).sort(), DIM_IDS.slice().sort());
}

// scoreOnce 点一次「按描述建议旋钮」：等请求真的发出，再等页面退出评分中状态。
// 用它代替「等某个值出现」，反例被错误接受时才能在断言处快速变红，而不是等超时。
async function scoreOnce(page, state) {
  const before = state.scorePosts.length;
  await page.locator('[data-action="suggest-personality"]').click();
  const deadline = Date.now() + 5000;
  while (state.scorePosts.length === before) {
    if (Date.now() > deadline) throw new Error('评分请求没有发出');
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
  await page.waitForFunction(() => {
    const node = document.querySelector('[data-action="suggest-personality"]');
    return node && !node.disabled && node.textContent.trim() === '按描述建议旋钮';
  });
}

// assertSuggestionPreserved 断言评分响应被拒收后草稿原样保留：
// 手调值 80/45、上一轮建议的范围与理由、建议前的撤销历史都还在，且没有保存。
async function assertSuggestionPreserved(page, state) {
  assert.equal(await valueOf(page, 'sociability'), '80', 'request_id/schema 校验失败时不得把 80 改成 60');
  assert.equal(await valueOf(page, 'exploration'), '45', '手调的 45 不得被建议值 40 覆盖');
  assert.equal(await valueOf(page, 'persistence'), '70');
  assert.equal(await valueOf(page, 'regularity'), '55');

  // 「原 80 → 80」「原 30 → 45」都读自 draft.before：范围、理由与撤销历史都在。
  assert.match(await row(page, 'sociability').locator('[data-role="suggest"]').textContent(),
    /建议 45–70（原 80 → 80）/);
  assert.match(await row(page, 'sociability').locator('[data-role="reason"]').textContent(), /爱主动约人/);
  assert.match(await row(page, 'exploration').locator('[data-role="suggest"]').textContent(),
    /建议 30–55（原 30 → 45）/);
  assert.match(await row(page, 'exploration').locator('[data-role="reason"]').textContent(), /愿意尝试新水域/);
  assert.equal(await row(page, 'sociability').locator('[data-role="band"]').isVisible(), true);

  // 撤销历史可用：逐项撤销回到建议前的 30（不是手调的 45，也不是已保存的 20）。
  await row(page, 'exploration').locator('[data-action="undo-personality-dim"]').click();
  assert.equal(await valueOf(page, 'exploration'), '30');
  assert.equal(await row(page, 'exploration').locator('[data-role="band"]').isVisible(), false);
  assert.match(await page.locator('#aiPersonNotice').textContent(), /已撤销这一项/);

  // 评分没有写库，也没有自动重评。
  assert.equal(state.savePosts.length, 0, '拒收的评分不得触发保存');
  assert.equal(state.agents.hutao.rev, 3);
  await page.waitForTimeout(200);
  assert.equal(state.scorePosts.length, 2, '拒收后不得自动再评');
}

// 两个独立反例共用同一段准备：先手调 80/30 再取得合法对照 60/40，最后回到手调 80/45，
// 这样「原 80 → 80」「原 30 → 45」的撤销历史与已保存值（80/20）可区分。
async function prepareSuggestion(page, state) {
  await openPersonality(page);
  await row(page, 'sociability').locator('[data-role="number"]').fill('80');
  await row(page, 'exploration').locator('[data-role="number"]').fill('30');

  state.scoreBehaviors.push({ status: 200, reply: OK_REPLY });
  await scoreOnce(page, state);
  assert.equal(await valueOf(page, 'sociability'), '60', '合法对照应把草稿填成 60');
  assert.match(await row(page, 'sociability').locator('[data-role="suggest"]').textContent(),
    /建议 45–70（原 80 → 60）/);
  assert.match(await row(page, 'sociability').locator('[data-role="reason"]').textContent(), /爱主动约人/);
  assert.match(await row(page, 'exploration').locator('[data-role="suggest"]').textContent(),
    /建议 30–55（原 30 → 40）/);

  await row(page, 'sociability').locator('[data-role="number"]').fill('80');
  await row(page, 'exploration').locator('[data-role="number"]').fill('45');
  assert.match(await row(page, 'exploration').locator('[data-role="suggest"]').textContent(),
    /建议 30–55（原 30 → 45）/);
}

test('T-107 手调、键盘、保存与刷新回读同一修订', async () => {
  const { browser, page, errors, state } = await openPage();
  try {
    await openPersonality(page);

    // 四维顺序与取值来自服务端视图；描述来自角色页，只读。
    assert.equal(await page.locator('#aiPersonRows .ai-person-row').count(), 4);
    assertFourRows({
      sociability: await valueOf(page, 'sociability'), exploration: await valueOf(page, 'exploration'),
      persistence: await valueOf(page, 'persistence'), regularity: await valueOf(page, 'regularity'),
    });
    assert.equal(await valueOf(page, 'sociability'), '80');
    assert.equal(await valueOf(page, 'exploration'), '20');
    assert.equal(await valueOf(page, 'persistence'), '60');
    assert.equal(await valueOf(page, 'regularity'), '40');
    assert.match(await row(page, 'sociability').locator('[data-role="value"]').textContent(), /80\/100（偏高）/);
    assert.match(await row(page, 'exploration').locator('[data-role="value"]').textContent(), /20\/100（偏低）/);
    assert.match(await page.locator('#aiPersonDescription').textContent(), /喜欢安静钓鱼/);
    assert.match(await page.locator('#aiPersonMeta').textContent(), /api\.deepseek\.com \/ deepseek-chat/);
    assert.match(await page.locator('#aiPersonMeta').textContent(), /急停已解除/);
    assert.match(await page.locator('#aiPersonMeta').textContent(), /驻留底线 2100 秒/);

    // 已保存是库里的修订；没有同版运行消费证据时不得显示「已生效」。
    assert.match(await page.locator('#aiPersonStatus').textContent(), /已保存 · 修订 3/);
    const effect = await page.locator('#aiPersonEffect').textContent();
    assert.match(effect, /等待同版运行证据/);
    assert.match(effect, /已保存修订 3/);
    assert.doesNotMatch(effect, /已生效/);
    const pageText = await page.locator('#view-ai').innerText();
    assert.doesNotMatch(pageText, /已生效 · 修订/);

    // 键盘：方向键逐点调整，改完立刻显示「待保存」。
    const range = row(page, 'sociability').locator('[data-role="range"]');
    await range.focus();
    await page.keyboard.press('ArrowRight');
    await page.keyboard.press('ArrowRight');
    assert.equal(await valueOf(page, 'sociability'), '82');
    assert.match(await page.locator('#aiPersonStatus').textContent(), /待保存/);

    // 超域：字段旁报错、不提交、聚焦该字段。
    const number = row(page, 'regularity').locator('[data-role="number"]');
    await number.fill('120');
    assert.match(await row(page, 'regularity').locator('[data-role="warn"]').textContent(), /需要 0–100 的整数/);
    await page.locator('[data-action="save-personality"]').click();
    await page.waitForTimeout(150);
    assert.equal(state.savePosts.length, 0, '有非法字段时不得提交');
    assert.equal(await page.evaluate(() => document.activeElement && document.activeElement.dataset
      ? document.activeElement.dataset.role : ''), 'number');

    // 改正后保存：提交 base_revision=3 与草稿四维。
    await number.fill('40');
    await page.locator('[data-action="save-personality"]').click();
    await page.waitForFunction(() => document.querySelector('#aiPersonStatus').textContent.includes('修订 4'));
    assert.equal(state.savePosts.length, 1);
    assert.equal(state.savePosts[0].base_revision, 3);
    assert.deepEqual(state.savePosts[0].values,
      { sociability: 82, exploration: 20, persistence: 60, regularity: 40 });
    assert.match(await page.locator('#aiPersonNotice').textContent(), /已保存修订 4/);
    assert.match(await page.locator('#aiPersonStatus').textContent(), /已保存 · 修订 4/);
    assert.doesNotMatch(await page.locator('#aiPersonEffect').textContent(), /已生效/);

    // 刷新后回读同一修订与同一组值。
    await page.reload();
    await page.locator('#aiTabs').waitFor();
    await openPersonality(page);
    assert.match(await page.locator('#aiPersonStatus').textContent(), /已保存 · 修订 4/);
    assert.equal(await valueOf(page, 'sociability'), '82');
    assert.equal(await valueOf(page, 'regularity'), '40');

    // 窄屏：单列、无横向滚动、控件可键盘到达。
    await page.setViewportSize({ width: 420, height: 900 });
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
    assert.ok(overflow <= 1, `窄屏横向溢出 ${overflow}px`);
    const card = await row(page, 'sociability').evaluate((node) => {
      const range = node.querySelector('[data-role="range"]');
      const number = node.querySelector('[data-role="number"]');
      const a = range.getBoundingClientRect();
      const b = number.getBoundingClientRect();
      return { stacked: b.top >= a.bottom - 1, focusable: range.tabIndex >= 0 };
    });
    assert.equal(card.stacked, true, '窄屏滑块与数字框应纵向排列');
    assert.equal(card.focusable, true);
    assert.deepEqual(errors, []);
  } finally {
    await browser.close();
  }
});

test('T-107 评分只填草稿：范围带、理由、越区间警告与逐项/全部撤销', async () => {
  const { browser, page, errors, state } = await openPage();
  try {
    await openPersonality(page);
    state.scoreBehaviors.push({ status: 200, reply: OK_REPLY });
    await page.locator('[data-action="suggest-personality"]').click();
    await page.waitForFunction(() => document.querySelector('#aiPersonNotice').textContent.includes('已按描述建议'));

    // 请求按票面六字段：描述与当前值来自页面视图。
    assert.equal(state.scorePosts.length, 1);
    const post = state.scorePosts[0];
    assert.match(post.request_id, /^ps-\d+-/);
    assert.equal(post.agent_key, 'hutao');
    assert.equal(post.base_revision, 3);
    assert.equal(post.dimension_schema_version, 1);
    assert.match(post.description, /喜欢安静钓鱼/);
    assert.deepEqual(post.current_values, { sociability: 80, exploration: 20, persistence: 60, regularity: 40 });

    // 建议回填草稿：值、浅色范围带、理由与「原 → 新」。
    assert.equal(await valueOf(page, 'sociability'), '60');
    assert.equal(await valueOf(page, 'persistence'), '70');
    assert.match(await row(page, 'sociability').locator('[data-role="suggest"]').textContent(),
      /建议 45–70（原 80 → 60）/);
    assert.match(await row(page, 'sociability').locator('[data-role="reason"]').textContent(), /爱主动约人/);
    assert.equal(await row(page, 'sociability').locator('[data-role="band"]').isVisible(), true);
    assert.match(await page.locator('#aiPersonStatus').textContent(), /待保存/);

    // 域内越出建议区间：黄色警告，但仍可保存。
    await row(page, 'sociability').locator('[data-role="number"]').fill('90');
    assert.match(await row(page, 'sociability').locator('[data-role="warn"]').textContent(), /超出建议区间 45–70/);

    // 逐项撤销：回到建议前的值 80，并清掉这一项的建议。
    await row(page, 'sociability').locator('[data-action="undo-personality-dim"]').click();
    assert.equal(await valueOf(page, 'sociability'), '80');
    assert.equal(await row(page, 'sociability').locator('[data-role="band"]').isVisible(), false);
    assert.match(await page.locator('#aiPersonNotice').textContent(), /已撤销这一项/);

    // 再评分后全部撤销：回到已保存的四维。
    state.scoreBehaviors.push({ status: 200, reply: OK_REPLY });
    await page.locator('[data-action="suggest-personality"]').click();
    await page.waitForFunction(() => document.querySelector('#aiPersonRows .ai-person-row[data-dim="sociability"] [data-role="number"]').value === '60');
    await page.locator('[data-action="undo-personality-all"]').click();
    assert.equal(await valueOf(page, 'sociability'), '80');
    assert.equal(await valueOf(page, 'exploration'), '20');
    assert.equal(await valueOf(page, 'persistence'), '60');
    assert.equal(await valueOf(page, 'regularity'), '40');
    assert.match(await page.locator('#aiPersonNotice').textContent(), /已撤销全部草稿/);

    // 全程没有保存：评分建议不进库。
    assert.equal(state.savePosts.length, 0);
    assert.equal(state.agents.hutao.rev, 3);
    assert.deepEqual(errors, []);
  } finally {
    await browser.close();
  }
});

test('T-107 两角色草稿隔离、切换三选一、冲突保留与描述过期', async () => {
  const { browser, page, errors, state } = await openPage();
  try {
    await openPersonality(page);

    // 胡桃上改一个值。
    await row(page, 'sociability').locator('[data-role="number"]').fill('82');
    // 切换角色必须三选一，不能静默丢草稿。
    await page.locator('#aiCapAgent').selectOption('klee');
    await page.locator('#aiModal').waitFor();
    const modalText = await page.locator('#aiModal').innerText();
    for (const label of ['保存并切换', '放弃并切换', '取消']) {
      assert.ok(modalText.includes(label), `切换弹窗缺「${label}」`);
    }
    await page.locator('[data-action="personality-switch-cancel"]').click();
    assert.equal(await page.locator('#aiCapAgent').inputValue(), 'hutao');
    assert.equal(await valueOf(page, 'sociability'), '82');

    // 放弃并切换：可莉显示自己的值，胡桃草稿被丢弃。
    await page.locator('#aiCapAgent').selectOption('klee');
    await page.locator('[data-action="personality-switch-discard"]').click();
    await page.waitForFunction(() => document.querySelector('#aiPersonDescription').textContent.includes('热闹'));
    assert.equal(await page.locator('#aiCapAgent').inputValue(), 'klee');
    assert.equal(await valueOf(page, 'sociability'), '10');
    assert.equal(await valueOf(page, 'exploration'), '90');
    assert.match(await page.locator('#aiPersonStatus').textContent(), /已保存 · 修订 1/);

    // 可莉改 55 后切回胡桃：选「保存并切换」，保存的是可莉，胡桃保持自己的值。
    await row(page, 'sociability').locator('[data-role="number"]').fill('55');
    await page.locator('#aiCapAgent').selectOption('hutao');
    await page.locator('[data-action="personality-switch-save"]').click();
    await page.waitForFunction(() => document.querySelector('#aiPersonDescription').textContent.includes('安静'));
    assert.equal(state.savePosts.length, 1);
    assert.equal(state.savePosts[0].agent_key, 'klee');
    assert.equal(state.savePosts[0].base_revision, 1);
    assert.equal(state.agents.klee.rev, 2);
    assert.equal(await valueOf(page, 'sociability'), '80', '胡桃不应被可莉的草稿污染');

    // 冲突：另一位管理员已保存 → 409 保留草稿并提示重新载入。
    await row(page, 'sociability').locator('[data-role="number"]').fill('82');
    state.saveBehaviors.push({
      status: 409, state: 'conflict',
      error: '性格配置已被其他管理员更新，请重新载入后再保存',
    });
    await page.locator('[data-action="save-personality"]').click();
    await page.waitForFunction(() => document.querySelector('#aiPersonNotice').textContent.includes('其他管理员'));
    assert.equal(await valueOf(page, 'sociability'), '82', '冲突后必须保留草稿');
    assert.match(await page.locator('#aiPersonNotice').textContent(), /草稿已保留/);
    assert.equal(await page.locator('[data-action="reload-personality"]').isVisible(), true);

    // 重新载入按服务端最新修订与值重填。
    state.agents.hutao.rev = 5;
    state.agents.hutao.values = { sociability: 45, exploration: 25, persistence: 65, regularity: 45 };
    await page.locator('[data-action="reload-personality"]').click();
    await page.waitForFunction(() => document.querySelector('#aiPersonStatus').textContent.includes('修订 5'));
    assert.equal(await valueOf(page, 'sociability'), '45');
    assert.match(await page.locator('#aiPersonNotice').textContent(), /已按最新配置重新载入/);

    // 描述变更：旧建议过期，手调值保留。刷新状态会重新读取性格视图。
    state.scoreBehaviors.push({ status: 200, reply: OK_REPLY });
    await page.locator('[data-action="suggest-personality"]').click();
    await page.waitForFunction(() => document.querySelector('#aiPersonNotice').textContent.includes('已按描述建议'));
    await row(page, 'sociability').locator('[data-role="number"]').fill('70');
    state.agents.hutao.description = '改成一段全新的描述：只在清晨出门。';
    await page.locator('[data-ai-tab="fishing"]').click();
    await page.locator('#aiPane-fishing [data-action="refresh-status"]').click();
    await page.waitForFunction(() => document.querySelector('#aiPersonDescription').textContent.includes('全新的描述'));
    await page.locator('[data-ai-tab="personality"]').click();
    assert.match(await page.locator('#aiPersonNotice').textContent(), /描述已改变/);
    assert.equal(await valueOf(page, 'sociability'), '70', '描述变了也要保留手调值');
    assert.equal(await row(page, 'sociability').locator('[data-role="band"]').isVisible(), false);
    assert.deepEqual(errors, []);
  } finally {
    await browser.close();
  }
});

test('T-107 失败路径保留草稿：非法、无法评估、超时、取消、迟到与急停', async () => {
  const { browser, page, errors, state } = await openPage();
  try {
    await openPersonality(page);

    // 非法输出：草稿不动。
    state.scoreBehaviors.push({ status: 502, state: 'illegal_output', error: '模型输出不符合约定：草稿未改动，可重试或直接手动调整' });
    await page.locator('[data-action="suggest-personality"]').click();
    await page.waitForFunction(() => document.querySelector('#aiPersonNotice').textContent.includes('评分失败'));
    assert.match(await page.locator('#aiPersonNotice').textContent(), /草稿未改动/);
    assert.equal(await valueOf(page, 'sociability'), '80');

    // 无法评估：只显示服务端固定文案，不猜值。
    state.scoreBehaviors.push({ status: 200, reply: { status: 'unassessable', reason: '请审查提示词：描述不足或互相矛盾' } });
    await page.locator('[data-action="suggest-personality"]').click();
    await page.waitForFunction(() => document.querySelector('#aiPersonNotice').textContent.includes('描述不足或互相矛盾'));
    assert.equal(await valueOf(page, 'sociability'), '80');
    assert.equal(await row(page, 'sociability').locator('[data-role="band"]').isVisible(), false);

    // 超时：保留草稿并提示可重试。
    state.scoreBehaviors.push({ status: 504, state: 'timeout', error: '评分超时（30 秒）：草稿未改动，可重试或手动调整' });
    await page.locator('[data-action="suggest-personality"]').click();
    await page.waitForFunction(() => document.querySelector('#aiPersonNotice').textContent.includes('超时'));
    assert.equal(await valueOf(page, 'sociability'), '80');

    // 迟到：评分在途时手调了草稿 → 回来的建议必须作废。
    state.scoreBehaviors.push({ status: 200, reply: OK_REPLY, delay: 400 });
    await page.locator('[data-action="suggest-personality"]').click();
    await row(page, 'sociability').locator('[data-role="number"]').fill('75');
    await page.waitForFunction(() => document.querySelector('#aiPersonNotice').textContent.includes('草稿已改动'));
    assert.equal(await valueOf(page, 'sociability'), '75', '迟到的建议不得覆盖手调值');
    assert.equal(await row(page, 'sociability').locator('[data-role="band"]').isVisible(), false);

    // 取消：在途请求可取消，取消后草稿不变。
    state.scoreBehaviors.push({ status: 200, reply: OK_REPLY, delay: 500 });
    await page.locator('[data-action="suggest-personality"]').click();
    await page.locator('[data-action="cancel-personality-score"]').click();
    await page.waitForFunction(() => document.querySelector('#aiPersonNotice').textContent.includes('已取消评分'));
    await page.waitForTimeout(700);
    assert.equal(await valueOf(page, 'sociability'), '75');
    assert.equal(await row(page, 'sociability').locator('[data-role="band"]').isVisible(), false);

    // 恢复后仍可正常评分（取消不会把页面卡在 pending）。
    state.scoreBehaviors.push({ status: 200, reply: OK_REPLY });
    await page.locator('[data-action="suggest-personality"]').click();
    await page.waitForFunction(() => document.querySelector('#aiPersonRows .ai-person-row[data-dim="sociability"] [data-role="number"]').value === '60');
    assert.deepEqual(errors, []);
  } finally {
    await browser.close();
  }

  // 急停：评分入口禁用并说明原因，页面不假装能评估。
  const second = await openPage({ emergency: true });
  try {
    await openPersonality(second.page);
    assert.match(await second.page.locator('#aiPersonMeta').textContent(), /急停已开启/);
    assert.equal(await second.page.locator('[data-action="suggest-personality"]').isDisabled(), true);
    assert.match(await second.page.locator('[data-action="suggest-personality"]').getAttribute('title'), /急停/);
    assert.equal(second.state.scorePosts.length, 0);
    assert.deepEqual(second.errors, []);
  } finally {
    await second.browser.close();
  }
});

// P2 反例一：其余字段与合法响应一致，只把服务端回显的 request_id 换成未知值。
test('T-107 反例：request_id 不符的评分响应被拒收，草稿保留手调、范围、理由与撤销历史', async () => {
  const { browser, page, errors, state } = await openPage();
  try {
    await prepareSuggestion(page, state);

    state.scoreBehaviors.push({ status: 200, reply: { ...OK_REPLY, request_id: 'ps-999-unknown' } });
    await scoreOnce(page, state);
    assert.match(await page.locator('#aiPersonNotice').textContent(), /request_id 与本次请求不符/);
    assert.match(await page.locator('#aiPersonNotice').textContent(), /可重新评分或手动调整/);
    await assertSuggestionPreserved(page, state);
    assert.deepEqual(errors, []);
  } finally {
    await browser.close();
  }
});

// P2 反例二：其余字段与合法响应一致，只把服务端回显的 schema_version 换成未知值。
test('T-107 反例：schema_version 未知的评分响应被拒收，草稿保留手调、范围、理由与撤销历史', async () => {
  const { browser, page, errors, state } = await openPage();
  try {
    await prepareSuggestion(page, state);

    state.scoreBehaviors.push({ status: 200, reply: { ...OK_REPLY, schema_version: 99 } });
    await scoreOnce(page, state);
    assert.match(await page.locator('#aiPersonNotice').textContent(), /schema_version 为 99（本次发出 1）/);
    assert.match(await page.locator('#aiPersonNotice').textContent(), /可重新评分或手动调整/);
    await assertSuggestionPreserved(page, state);
    assert.deepEqual(errors, []);
  } finally {
    await browser.close();
  }
});

// P2 反例三（R-150）：其余字段与合法响应一致，只把回显 schema_version 换成布尔值。
// 页面必须按整数类型严格比较，不能让 Number(true) === 1 冒充版本 1。
test('T-107 反例：schema_version 为布尔值（非整数）的评分响应被拒收，草稿保留手调、范围、理由与撤销历史', async () => {
  const { browser, page, errors, state } = await openPage();
  try {
    await prepareSuggestion(page, state);

    state.scoreBehaviors.push({ status: 200, reply: { ...OK_REPLY, schema_version: true } });
    await scoreOnce(page, state);
    // 缺陷会把手调 80 覆盖成建议值 60：先证明手调值没被改动。
    assert.equal(await valueOf(page, 'sociability'), '80', '布尔版本不得让响应回填草稿');
    assert.match(await page.locator('#aiPersonNotice').textContent(), /schema_version 为 true（本次发出 1）/);
    assert.match(await page.locator('#aiPersonNotice').textContent(), /可重新评分或手动调整/);
    await assertSuggestionPreserved(page, state);
    assert.deepEqual(errors, []);
  } finally {
    await browser.close();
  }
});

// P5：评分入口不可用（这里直接摘掉按钮）时，管理员手调、键盘与保存/刷新都必须照常可用。
test('T-107 隐藏评分入口后手调、键盘与保存仍可用', async () => {
  const { browser, page, errors, state } = await openPage();
  try {
    await openPersonality(page);
    await page.evaluate(() => {
      const node = document.querySelector('[data-action="suggest-personality"]');
      if (node) node.remove();
    });
    assert.equal(await page.locator('[data-action="suggest-personality"]').count(), 0);

    const range = row(page, 'sociability').locator('[data-role="range"]');
    await range.focus();
    await page.keyboard.press('ArrowLeft');
    assert.equal(await valueOf(page, 'sociability'), '79');
    await row(page, 'regularity').locator('[data-role="number"]').fill('45');
    assert.match(await page.locator('#aiPersonStatus').textContent(), /待保存/);

    await page.locator('[data-action="save-personality"]').click();
    await page.waitForFunction(() => document.querySelector('#aiPersonStatus').textContent.includes('修订 4'));
    assert.equal(state.savePosts.length, 1);
    assert.equal(state.savePosts[0].base_revision, 3);
    assert.deepEqual(state.savePosts[0].values,
      { sociability: 79, exploration: 20, persistence: 60, regularity: 45 });
    assert.equal(state.scorePosts.length, 0, '没有评分入口时不得发出评分请求');

    await page.reload();
    await page.locator('#aiTabs').waitFor();
    await openPersonality(page);
    assert.match(await page.locator('#aiPersonStatus').textContent(), /已保存 · 修订 4/);
    assert.equal(await valueOf(page, 'sociability'), '79');
    assert.equal(await valueOf(page, 'regularity'), '45');
    assert.deepEqual(errors, []);
  } finally {
    await browser.close();
  }
});

// P4：服务端同版证据点亮「已生效 · 修订 r」；迟到的旧回读/旧生效响应
// 不得覆盖刚保存的新版本，也不得覆盖另一角色。
test('T-107 P4 已生效显示带修订号，迟到响应不覆盖新版本或另一角色', async () => {
  const { browser, page, errors, state } = await openPage();
  try {
    await openPersonality(page);
    const effectText = () => page.locator('#aiPersonEffect').textContent();
    assert.match(await effectText(), /等待同版运行证据/);
    assert.doesNotMatch(await effectText(), /已生效/);

    // 服务端证据到达（角色/代次/修订逐条匹配）：刷新显示被消费的修订号与真实 task/result。
    state.agents.hutao.effective = true;
    state.agents.hutao.effectiveTask = 11;
    state.agents.hutao.effectiveResult = 12;
    await page.locator('[data-action="reload-personality"]').click();
    await page.waitForFunction(() =>
      document.querySelector('#aiPersonEffect').textContent.includes('已生效 · 修订 3'));
    assert.match(await effectText(), /任务 11 \/ 结果 12/);
    assert.match(await effectText(), /已保存 · 修订 3/);

    // 迟到回读：刷新状态时发出一次回读，它在「r=3 已生效」时点取快照并延迟返回；
    // 其间保存 r=4 并刷新到 r=4。
    state.personalityBehaviors.push({ delay: 1200 });
    const getsBefore = state.personalityGets;
    await page.locator('[data-ai-tab="fishing"]').click();
    await page.locator('#aiPane-fishing [data-action="refresh-status"]').click();
    // 等这次回读真的发出（路由在请求到达时取快照），再做保存。
    const deadline = Date.now() + 5000;
    while (state.personalityGets === getsBefore) {
      if (Date.now() > deadline) throw new Error('性格回读没有发出');
      await new Promise((resolve) => setTimeout(resolve, 10));
    }
    await page.locator('[data-ai-tab="personality"]').click();
    await row(page, 'sociability').locator('[data-role="number"]').fill('75');
    await page.locator('[data-action="save-personality"]').click();
    await page.waitForFunction(() => document.querySelector('#aiPersonStatus').textContent.includes('修订 4'));
    // 旧响应此时才回来：不得退回 r=3，也不得再显示旧的「已生效」。
    await page.waitForTimeout(1600);
    assert.match(await page.locator('#aiPersonStatus').textContent(), /已保存 · 修订 4/);
    assert.match(await effectText(), /等待同版运行证据/);
    assert.match(await effectText(), /已保存修订 4/);
    assert.doesNotMatch(await effectText(), /已生效/);
    assert.equal(await valueOf(page, 'sociability'), '75');

    // 另一角色：迟到的快照里可莉还是「r=1 已生效」；其间保存可莉 r=2（回到等待）。
    // 旧响应回来后不得把可莉退回 r=1 或把旧的已生效标回来。
    await page.locator('#aiCapAgent').selectOption('klee');
    await page.waitForFunction(() => document.querySelector('#aiPersonDescription').textContent.includes('热闹'));
    state.agents.klee.effective = true;
    state.agents.klee.effectiveTask = 21;
    state.agents.klee.effectiveResult = 22;
    state.personalityBehaviors.push({ delay: 1200 });
    const getsBefore2 = state.personalityGets;
    await page.locator('[data-ai-tab="fishing"]').click();
    await page.locator('#aiPane-fishing [data-action="refresh-status"]').click();
    const deadline2 = Date.now() + 5000;
    while (state.personalityGets === getsBefore2) {
      if (Date.now() > deadline2) throw new Error('性格回读没有发出（可莉）');
      await new Promise((resolve) => setTimeout(resolve, 10));
    }
    await page.locator('[data-ai-tab="personality"]').click();
    await row(page, 'sociability').locator('[data-role="number"]').fill('55');
    await page.locator('[data-action="save-personality"]').click();
    await page.waitForFunction(() => document.querySelector('#aiPersonStatus').textContent.includes('修订 2'));
    await page.waitForTimeout(1600);
    assert.match(await page.locator('#aiPersonStatus').textContent(), /已保存 · 修订 2/);
    assert.match(await effectText(), /等待同版运行证据/);
    assert.doesNotMatch(await effectText(), /已生效/);
    assert.equal(await valueOf(page, 'sociability'), '55');
    assert.equal(state.agents.klee.rev, 2);

    // 迟到的旧快照也不得把胡桃退回到保存前的生效状态。
    await page.locator('#aiCapAgent').selectOption('hutao');
    await page.waitForFunction(() => document.querySelector('#aiPersonDescription').textContent.includes('安静'));
    assert.match(await page.locator('#aiPersonStatus').textContent(), /已保存 · 修订 4/);
    assert.doesNotMatch(await effectText(), /已生效/);
    assert.equal(await valueOf(page, 'sociability'), '75');
    assert.deepEqual(errors, []);
  } finally {
    await browser.close();
  }
});

// P4：进入页时角色只有页面草稿（服务端 not_saved）；保存前不得显示已生效。
test('T-107 P4 未保存角色只见草稿，保存后才进入等待', async () => {
  const { browser, page, errors, state } = await openPage({ unsaved: true });
  try {
    await openPersonality(page);
    assert.match(await page.locator('#aiPersonStatus').textContent(), /尚未保存/);
    const effect = await page.locator('#aiPersonEffect').textContent();
    assert.match(effect, /尚未保存性格/);
    assert.doesNotMatch(effect, /已生效/);
    assert.equal(await valueOf(page, 'sociability'), '50');

    // 手调只是草稿：状态变待保存，仍未落库、仍不显示已生效。
    await row(page, 'sociability').locator('[data-role="number"]').fill('65');
    assert.match(await page.locator('#aiPersonStatus').textContent(), /待保存/);
    assert.doesNotMatch(await page.locator('#aiPersonEffect').textContent(), /已生效/);
    assert.equal(state.savePosts.length, 0);

    // 保存后是修订 1：进入等待同版运行证据，仍不显示已生效。
    await page.locator('[data-action="save-personality"]').click();
    await page.waitForFunction(() => document.querySelector('#aiPersonStatus').textContent.includes('修订 1'));
    assert.equal(state.savePosts.length, 1);
    assert.equal(state.savePosts[0].base_revision, 0);
    assert.deepEqual(state.savePosts[0].values,
      { sociability: 65, exploration: 50, persistence: 50, regularity: 50 });
    assert.match(await page.locator('#aiPersonEffect').textContent(), /等待同版运行证据/);
    assert.doesNotMatch(await page.locator('#aiPersonEffect').textContent(), /已生效/);
    assert.deepEqual(errors, []);
  } finally {
    await browser.close();
  }
});
