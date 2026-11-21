// AI 助手：让 AI 角色在游戏里陪玩家钓鱼、聊天。
// 结构：开始使用 / 角色 / 对话 / 虚拟钓鱼 / 连接与安全。每个角色的全部设置集中在一个面板里。
import {
  h, api, post, icon, toast, errText, dialog, confirmDialog, drawer, pageHead, section, callout, badge, empty, btn,
  Form, savebar, setLeaveGuard, segmented, tabs, toggleEl, relTime, fmt, navigate, parseHash,
} from '../core.js';
import { localKnowledgeSection } from '../ai-knowledge.js';

// ---------- 文案：设置项的标签、说明、单位全部在这里，后端只提供范围与默认值 ----------
const COPY = {
  // 对话
  ai_profile_channel_reply_soft_chars: ['期望长度', '字'], ai_profile_channel_reply_chars: ['最长', '字'],
  ai_profile_channel_history_messages: ['回看消息', '条'], ai_profile_channel_history_chars: ['回看字数', '字'], ai_profile_channel_context_tokens: ['上下文上限', 'tokens'],
  ai_profile_dm_reply_soft_chars: ['期望长度', '字'], ai_profile_dm_reply_chars: ['最长', '字'],
  ai_profile_dm_history_messages: ['回看消息', '条'], ai_profile_dm_history_chars: ['回看字数', '字'], ai_profile_dm_context_tokens: ['上下文上限', 'tokens'],
  ai_profile_roundtable_reply_soft_chars: ['期望长度', '字'], ai_profile_roundtable_reply_chars: ['最长', '字'],
  ai_profile_roundtable_context_messages: ['回看消息', '条'], ai_profile_roundtable_context_tokens: ['上下文上限', 'tokens'],
  ai_profile_internal_chars: ['游戏资料篇幅', '字', '回答钓鱼问题时，最多带上多少字的鱼情、攻略资料。越多越准，但每次调用更慢更贵。'],
  // 虚拟钓鱼
  ai_fishing_enabled: ['让 AI 角色也钓鱼', '', 'AI 角色会在后台模拟钓鱼并积累渔获。关闭后不再上鱼，已有渔获保留。'],
  ai_fishing_talk_enabled: ['钓到鱼时说一句', '', 'AI 角色钓到鱼时，按自己的性格在聊天里说一句。'],
  ai_fishing_hourly_limit: ['每小时上鱼配额', '次', '只限制可播报档的上鱼尝试，不是说话上限；说话总量看「整体氛围」。'],
  ai_memory_inject: ['记得自己的好成绩', '', '聊天时 AI 知道自己钓到过哪些星级、稀有鱼。关闭不会删除记录。'],
  ai_catch_brief_inject: ['知道自己钓过鱼', '', 'AI 知道自己有渔获，但不会主动报具体数字，玩家问起才说。'],
  ai_memory_recent: ['提起最近几条成绩', '条', ''], ai_memory_best: ['提起最好的几条成绩', '条', ''],
  ai_memory_max_rows: ['最多保留多少条成绩', '条', '超出的旧记录会被清理。'],
  ai_fishing_base_interval_ms: ['两次上鱼的基本间隔', '分钟', '上鱼节拍，与说话预算无关。', 60000],
  ai_fishing_max_interval_ms: ['上鱼间隔最长拉到', '分钟', '', 60000],
  ai_fishing_star_weight: ['钓到星级鱼', '', ''], ai_fishing_super_weight: ['钓到蓝色稀有鱼', '', ''],
  ai_fishing_miss_weight: ['钓到没达标的小鱼', '', ''], ai_fishing_near_weight: ['钓到差一点星级的鱼', '', ''],
  ai_fishing_miss_ratio: ['「没达标」的界线', '', '低于星级标准的这个比例算没达标。'],
  ai_fishing_near_ratio: ['「差一点」的界线', '', '达到星级标准的这个比例算差一点。'],
  ai_fishing_talk_super_chance: ['钓到蓝色稀有鱼', '', ''], ai_fishing_talk_star_chance: ['钓到星级鱼', '', ''],
  ai_fishing_talk_near_chance: ['钓到差一点星级的鱼', '', ''], ai_fishing_talk_miss_chance: ['钓到没达标的小鱼', '', ''],
  ai_fishing_roundtable_chance: ['钓到稀有鱼后引发多人聊天', '', '其他 AI 角色接着聊的概率。'],
  ai_talk_hourly_limit: ['每小时最多说话', '句', '所有角色、所有说话通道合计。'],
  ai_talk_min_gap_s: ['两条消息至少间隔', '秒', '闲时搭话遵守；上鱼播报可插队。'],
  ai_talk_burst: ['允许超前说几句', '句', '小时开头或跨时段允许连续说几句。'],
  ai_talk_roundtable_speakers: ['最多几人接话', '人', '2–12；仅自定义档生效，条数按人数 +2。'],
  ai_talk_weight_0: ['0–15 分钟权重', '', '0–3；四段归一化后决定说话落在哪个时段。'],
  ai_talk_weight_1: ['15–30 分钟权重', '', '0–3。'],
  ai_talk_weight_2: ['30–45 分钟权重', '', '0–3。'],
  ai_talk_weight_3: ['45–60 分钟权重', '', '0–3。'],
  ai_idle_silence_min: ['安静多久后搭话', '分钟', '公共频道里没人说话超过这个时间，AI 才可能自己找个话头。'],
  ai_idle_hourly_limit: ['每小时最多搭话', '句', '闲时搭话的单独上限，仍受「整体氛围」总预算约束。'],
  // 连接
  web_search_enabled: ['允许联网搜索', '', '角色需另获「联网搜索」能力。本地未命中时才使用选定的搜索方式。'],
  search_endpoint: ['搜索服务地址', '', '自定义服务须接收 GET 请求的 q 参数。'],
  max_tool_rounds: ['每次回答最多查几轮资料', '轮', '数字越大越可能答准，但回复更慢。'],
};
const SCENES = [
  ['channel', '频道聊天', '地图公共频道里接话'],
  ['dm', '私聊', '玩家单独找 AI 聊天'],
  ['roundtable', '多人对话', '几个 AI 角色互相接话'],
];
const SCENE_COLS = [
  ['reply_soft_chars', '期望长度', '字'], ['reply_chars', '最长', '字'], ['history_messages', '回看消息', '条'], ['history_chars', '回看字数', '字'], ['context_tokens', '上下文上限', 'tokens'],
];
const EFFORT = [['off', '不思考（最快）'], ['low', '浅'], ['medium', '中'], ['high', '深'], ['max', '最深（最慢）']];

const S = { status: null, specs: {}, agents: [], caps: null, capByKey: {}, channels: null, personality: null, section: 'start' };

async function loadStatus() {
  const d = await api('/api/ai/status');
  S.status = d;
  S.specs = {};
  for (const sp of d.specs || []) S.specs[sp.key] = sp;
  S.agents = d.agents || [];
  return d;
}
async function loadExtras() {
  const [caps, chans, pers] = await Promise.all([
    api('/api/ai/capabilities').catch(() => null),
    api('/api/ai/channels').catch(() => null),
    api('/api/ai/personality').catch(() => null),
  ]);
  S.caps = caps;
  S.capByKey = {};
  for (const a of (caps && caps.agents) || []) S.capByKey[a.key] = a;
  S.channels = chans;
  S.personality = pers;
}
function setting(key) { return (S.status.settings || {})[key]; }

// 用后端 spec 的范围 + 前端文案生成表单字段
function specFor(key, extra = {}) {
  const sp = S.specs[key] || { key, kind: 'string' };
  const [label, unit, help, scale] = COPY[key] || [sp.label || key, '', sp.hint || ''];
  // 后端 JSON 省略了值为 0 的 min，有 max 时按 0 处理
  const min = sp.min ?? (sp.max !== undefined ? 0 : undefined);
  const out = { key, label, help, unit: unit || undefined, kind: sp.kind === 'bool' ? 'bool' : sp.kind === 'int' ? 'int' : sp.kind === 'float' ? 'float' : 'string', def: sp.def, min, max: sp.max, ...extra };
  if (scale) {
    out.scale = scale; out.kind = 'float';
    out.min = (min ?? 0) / scale; out.max = sp.max / scale; out.def = Number(sp.def) / scale;
  }
  if (out.min === undefined) delete out.min;
  if (out.max === undefined) delete out.max;
  return out;
}
function settingValue(spec) {
  const v = setting(spec.key);
  if (spec.scale) return v === undefined || v === '' ? '' : String(Number(v) / spec.scale);
  return v;
}
function toPayload(form, values) {
  const body = {};
  for (const [k, v] of Object.entries(values)) {
    const f = form.fields.get(k);
    const sp = f.spec;
    if (sp.kind === 'bool') body[k] = v === '1';
    else if (sp.kind === 'int' || sp.kind === 'float') body[k] = sp.scale ? Math.round(Number(v) * sp.scale) : Number(v);
    else body[k] = v;
  }
  return body;
}

// ---------- 页面外壳 ----------
const SECTIONS = [
  ['start', '开始使用'], ['agents', '角色'], ['talk', '对话'], ['fishing', '虚拟钓鱼'], ['conn', '连接与安全'],
];

export default async function aiPage(params) {
  await loadStatus();
  await loadExtras();
  if (params[0] && SECTIONS.some(([k]) => k === params[0])) S.section = params[0];
  const root = h('div', { class: 'page' });
  const content = h('div');
  const sub = h('nav', { class: 'subnav', 'aria-label': 'AI 助手' });
  const renderNav = () => sub.replaceChildren(...SECTIONS.map(([k, label]) => h('button', {
    type: 'button', class: S.section === k ? 'on' : '',
    onclick: () => { if (S.section === k) return; S.section = k; history.replaceState(null, '', '#ai/' + k); renderNav(); show(); },
  }, label, k === 'start' && setupDone() < 4 ? badge(setupDone() + '/4', 'warn') : null,
  k === 'agents' ? badge(String(S.agents.length)) : null)));
  const show = () => {
    const builders = { start: startView, agents: agentsView, talk: talkView, fishing: fishingView, conn: connView };
    content.replaceChildren(builders[S.section]());
  };
  S.refresh = async (keepSection = true) => {
    await loadStatus();
    await loadExtras();
    head.replaceWith(head = header());
    renderNav();
    if (keepSection) show();
  };
  let head = header();
  renderNav();
  show();
  root.append(head, h('div', { class: 'subnav-layout' }, sub, content));
  if (params[0] === 'conn' && params[1] === 'search') {
    requestAnimationFrame(() => root.querySelector('#ai-web-search')?.scrollIntoView({ block: 'start' }));
  }
  return root;
}

function setupDone() { return ((S.status.onboarding || S.status).steps || []).filter((s) => s.state === 'done').length; }
function emergency() { return S.status.emergency_stop || {}; }

function header() {
  const em = emergency();
  const bridge = S.status.bridge || {};
  let state;
  // 急停只认后端回读的 emergency_stop；读不准时（fault）明说待确认，不自己猜
  if (em.fault) state = badge('暂停状态待确认，请刷新', 'warn', true);
  else if (em.stopped) state = badge('已暂停', 'bad', true);
  else if (!bridge.online) state = badge('AI 服务未运行', 'warn', true);
  else if ((S.status.credential || {}).state !== 'configured') state = badge('还没连接模型', 'warn', true);
  else state = badge('运行中', 'ok', true);
  const pauseBtn = em.stopped
    ? btn('恢复 AI', () => setEmergency(false), 'primary', { icon: 'power' })
    : btn('暂停所有 AI', () => setEmergency(true), 'danger', { icon: 'power' });
  return h('div', { class: 'page-head' },
    h('div', {}, h('div', { class: 'row' }, h('h1', { text: 'AI 助手' }), state),
      h('p', { text: '让 AI 角色在游戏里陪玩家钓鱼、聊天。AI 回复由你配置的模型服务生成。' })),
    h('div', { class: 'actions' }, btn('', () => S.refresh(), 'ghost icon', { icon: 'refresh', title: '刷新', 'aria-label': '刷新' }), pauseBtn));
}

async function setEmergency(stop) {
  const ok = await confirmDialog(stop
    ? { title: '暂停所有 AI？', desc: '所有 AI 角色会立即停止说话，不再调用模型服务。随时可以恢复。', confirm: '暂停', danger: true }
    : { title: '恢复 AI？', desc: '恢复后 AI 角色会重新开始回复。建议恢复后到「连接与安全」测试一次连接。', confirm: '恢复' });
  if (!ok) return;
  try {
    const body = { emergency_stop: stop };
    if (emergency().epoch) body.emergency_stop_epoch = emergency().epoch;
    await post('/api/ai/settings', body);
    toast(stop ? '已暂停所有 AI' : '已恢复 AI');
    await S.refresh();
  } catch (e) { toast(errText(e), 'bad'); }
}

// ---------- 开始使用 ----------
function startView() {
  const ob = S.status.onboarding || S.status;
  const steps = ob.steps || [];
  const byId = Object.fromEntries(steps.map((s) => [s.id, s]));
  const bridge = S.status.bridge || {};
  const first = (ob.first_sentence || {}).agent || (S.agents[0] && S.agents[0].display) || '';
  const defs = [
    ['connection', '连接模型服务', '填写 AI 服务商的地址、模型名称和密钥，然后测试一次。', '去设置', () => go('conn')],
    ['create', '创建一个 AI 角色', '给它起个名字，写一段人设，比如「热心的老钓友」。', '新建角色', () => { go('agents'); setTimeout(createAgentDialog, 50); }],
    ['join', '让角色进入游戏', '进入游戏后，玩家就能在联系人里找到它。', '去角色列表', () => go('agents')],
    ['hello', '在游戏里打个招呼', first ? `打开游戏联系人，找到「${first}」，私聊发送「你好」。收到回复就成功了。` : '角色进入游戏后，在游戏联系人里找到它，私聊发送「你好」。', '复制「你好」', () => { navigator.clipboard && navigator.clipboard.writeText('你好'); toast('已复制'); }],
  ];
  const firstTodo = defs.findIndex(([id]) => (byId[id] || {}).state !== 'done');
  const list = h('div', { class: 'steps' }, defs.map(([id, t, d, label, action], i) => {
    const done = (byId[id] || {}).state === 'done';
    const cur = i === firstTodo;
    return h('div', { class: 'step' + (done ? ' done' : cur ? ' current' : '') },
      h('div', { class: 'step-mark' }, done ? icon('check') : String(i + 1)),
      h('div', {}, h('div', { class: 't', text: t }), h('div', { class: 'd', text: d })),
      done ? badge('已完成', 'ok') : btn(label, action, cur ? 'primary sm' : 'sm'));
  }));
  const alerts = [];
  if (emergency().stopped) alerts.push(callout('bad', 'AI 已暂停：所有角色都不会回复。点右上角「恢复 AI」可以重新开启。'));
  if (!bridge.online) alerts.push(callout('warn', ['AI 服务程序没有运行，角色不会回复。请回到启动器，在「诊断」页启动「AI 助手」组件。']));
  const last = S.status.last_error || {};
  if (last.error) alerts.push(callout('warn', ['最近一次回复失败', last.agent ? `（${last.agent}）` : '', '：', last.error]));
  return h('div', {},
    alerts.length ? h('div', { class: 'stack', style: { marginBottom: '16px' } }, alerts) : null,
    section({ title: '四步让 AI 角色开口', desc: firstTodo < 0 ? '全部完成。AI 角色已经可以在游戏里聊天了。' : '按顺序完成下面几步，大约 5 分钟。', body: list }),
    section({
      title: '现在的情况',
      body: h('dl', { class: 'kv' },
        h('dt', { text: '模型服务' }), h('dd', { text: S.status.destination && S.status.destination.host ? [S.status.destination.host, S.status.model_id].filter(Boolean).join(' · ') + (S.status.model_id ? '' : '（还没填模型名称）') : '还没设置' }),
        h('dt', { text: '连接测试' }), h('dd', {}, retestBadge()),
        h('dt', { text: '角色' }), h('dd', { text: `${S.agents.length} 个，${S.agents.filter((a) => a.joined).length} 个在游戏中` }),
        h('dt', { text: 'AI 服务程序' }), h('dd', {}, bridge.online ? badge('运行中', 'ok', true) : badge('没有运行', 'warn', true))),
    }));
}
function go(sec) { S.section = sec; history.replaceState(null, '', '#ai/' + sec); S.refresh(); }
function retestBadge() {
  const r = S.status.retest || {};
  if ((S.status.credential || {}).state !== 'configured') return badge('还没设置', '');
  if (!r.current) return badge('还没测试', 'warn');
  return r.ok ? badge('通过', 'ok', true) : badge('失败', 'bad', true);
}

// ---------- 角色 ----------
function agentsView() {
  const list = S.agents.length
    ? h('div', { class: 'grid-2' }, S.agents.map(agentCard))
    : h('div', { class: 'card' }, empty('还没有 AI 角色', '创建第一个角色，给它起个名字和人设。', btn('新建角色', createAgentDialog, 'primary', { icon: 'plus' })));
  return h('div', {},
    h('div', { class: 'row', style: { marginBottom: '14px' } }, h('div', { class: 'grow muted small', text: '点一个角色查看和修改它的全部设置：资料、活动范围、能力、性格、钓鱼战绩。' }),
      S.agents.length ? btn('新建角色', createAgentDialog, 'primary', { icon: 'plus' }) : null),
    list);
}
function agentStatus(a) {
  if (!a.joined) return badge('不在游戏中', '', true);
  if (!a.contact_visible) return badge('进入中，等待玩家看到', 'warn', true);
  return badge('在游戏中', 'ok', true);
}
const ACTOR_LABELS = { fishing: '钓鱼中', resting: '休息中', moving: '换点中', idle: '闲逛中' };

// 角色卡上的「现在在哪、在干什么」：只在有运行态时显示，不猜。
// 状态很久没更新（游戏服没跑/没人在线）时明确写出来，避免把陈旧状态当成正在发生。
function actorLine(a) {
  const act = a.actor;
  if (!act || !act.activity) return '';
  const nowSec = Date.now() / 1000;
  const bits = [ACTOR_LABELS[act.activity] || act.activity];
  if (act.loc_name) bits.push(act.loc_name);
  if (act.target_name) bits.push('目标 ' + act.target_name);
  if (act.since_ts) {
    const mins = Math.max(0, Math.round((nowSec - act.since_ts) / 60));
    if (mins >= 1) bits.push('已 ' + mins + ' 分钟');
  }
  if (act.until_ts && act.until_ts > nowSec) {
    const left = Math.max(1, Math.round((act.until_ts - nowSec) / 60));
    bits.push('预计 ' + left + ' 分钟后换状态');
  }
  if (act.updated_ts) {
    const age = Math.round((nowSec - act.updated_ts) / 60);
    if (age >= 3) bits.push('状态 ' + age + ' 分钟前更新');
  }
  return bits.join(' · ');
}

function agentCard(a) {
  const now = actorLine(a);
  return h('button', {
    type: 'button', class: 'card', onclick: () => agentDrawer(a.key),
    style: { textAlign: 'left', padding: '16px 18px', cursor: 'pointer', display: 'flex', gap: '14px', alignItems: 'flex-start' },
  },
  h('div', { class: 'avatar', style: { width: '40px', height: '40px', fontSize: '16px', background: 'var(--accent-soft)', color: 'var(--accent)' }, text: (a.display || a.key || '?').slice(0, 1) }),
  h('div', { class: 'grow' },
    h('div', { class: 'row', style: { justifyContent: 'space-between' } }, h('b', { style: { fontSize: '15px' }, text: a.display || a.key }), agentStatus(a)),
    h('div', { class: 'muted small', style: { marginTop: '2px' }, text: a.role || '没有填写身份' }),
    now ? h('div', { class: 'small', style: { marginTop: '6px', color: 'var(--accent)' }, text: now }) : null,
    h('div', { class: 'row small muted', style: { marginTop: '10px', gap: '14px' } },
      h('span', { text: '渔获 ' + fmt(a.catch_total || 0) + ' 条' }),
      a.last_dm_ts ? h('span', { text: '最近私聊 ' + relTime(a.last_dm_ts) }) : h('span', { text: '还没有人私聊过' }))));
}

function createAgentDialog() {
  const form = new Form();
  const body = h('div', { class: 'form-grid' },
    form.stacked({ key: 'display', label: '名字', kind: 'string', placeholder: '例如 老张', help: '玩家在游戏里看到的名字。', required: true }, ''),
    form.stacked({ key: 'key', label: '内部标识', kind: 'string', placeholder: '例如 laozhang', help: '只能用小写字母、数字、- 和 _，创建后不能改。', required: true, pattern: /^[a-z0-9_-]+$/, patternMsg: '只能用小写字母、数字、- 和 _' }, ''),
    form.stacked({ key: 'role', label: '身份', kind: 'string', placeholder: '例如 热心的老钓友', span: true }, ''),
    form.stacked({ key: 'prompt', label: '人设', kind: 'text', rows: 5, span: true, placeholder: '描述它的性格、说话方式、擅长什么。例如：你是一位钓了三十年鱼的老钓友，说话直爽，喜欢分享钓点。不知道的事就说不知道，不要编造。' }, ''),
    form.stacked({ key: 'thinking_effort', label: '思考深度', kind: 'select', options: EFFORT, help: '思考越深，回答越周到，但回复更慢、更费钱。' }, 'off'));
  dialog({
    title: '新建 AI 角色', desc: '创建后角色还不会出现在游戏里，需要再点「进入游戏」。', body, size: 'lg',
    actions: [{ label: '取消', kind: 'ghost' }, {
      label: '创建', kind: 'primary', onClick: async () => {
        const keys = ['display', 'key', 'role', 'prompt', 'thinking_effort'];
        if (!form.validate(keys)) return true;
        const v = form.values(keys);
        const d = await post('/api/ai/agent/create', { ...v, thinking_enabled: v.thinking_effort !== 'off' });
        toast(d.revived ? `已恢复「${v.display}」，原来的聊天记录还在。` : `已创建「${v.display}」`);
        await S.refresh();
        agentDrawer(v.key);
      },
    }],
  });
}

// 单个角色的全部设置
function agentDrawer(key, tab = 'profile') {
  const a = S.agents.find((x) => x.key === key);
  if (!a) return;
  const body = h('div');
  const joinBtn = a.joined
    ? btn('移出游戏', async () => {
      if (!(await confirmDialog({ title: `把「${a.display}」移出游戏？`, desc: '它会停止在所有频道接话，也不再出现在玩家联系人里。聊天记录和渔获会保留，之后可以重新进入。', confirm: '移出', danger: true }))) return;
      await post('/api/ai/agent/join', { key, joined: false }); toast('已移出游戏'); await reopen();
    }, 'sm')
    : btn('进入游戏', async () => { await post('/api/ai/agent/join', { key, joined: true }); toast('已进入游戏，玩家重新登录后就能看到'); await reopen(); }, 'primary sm');
  const d = drawer({
    title: a.display || a.key,
    sub: (a.role || '') + (a.contact_uid ? `  ·  游戏内 UID ${a.contact_uid}` : ''),
    body,
    foot: [btn('删除角色', async () => {
      if (!(await confirmDialog({ title: `删除「${a.display}」？`, desc: '聊天记录会保留。之后用同一个内部标识重新创建，可以恢复这个角色。', confirm: '删除', danger: true }))) return;
      const r = await post('/api/ai/agent/delete', { key });
      if (!await waitCommand(r.command_id, '已删除')) return;
      d.close(); await S.refresh();
    }, 'danger sm', { icon: 'trash' }), h('span', { class: 'spacer' }), agentStatus(a), joinBtn],
    width: 'min(820px,100%)',
  });
  async function reopen() { d.close(); await S.refresh(); agentDrawer(key, current); }
  let current = tab;
  const pane = h('div');
  const builders = { profile: agentProfile, scope: (agent) => agentScope(agent),
    abilities: (agent) => agentAbilities(agent, reopen, () => { d.close(); navigate('ai', 'conn', 'search'); }),
    personality: agentPersonality, record: agentRecord };
  const showTab = (t) => { current = t; pane.replaceChildren(builders[t](a, reopen)); };
  body.append(tabs([['profile', '资料'], ['scope', '活动范围'], ['abilities', '能力'], ['personality', '性格'], ['record', '钓鱼战绩']], tab, showTab), pane);
  showTab(tab);
}

function agentProfile(a, reopen) {
  const form = new Form();
  const status = h('div', { class: 'row' });
  const save = btn('保存资料', async () => {
    const keys = ['display', 'role', 'prompt', 'thinking_effort'];
    if (!form.validate(keys)) return;
    save.disabled = true;
    try {
      const v = form.values(keys);
      await post('/api/ai/agent/update', { key: a.key, display: v.display, role: v.role, prompt: v.prompt });
      await post('/api/ai/agent', { key: a.key, thinking_enabled: v.thinking_effort !== 'off', thinking_effort: v.thinking_effort });
      toast('已保存');
      await reopen();
    } catch (e) { toast(errText(e), 'bad'); } finally { save.disabled = false; }
  }, 'primary');
  return h('div', { class: 'stack' },
    h('div', { class: 'form-grid' },
      form.stacked({ key: 'display', label: '名字', kind: 'string', required: true }, a.display || ''),
      h('div', { class: 'field' }, h('span', { class: 'label', text: '内部标识' }), h('div', { class: 'input', style: { display: 'flex', alignItems: 'center', color: 'var(--muted)' } }, h('code', { text: a.key }))),
      form.stacked({ key: 'role', label: '身份', kind: 'string', span: true }, a.role || ''),
      form.stacked({ key: 'prompt', label: '人设', kind: 'text', rows: 8, span: true, help: '决定它怎么说话、知道什么。写清楚「不知道就说不知道」可以减少胡编。' }, a.prompt || ''),
      form.stacked({ key: 'thinking_effort', label: '思考深度', kind: 'select', options: EFFORT, help: '思考越深，回答越周到，但回复更慢、更费钱。' }, a.thinking_enabled ? (a.thinking_effort || 'low') : 'off')),
    h('div', { class: 'row', style: { justifyContent: 'flex-end' } }, status, save));
}

function agentScope(a, advancedOpen = false) {
  const root = h('div', { class: 'stack' });
  const data = S.channels;
  async function reload(open) {
    try {
      S.channels = await api('/api/ai/channels');
    } catch (e) {
      S.channels = null;
      toast('读取活动范围失败：' + errText(e), 'bad');
    }
    if (root.isConnected) root.replaceWith(agentScope(a, open));
  }
  if (!data) {
    root.append(empty('读不到活动范围', '请重试读取。', btn('重试', async (e) => {
      e.currentTarget.disabled = true;
      await reload(advancedOpen);
    })));
    return root;
  }
  const maps = data.maps || [];
  const channels = data.channels || [];
  const isMember = (scope) => (scope.agent_keys || []).includes(a.key);
  const mine = maps.filter(isMember);
  const myChannels = channels.filter(isMember);
  const mapName = (m) => (m.map_key === '*' ? '所有地图（任何频道）' : (m.label || m.map_key));
  async function changeScope(scope, enabled) {
    const buttons = root.querySelectorAll('button:not([disabled])');
    buttons.forEach((b) => { b.disabled = true; });
    try {
      const r = await post('/api/ai/channel', { ...scope, agent: a.key, enabled });
      const message = enabled ? '已添加' : '已移除';
      if (scope.channel) {
        if (!await waitCommand(r.command_id, message)) return;
      } else {
        toast(message);
      }
      await reload(advanced.open);
    } catch (e) {
      toast(errText(e), 'bad');
    } finally {
      buttons.forEach((b) => { b.disabled = false; });
    }
  }
  const select = h('select', { class: 'select' }, maps.filter((m) => !mine.includes(m)).map((m) => h('option', { value: m.map_key, text: mapName(m) })));
  const chSelect = channels.length
    ? h('select', { class: 'select', 'aria-label': '频道' }, channels.filter((c) => !isMember(c)).map((c) => h('option', { value: c.name, text: c.label ? `${c.label}（${c.name}）` : c.name })))
    : h('input', { class: 'input', type: 'text', placeholder: '输入已知的频道标识', 'aria-label': '频道标识' });
  const addChannel = btn('添加频道', () => {
    const channel = chSelect.value.trim();
    if (channel) return changeScope({ channel }, true);
    toast('请输入频道标识', 'bad');
    chSelect.focus();
  }, '', { disabled: channels.length > 0 && !chSelect.options.length });
  const advanced = h('details', { class: 'advanced', open: advancedOpen }, h('summary', {}, '按频道设置 ', h('small', { text: '（高级）' })),
    h('div', { class: 'stack', style: { paddingTop: '12px' } },
      h('p', { class: 'muted small', text: '频道订阅只覆盖选中的区域，可与地图订阅同时使用。玩家也可以在游戏里说「@邀请 角色名」「@移出 角色名」。' }),
      myChannels.length ? h('div', { class: 'card' }, h('div', { class: 'list' }, myChannels.map((c) => h('div', { class: 'list-item' }, h('div', { class: 'grow' }, c.label || c.name, ' ', h('code', { class: 'muted', text: c.name })), btn('移除', () => changeScope({ channel: c.name }, false), 'ghost sm'))))) : null,
      !channels.length ? h('p', { class: 'muted small', text: '服务器还没有记录频道。进入游戏切换频道后，可刷新页面查看。' }) : null,
      h('div', { class: 'row' }, h('div', { class: 'grow' }, chSelect), addChannel)));
  if (!a.joined) root.append(callout('warn', '设置好范围后，点击「进入游戏」让玩家看到这个角色。'));
  root.append(
    h('p', { class: 'text-2 small', text: '活动范围影响角色在公共频道里的自动接话。角色进入游戏后，玩家可通过联系人私聊。' }),
    h('div', { class: 'card' }, mine.length
      ? h('div', { class: 'list' }, mine.map((m) => h('div', { class: 'list-item' }, h('div', { class: 'grow title', text: mapName(m) }), btn('移除', () => changeScope({ map_key: m.map_key }, false), 'ghost sm'))))
      : empty('还没有订阅地图', '添加地图可覆盖该地图下的所有频道。')),
    h('div', { class: 'row' }, h('div', { class: 'grow' }, select), btn('添加', () => select.value && changeScope({ map_key: select.value }, true), 'primary', { disabled: !select.options.length })),
    advanced);
  return root;
}

function agentAbilities(a, reopen, openConnection) {
  const catalog = (S.caps && S.caps.catalog) || [];
  const view = S.capByKey[a.key];
  const have = new Set((view && view.capabilities) || []);
  const boxes = {};
  const renderAbility = (cap) => {
    const box = h('input', { type: 'checkbox' });
    box.checked = have.has(cap.id);
    boxes[cap.id] = box;
    return h('label', { class: 'list-item', style: { cursor: 'pointer', alignItems: 'flex-start' } }, box,
      h('div', { class: 'grow' }, h('div', { class: 'title', text: cap.label || cap.id }), cap.desc ? h('div', { class: 'sub', text: cap.desc }) : null,
        cap.requires_roundtable ? h('div', { class: 'sub', style: { color: 'var(--warn)' }, text: '需要同时打开「参加多人对话」' }) : null));
  };
  const globalSearch = setting('web_search_enabled');
  const online = globalSearch === '1' || globalSearch === true;
  const legacy = catalog.filter((cap) => cap.legacy && have.has(cap.id));
  let removeLegacy = false;
  const visible = catalog.filter((cap) => !cap.legacy && (online || cap.id !== 'online_search'));
  const knowledge = visible.filter((cap) => cap.group === 'knowledge').map(renderAbility);
  const behavior = visible.filter((cap) => cap.group !== 'knowledge').map(renderAbility);
  let stateText = '还没设置过能力，目前只会私聊。';
  if (view?.legacy && !view.present) {
    stateText = '沿用历史只读范围';
  } else if (view?.present) {
    stateText = { available: '已生效', pending: '已保存，正在应用到游戏', failed: '已保存，但应用到游戏时失败了', unavailable: '没有生效（角色可能已被删除）' }[view.availability] || '状态未知';
  }
  const save = btn('保存能力', async () => {
    const caps = [...have].filter((id) => !boxes[id] && (!removeLegacy || !legacy.some((c) => c.id === id)));
    caps.push(...Object.entries(boxes).filter(([, b]) => b.checked).map(([id]) => id));
    if (caps.includes('broadcast_catch') && !caps.includes('join_roundtable')) { toast('「播报自己的渔获」需要同时打开「参加多人对话」', 'bad'); return; }
    try {
      const d = await post('/api/ai/capabilities', { agent_key: a.key, base_revision: view ? view.revision : 0, capabilities: caps });
      toast(d.changed ? '已保存' : '没有变化');
      await reopen();
    } catch (e) { toast(errText(e), 'bad'); if (/已被其他管理员更新/.test(errText(e))) await reopen(); }
  }, 'primary');
  const legacyStatus = h('span', { text: '仍保留历史只读权限' });
  const legacyAction = btn('移除历史权限', () => {
    removeLegacy = !removeLegacy;
    legacyStatus.textContent = removeLegacy ? '保存后移除历史只读权限' : '仍保留历史只读权限';
    legacyAction.textContent = removeLegacy ? '撤销移除' : '移除历史权限';
  }, 'ghost sm');
  const searchNotice = !online && catalog.some((c) => c.id === 'online_search')
    ? h('div', { class: 'list-item' }, h('span', { class: 'grow muted small', text: '全局联网已关闭' }), btn('去配置联网', openConnection, 'ghost sm')) : null;
  return h('div', { class: 'stack' },
    h('p', { class: 'text-2 small', text: '按角色用途选择可查的资料和可做的事。自动接话的时机还受性格影响。' }),
    h('div', { class: 'row' }, h('span', { class: 'muted small', text: '当前状态' }), badge(stateText, view && view.availability === 'available' ? 'ok' : view && view.availability === 'failed' ? 'bad' : '')),
    view && view.availability === 'failed' ? callout('bad', h('span', {}, '能力没能应用到游戏。', h('a', { href: '#', onclick: async (e) => { e.preventDefault(); await post('/api/ai/capabilities', { agent_key: a.key, base_revision: view.revision, capabilities: view.capabilities || [] }); toast('已重试'); await reopen(); } }, '重试'))) : null,
    knowledge.length || searchNotice ? h('div', { class: 'card' }, h('div', { class: 'list' }, h('div', { class: 'list-item title', text: '资料查询' }), knowledge, searchNotice)) : null,
    behavior.length ? h('div', { class: 'card' }, h('div', { class: 'list' }, h('div', { class: 'list-item title', text: '角色行为' }), behavior)) : null,
    catalog.length ? null : empty('读不到能力列表'),
    h('div', { class: 'row' },
      legacy.length ? h('div', { class: 'row muted small grow' }, legacyStatus, legacyAction) : h('span', { class: 'spacer' }),
      save));
}

// 性格 = 「想不想做」：四个旋钮（0–100，默认 50）经同一条派生公式，同时决定行为（起桌、接话、换目标、休息）
// 和表达（用词、语气、主动度）。维度、分段与文案全部来自后端，前端不另存一份定义。
function agentPersonality(a, reopen) {
  const p = S.personality;
  const view = p && (p.agents || []).find((v) => v.agent_key === a.key);
  if (!p || !view) return empty('读不到性格设置');
  const dims = (view.dimensions && view.dimensions.length ? view.dimensions : p.dimensions) || [];
  const bands = p.bands || [];
  const bandOf = (v) => (bands.find((b) => v >= b.min && v <= b.max) || {}).band;
  const values = {};
  const rows = dims.map((dim) => {
    const v = Number((view.values || {})[dim.id] ?? 50);
    values[dim.id] = v;
    const out = h('output', { text: String(v) });
    const text = h('div', { class: 'small text-2', style: { marginTop: '6px', minHeight: '20px' } });
    const describe = (n) => { const b = bandOf(n); text.textContent = (b && dim[b + '_band']) || ''; };
    describe(v);
    const range = h('input', { type: 'range', min: 0, max: 100, step: 1, value: v, 'aria-label': dim.label, oninput: (e) => { const n = Number(e.target.value); values[dim.id] = n; out.textContent = String(n); describe(n); } });
    return h('div', { style: { padding: '14px 0', borderTop: '1px solid var(--line)' } },
      h('b', { style: { fontWeight: 600 }, text: dim.label }),
      h('div', { class: 'range-row', style: { marginTop: '6px' } }, h('span', { class: 'muted small nowrap', style: { width: '84px' }, text: dim.low_end }), range, h('span', { class: 'muted small nowrap', style: { width: '84px', textAlign: 'right' }, text: dim.high_end }), out),
      text);
  });
  const dv = view.derived || {};
  const mins = (s) => (s >= 3600 ? (s / 3600).toFixed(1) + ' 小时' : Math.round(s / 60) + ' 分钟');
  const derived = view.configured && (dv.dwell_floor_seconds || dv.rest_interval_seconds)
    ? h('div', { class: 'card', style: { padding: '12px 14px', background: 'var(--surface-2)' } },
      h('div', { class: 'small muted', style: { marginBottom: '4px' }, text: '按已保存的性格，服务器算出的实际表现' }),
      h('div', { class: 'small' }, dv.dwell_floor_seconds ? `一个钓点至少待 ${mins(dv.dwell_floor_seconds)}` : '', dv.dwell_floor_seconds && dv.rest_interval_seconds ? '，' : '', dv.rest_interval_seconds ? `大约每 ${mins(dv.rest_interval_seconds)}休息一次` : ''))
    : null;
  const save = btn('保存性格', async () => {
    try {
      const d = await post('/api/ai/personality', { agent_key: a.key, base_revision: view.revision || 0, schema_version: p.schema_version, values });
      toast(d.changed ? '已保存' : '没有变化');
      await reopen();
    } catch (e) { toast(errText(e), 'bad'); }
  }, 'primary');
  return h('div', { class: 'stack' },
    h('p', { class: 'text-2 small', text: '性格决定它「想不想」做一件事。同一组旋钮同时影响它的行为（要不要拉人开聊、接不接话、换不换钓点、什么时候休息）和说话方式（用词、语气、主不主动）。能不能做由「能力」决定。' }),
    view.needs_repair ? callout('warn', '这个角色的性格设置有问题，保存一次即可修复。') : null,
    derived,
    h('div', {}, rows),
    h('div', { class: 'row', style: { justifyContent: 'flex-end' } }, save));
}

function agentRecord(a, reopen) {
  const totalKg = (Number(a.catch_total_g || 0) / 1000).toFixed(2);
  const todayKg = (Number(a.catch_today_g || 0) / 1000).toFixed(2);
  const act = a.actor || {};
  return h('div', { class: 'stack' },
    h('div', { class: 'grid-3' },
      h('div', { class: 'card stat' }, h('div', { class: 'k', text: '累计渔获' }), h('div', { class: 'v', text: fmt(a.catch_total || 0) }), h('div', { class: 's', text: totalKg + ' kg' })),
      h('div', { class: 'card stat' }, h('div', { class: 'k', text: '今天' }), h('div', { class: 'v', text: fmt(a.catch_today || 0) }), h('div', { class: 's', text: todayKg + ' kg' })),
      h('div', { class: 'card stat' }, h('div', { class: 'k', text: '记住的好成绩' }), h('div', { class: 'v', text: fmt(a.memory_total || 0) }), h('div', { class: 's', text: '星级与稀有鱼' }))),
    act.activity ? h('div', { class: 'muted small', text: '现在：' + [act.activity, act.loc_name, act.target_name].filter(Boolean).join(' · ') }) : null,
    section({
      title: '清除好成绩记忆', desc: 'AI 会忘记自己钓到过哪些星级、稀有鱼。渔获记录本身不会删除。',
      body: btn('清除', async () => {
        if (!(await confirmDialog({ title: '清除好成绩记忆？', desc: `「${a.display}」会忘记自己的星级、稀有鱼成绩。渔获记录保留。`, confirm: '清除', danger: true }))) return;
        const r = await post('/api/ai/memory/clear', { key: a.key });
        if (!await waitCommand(r.command_id, '已清除')) return;
        await reopen();
      }, 'danger'),
    }));
}

// 运行态命令：后台受理后要等游戏服应用，轮询到结果
async function waitCommand(id, okText) {
  if (!id) { toast('服务器未返回操作编号，请刷新后核对状态', 'bad'); return false; }
  toast('已提交，正在应用到游戏…');
  const deadline = Date.now() + 30000;
  while (Date.now() < deadline) {
    let d;
    try {
      d = await api('/api/ai/commands/' + encodeURIComponent(id), { signal: AbortSignal.timeout(Math.max(1, deadline - Date.now())) });
    } catch (e) {
      if (e.name === 'TimeoutError') break;
      toast('无法确认操作是否生效，请刷新后核对：' + errText(e), 'bad');
      return false;
    }
    if (d.status === 'applied') { toast(okText); return true; }
    if (d.status === 'rejected') {
      const reasons = {
        agent_missing: '角色已不存在',
        agent_disabled: '角色已停用',
        emergency_stop: 'AI 紧急停止已开启，请解除后重试',
        queued_before_resume: '操作属于上一次运行，请重新提交',
      };
      toast('游戏服拒绝了操作：' + (reasons[d.reason] || d.reason || '规则不允许'), 'bad');
      return false;
    }
    if (d.status === 'expired') { toast('操作已过期，请重试', 'bad'); return false; }
    await new Promise((r) => setTimeout(r, 1000));
  }
  toast('游戏服仍在等待处理，稍后刷新核对状态', 'warn');
  return false;
}

// ---------- 设置类页面通用 ----------
// 后端对几个历史开关给出的确认状态（如「未确认」= 还是默认值、没被明确选过），原样显示
function securityBadge(key) {
  const st = (((S.status || {}).security || {}).keys || {})[key];
  if (!st || st.state === 'off') return null;
  return badge(st.display, 'warn');
}
function settingsForm() {
  const form = new Form();
  savebar(form, { onSave: async (values) => { await post('/api/ai/settings', toPayload(form, values)); await S.refresh(); } });
  setLeaveGuard(() => form.isDirty());
  return form;
}

// ---------- 对话 ----------
function talkView() {
  const form = settingsForm();
  const cell = (key) => {
    if (!S.specs[key]) return h('td', { class: 'muted', text: '—' });
    const spec = specFor(key);
    const f = form.field(spec, settingValue(spec));
    const wrap = h('div', {});
    f.input = h('input', { class: 'input num', inputmode: 'numeric', value: f.original, 'aria-label': spec.label, placeholder: String(spec.def ?? ''), oninput: () => form.refresh() });
    f.errEl = h('div', { class: 'small', style: { color: '#f08a97' }, hidden: true });
    f.row = wrap;
    wrap.append(f.input, f.errEl,
      h('div', { class: 'muted', style: { fontSize: '11.5px', marginTop: '4px' }, text: `可填 ${spec.min ?? 0}–${spec.max}` }));
    return h('td', {}, wrap);
  };
  const table = h('div', { class: 'table-wrap' }, h('table', { class: 'table' },
    h('thead', {}, h('tr', {}, h('th', { text: '场景', style: { width: '180px' } }), SCENE_COLS.map(([, l, u]) => h('th', {}, l, h('span', { style: { fontWeight: 400 }, text: '（' + u + '）' }))))),
    h('tbody', {}, SCENES.map(([g, name, desc]) => {
      const p = 'ai_profile_' + g + '_';
      return h('tr', { style: { verticalAlign: 'top' } }, h('td', { style: { paddingTop: '14px' } }, h('b', { text: name }), h('span', { class: 'sub', text: desc })),
        SCENE_COLS.map(([col]) => cell(p + (g === 'roundtable' && col === 'history_messages' ? 'context_messages' : col))));
    }))));
  // 软目标不能大于硬上限
  const origValidate = form.validate.bind(form);
  form.validate = (keys) => {
    let ok = origValidate(keys);
    for (const [g] of SCENES) {
      const soft = form.fields.get(`ai_profile_${g}_reply_soft_chars`), hard = form.fields.get(`ai_profile_${g}_reply_chars`);
      if (soft && hard && Number(form.value(soft.spec.key)) > Number(form.value(hard.spec.key))) {
        soft.input.setAttribute('aria-invalid', 'true'); soft.errEl.hidden = false; soft.errEl.textContent = '不能超过「最长」'; ok = false;
      }
    }
    return ok;
  };
  return h('div', {},
    localKnowledgeSection(),
    section({
      title: '回复长度与记忆',
      desc: 'AI 每次回复前会回看最近的聊天记录。回看得越多，越懂上下文，但每次调用更慢、更费钱。',
      body: h('div', { class: 'stack' }, table,
        h('dl', { class: 'kv small', style: { gridTemplateColumns: '90px 1fr' } },
          h('dt', { text: '期望长度' }), h('dd', { class: 'muted', text: '告诉 AI 大概说多少字。' }),
          h('dt', { text: '最长' }), h('dd', { class: 'muted', text: '超过这个字数会被截断。必须不小于期望长度。' }),
          h('dt', { text: '回看消息 / 字数' }), h('dd', { class: 'muted', text: '最多参考最近多少条、多少字的聊天记录。' }),
          h('dt', { text: '上下文上限' }), h('dd', { class: 'muted', text: '一次调用能塞给模型的总量（tokens，约等于字数）。一般不用改。' }))),
    }),
    section({ title: '游戏资料', body: form.row(specFor('ai_profile_internal_chars'), settingValue(specFor('ai_profile_internal_chars'))) }));
}

// ---------- 虚拟钓鱼 ----------
function fishingView() {
  const form = settingsForm();
  const row = (key) => {
    if (!S.specs[key]) return null;
    const el = form.row(specFor(key), settingValue(specFor(key)));
    const b = securityBadge(key);
    if (b) { b.title = '这个开关还是默认值，没有被明确设置过'; el.querySelector('.s-label').append(b); }
    return el;
  };
  const ratioRow = (key) => {
    if (!S.specs[key]) return null;
    const spec = specFor(key, { unit: '' });
    return form.row(spec, setting(key));
  };
  // 能力是按角色授予的：没有任何角色勾选时，这组设置不会生效。
  // 读不到能力目录时返回 null，不冒充「一个都没有」。
  const idleKeys = () => (S.caps ? (S.agents || []).filter((a) => {
    const v = S.capByKey[a.key];
    return !!(v && (v.capabilities || []).includes('idle_chat'));
  }).map((a) => a.display || a.key) : null);
  // 把两个数字翻译成一句人话：管理员不用自己算 60 / 句数。
  const idlePreview = h('p', { class: 'small text-2', style: { marginTop: '2px' } });
  const refreshIdlePreview = () => {
    const minutes = Number(form.value('ai_idle_silence_min'));
    if (!Number.isFinite(minutes) || minutes <= 0) { idlePreview.textContent = ''; return; }
    idlePreview.textContent = `效果：距上次上鱼超过 1 分钟、频道安静约 ${minutes} 分钟后，由上面「说话节奏」决定最多说几句。`;
  };
  // 说话节奏：预设数字来自后端 talk_moods，前端不维护第二份预算表。
  const moods = (S.status && S.status.talk_moods) || [];
  const moodRow = moods.length
    ? form.row({
      key: 'ai_fishing_mood', label: '整体氛围', kind: 'select',
      options: [...moods.map((m) => [m.id, m.label]), ['custom', '自定义']],
      help: '频道里所有 AI 角色合计；上鱼播报优先，闲时搭话只用剩余额度。',
    }, setting('ai_fishing_mood') || 'normal')
    : null;
  const talkPreview = h('p', { class: 'small text-2', style: { marginTop: '2px' } });
  const talkTip = h('p', { class: 'small muted', style: { marginTop: '4px' } },
    '接话人数随氛围：冷清 3 / 正常 5 / 热闹 10 / 不限全部。实际取频道内已授权、当前可接话的角色数；闲时搭话还要求正在钓鱼。圆桌按一次事件计 1 格预算。');
  const customTalk = h('details', { class: 'advanced', hidden: true },
    h('summary', {}, '自定义节奏', h('small', { text: '（只有「自定义」档生效）' })),
    h('div', {},
      row('ai_talk_hourly_limit'), row('ai_talk_min_gap_s'), row('ai_talk_burst'),
      row('ai_talk_roundtable_speakers'),
      h('p', { class: 'muted small', style: { padding: '8px 0' }, text: '四段权重决定说话落在小时的哪个时段；全 0 会退回均匀。' }),
      row('ai_talk_weight_0'), row('ai_talk_weight_1'), row('ai_talk_weight_2'), row('ai_talk_weight_3')));
  const refreshTalkPreview = () => {
    if (!moodRow) { customTalk.hidden = true; return; }
    const id = form.value('ai_fishing_mood');
    const preset = moods.find((m) => m.id === id);
    if (preset) {
      const rt = preset.roundtable_speakers ? `；一次圆桌最多 ${preset.roundtable_speakers} 人、${preset.roundtable_turns} 条` : '';
      if (preset.budget > 0) {
        talkPreview.textContent = `${preset.label}：每小时最多约 ${preset.budget} 句，两条消息至少间隔 ${Math.round(preset.min_gap_s / 60)} 分钟${rt}。`;
      } else {
        talkPreview.textContent = `${preset.label}：不设每小时上限，两条消息至少间隔 ${Math.max(1, Math.round(preset.min_gap_s / 60))} 分钟${rt}；模型调用会明显增加。`;
      }
    } else if (id === 'custom') {
      const b = Number(form.value('ai_talk_hourly_limit'));
      const g = Number(form.value('ai_talk_min_gap_s'));
      const sp = Number(form.value('ai_talk_roundtable_speakers'));
      const rt = Number.isFinite(sp) && sp >= 2 ? `；一次圆桌最多 ${sp} 人、${Math.min(16, sp + 2)} 条` : '';
      talkPreview.textContent = Number.isFinite(b) && b > 0
        ? `自定义：每小时最多 ${b} 句${Number.isFinite(g) && g > 0 ? `，至少间隔 ${Math.round(g / 6) / 10} 分钟` : ''}${rt}。`
        : '';
    } else {
      talkPreview.textContent = '';
    }
    customTalk.hidden = id !== 'custom';
    if (id === 'custom') customTalk.open = true;
  };
  const idleChange = form.onChange;
  form.onChange = (f) => { idleChange?.(f); refreshTalkPreview(); refreshIdlePreview(); };
  const idleStatus = () => {
    const keys = idleKeys();
    if (keys === null) return null; // 能力目录读取失败：不猜
    if (keys.length === 0) return callout('info', '还没有角色开启「闲时搭话」。到「角色 → 能力」勾选后，这里的设置才会生效。');
    return h('p', { class: 'small muted', text: `已开启：${keys.join('、')}` });
  };
  const idleBody = h('div', {}, idleStatus(), row('ai_idle_silence_min'), idlePreview);
  refreshTalkPreview();
  refreshIdlePreview();
  return h('div', {},
    section({ title: '基本', body: h('div', {}, row('ai_fishing_enabled')) }),
    section({
      title: '说话节奏',
      desc: '决定频道里一小时总共能听到几句。四档预设直接选；想细调再选「自定义」。',
      body: h('div', {}, moodRow, talkPreview, talkTip, customTalk),
    }),
    section({
      title: '闲时搭话',
      desc: '没鱼、也没人说话的时候，AI 可以自己找个话头。需要先打开上面的「虚拟钓鱼」；每个角色还要在「角色 → 能力」里勾选「闲时搭话」。',
      body: idleBody,
    }),
    section({ title: '记忆', desc: '聊天时 AI 会不会提起自己的钓鱼成绩。', body: h('div', {}, row('ai_catch_brief_inject'), row('ai_memory_inject'),
      h('details', { class: 'advanced' }, h('summary', {}, '更多记忆设置'), h('div', {}, row('ai_memory_recent'), row('ai_memory_best'), row('ai_memory_max_rows')))) }),
    section({
      title: '高级', desc: '一般不需要修改。',
      body: h('div', {},
        h('details', { class: 'advanced' }, h('summary', {}, '上鱼节拍'), h('div', {}, row('ai_fishing_base_interval_ms'), row('ai_fishing_max_interval_ms'))),
        h('details', { class: 'advanced' }, h('summary', {}, '钓到什么鱼 ', h('small', { text: '（各类鱼所占比例，0–1）' })),
          h('div', {}, h('p', { class: 'muted small', style: { padding: '8px 0' }, text: '决定 AI 角色的渔获里各类鱼占多少。数值是比例，例如 0.02 表示约 2%。' }),
            ratioRow('ai_fishing_star_weight'), ratioRow('ai_fishing_super_weight'), ratioRow('ai_fishing_near_weight'), ratioRow('ai_fishing_miss_weight'), ratioRow('ai_fishing_near_ratio'), ratioRow('ai_fishing_miss_ratio')))),
    }));
}

// ---------- 连接与安全 ----------
function connView() {
  const st = S.status;
  const cred = st.credential || {};
  const configured = cred.state === 'configured';
  const form = new Form();
  const result = h('div', { class: 'small', style: { minHeight: '20px' } });
  const fields = h('div', { class: 'form-grid' },
    form.stacked({ key: 'base_url', label: '服务商地址', kind: 'string', span: true, placeholder: '例如 https://api.deepseek.com', help: '兼容 OpenAI 接口格式的服务商地址，在服务商的文档里能找到。' }, st.base_url || ''),
    form.stacked({ key: 'model_id', label: '模型名称', kind: 'string', placeholder: '按服务商文档填写' }, st.model_id || ''),
    form.stacked({ key: 'api_key', label: 'API 密钥', kind: 'string', secret: true, placeholder: configured ? `已保存（${cred.masked || '已加密'}），不改就留空` : 'sk-…', help: '加密保存在这台电脑上，页面不会显示出来。' }, ''));
  const saveBtn = btn('保存', async () => {
    const v = { base_url: form.value('base_url'), model_id: form.value('model_id'), api_key: form.value('api_key') };
    if (!v.base_url || !v.model_id) { toast('请填写服务商地址和模型名称', 'bad'); return; }
    if (!configured && !v.api_key) { toast('请填写 API 密钥', 'bad'); return; }
    saveBtn.disabled = true;
    try {
      const d = await post('/api/ai/config', { ...v, revision: Number(st.revision) || 0 });
      if (d && d.saved) { toast('已保存。建议马上测试一次连接。'); await S.refresh(); }
    } catch (e) { toast(errText(e), 'bad'); } finally { saveBtn.disabled = false; }
  }, 'primary');
  const testBtn = btn('测试连接', async () => {
    if (form.isDirty()) { result.replaceChildren(callout('warn', '有修改还没保存。先点「保存」，再测试。')); return; }
    testBtn.disabled = true;
    result.replaceChildren(h('span', { class: 'muted', text: '正在测试，会向服务商发送一句测试文字…' }));
    try {
      const d = await post('/api/ai/test', {});
      result.replaceChildren(d.ok ? callout('ok', '连接正常，模型可以回复。') : callout('bad', '连接失败：' + (d.error || '未知原因')));
      await loadStatus();
    } catch (e) { result.replaceChildren(callout('bad', '连接失败：' + errText(e))); }
    finally { testBtn.disabled = !configured; }
  }, '', { disabled: !configured });
  const r = st.retest || {};
  if (configured && r.current && r.ok === false && r.display) result.replaceChildren(callout('bad', '上次测试失败，请检查地址、模型名称和密钥。'));

  const searchForm = settingsForm();
  const row = (key) => (S.specs[key] ? searchForm.row(specFor(key), settingValue(specFor(key))) : null);
  const search = st.web_search || {};
  const searchSwitch = row('web_search_enabled');
  const searchMode = searchForm.row({
    key: 'web_search_mode', kind: 'select', label: '搜索方式',
    options: [['local', '仅本地资料'], ['native', '厂商原生搜索'], ['custom', '自定义搜索服务']],
  }, search.mode || (setting('search_endpoint') ? 'custom' : 'local'));
  const searchEndpoint = row('search_endpoint');
  const searchState = h('p', { class: 'small muted' });
  const refreshSearch = () => {
    const mode = searchForm.value('web_search_mode');
    if (searchEndpoint) searchEndpoint.hidden = mode !== 'custom';
    if (mode === 'native') {
      searchState.textContent = search.native_supported
        ? (search.native_confirmed_at ? '当前连接已确认执行过搜索 · ' + relTime(search.native_confirmed_at) : '当前连接支持原生搜索，等待首次搜索确认。')
        : '当前连接不支持原生搜索。首版支持 DeepSeek 官方接口的 deepseek-flash，当前连接仍可使用本地资料和聊天。';
    } else if (mode === 'custom') {
      searchState.textContent = searchForm.value('search_endpoint') ? '' : '填写兼容的搜索服务地址后可用。';
    } else {
      searchState.textContent = '';
    }
    searchState.hidden = !searchState.textContent;
  };
  const onSearchChange = searchForm.onChange;
  searchForm.onChange = (form) => { onSearchChange?.(form); refreshSearch(); };
  refreshSearch();
  const bridge = st.bridge || {};
  const queue = st.queue || {};
  return h('div', {},
    section({
      title: '模型服务', desc: 'AI 角色的回复由这里配置的大模型服务生成，会产生服务商的调用费用。',
      actions: retestBadge(),
      body: h('div', { class: 'stack' }, fields, result),
      foot: [testBtn, saveBtn],
    }),
    section({ id: 'ai-web-search', title: '联网搜索', body: h('div', {}, searchSwitch, searchMode, searchEndpoint, searchState,
      h('details', { class: 'advanced' }, h('summary', {}, '高级'), row('max_tool_rounds'))) }),
    section({
      title: '紧急暂停', desc: '出现异常（比如 AI 胡言乱语、费用异常）时，一键让所有角色停止说话。',
      body: h('div', { class: 'row' }, emergency().stopped ? badge('已暂停', 'bad', true) : badge('正常运行', 'ok', true), h('span', { class: 'spacer' }),
        emergency().stopped ? btn('恢复 AI', () => setEmergency(false), 'primary') : btn('暂停所有 AI', () => setEmergency(true), 'danger')),
    }),
    section({
      title: '运行诊断', desc: '排查问题时看这里。',
      body: h('dl', { class: 'kv' },
        h('dt', { text: 'AI 服务程序' }), h('dd', {}, bridge.online ? badge('运行中', 'ok', true) : badge('没有运行', 'warn', true)),
        h('dt', { text: '等待回复' }), h('dd', { text: `${queue.pending || 0} 条排队，${queue.processing || 0} 条处理中` }),
        h('dt', { text: '最近一次失败' }), h('dd', { text: (st.last_error && st.last_error.error) ? `${st.last_error.agent || ''} ${st.last_error.error}` : '没有' })),
    }));
}
