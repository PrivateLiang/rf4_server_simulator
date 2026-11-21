// AI 助手页前端模块（唯一入口）。
// 由官方后台页的窄桥 window.RF4Admin 提供 api / toast / 模态框 / 路由注册；
// 本模块不读取 TOKEN，不把凭据拼进 URL，不修改官方全局 go。
const host = window.RF4Admin;

const S = {
  mounted: false,
  loadPromise: null,
  listenersBound: false,
  active: false,
  epoch: 0,
  specs: [],
  modules: [],
  settings: {},
  settingStates: {},
  status: {},
  agents: [],
  selectedKey: '',
  capabilities: null,
  capByKey: {},
  capDirty: false,
  channels: null,
  channelName: 'nav1_03',
  drafts: {},
  authStale: false,
  actorMaster: true,
  // 急停状态卡：只反映「已回读」的运行事实；保存中/待保存不冒充已生效。
  emergency: { pending: false, saving: false, formEpoch: null },
  lastFocus: null,
  // 四维性格与评分草稿（T-107）。草稿按角色分开：草稿从 service 回读值出发，
  // 评分建议只写进草稿；只有显式保存才落库。
  personality: {
    loaded: false,
    views: {},
    dimensions: [],
    bands: [],
    schemaVersion: 1,
    destination: null,
    emergency: null,
    draft: {},
    ui: null,
    notice: {},
    superseded: [],
    score: { pending: false, seq: 0, controller: null, key: '' },
  },
};

// 状态读取序号：迟到的 /api/ai/status 响应不得覆盖更新的状态。
let aiStatusSeq = 0;
let aiRetestSeq = 0;
// 性格回读序号：迟到的旧响应不得覆盖新版本或另一角色（T-107 P4）。
// 与状态序号同一口径：只有最后一次发出的回读允许应用到页面。
let aiPersonalitySeq = 0;

const timers = new Set();

const ACTIVITY_LABELS = {
  fishing: ['在钓鱼', 'fishing'],
  resting: ['休息中', 'resting'],
  moving: ['换点中', 'moving'],
  idle: ['闲逛中', 'idle'],
};

const BUDGET_GROUP_TITLE = {
  channel: '频道',
  dm: '私聊',
  roundtable: '圆桌',
  profile: '资料预算',
};

const CAPABILITY_AVAILABILITY = {
  available: '已生效',
  pending: '已保存，等待游戏服应用',
  failed: '已保存，但运行命令未生效（可重试）',
  unavailable: '配置未生效（角色不存在或旧代次）',
};

function $(id) {
  return document.getElementById(id);
}

function el(tag, attrs, ...children) {
  const node = document.createElement(tag);
  if (attrs) {
    for (const [key, value] of Object.entries(attrs)) {
      if (value === undefined || value === null) continue;
      if (key === 'text') node.textContent = String(value);
      else if (key === 'class') node.className = String(value);
      else if (key === 'value') node.value = String(value);
      else if (key === 'checked') node.checked = !!value;
      else if (key === 'selected') node.selected = !!value;
      else if (key === 'disabled') node.disabled = !!value;
      else node.setAttribute(key, String(value));
    }
  }
  for (const child of children) {
    if (child === undefined || child === null) continue;
    node.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return node;
}

function actionButton(cls, action, label, data) {
  const btn = el('button', { class: cls, type: 'button', 'data-action': action, text: label });
  if (data) {
    for (const [key, value] of Object.entries(data)) btn.dataset[key] = String(value);
  }
  return btn;
}

function setText(id, value) {
  const node = $(id);
  if (node) node.textContent = value === undefined || value === null ? '' : String(value);
}

function setInput(id, value) {
  const node = $(id);
  if (node) node.value = value === undefined || value === null ? '' : String(value);
}

function sleep(ms) {
  return new Promise((resolve) => {
    const timer = setTimeout(() => {
      timers.delete(timer);
      resolve();
    }, ms);
    timers.add(timer);
  });
}

function cancelPending() {
  S.epoch += 1;
  for (const timer of timers) clearTimeout(timer);
  timers.clear();
}

// 请求取消：进入/离开/注销时中止在途请求；被取消的调用不弹错误提示。
let apiController = null;

function resetAPIController() {
  if (apiController) apiController.abort();
  apiController = new AbortController();
}

function abortAPIController() {
  if (apiController) apiController.abort();
  apiController = null;
}

function authErrorText(e) {
  const text = e && e.message ? String(e.message) : '';
  return /HTTP 401|未登录|登录已过期|登录失效|Unauthorized/i.test(text);
}

async function api(path, opt) {
  const signal = apiController ? apiController.signal : undefined;
  try {
    return await host.api(path, { ...(opt || {}), signal: signal });
  } catch (e) {
    if (isAbort(e)) return;
    if (authErrorText(e)) {
      // 登录由官方外壳接管；这里只标记状态并保留草稿，不清空任何表单。
      S.authStale = true;
      if (S.mounted) renderOverview(S.status);
      throw new Error('需登录：登录已过期，草稿已保留');
    }
    if (e && e.name === 'AbortError') {
      const aborted = new Error('请求已取消');
      aborted.aborted = true;
      throw aborted;
    }
    throw e;
  }
}

function isAbort(e) {
  return !!(e && e.aborted);
}

// ---------- 加载与挂载 ----------

function bindMountEvents() {
  const mount = $('view-ai');
  if (!mount || S.listenersBound) return;
  S.listenersBound = true;
  mount.addEventListener('click', onMountedClick);
  mount.addEventListener('change', onMountedChange);
  mount.addEventListener('input', onMountedInput);
  const modalBg = $('aiModalBg');
  if (modalBg) {
    modalBg.addEventListener('click', (event) => {
      if (event.target === modalBg) closeAIModal();
    });
  }
  document.addEventListener('keydown', (event) => {
    if (event.key === 'Escape') closeAIModal();
    if (event.key === 'Tab') trapModalFocus(event);
  });
}

function loadStylesheet() {
  return new Promise((resolve, reject) => {
    if (document.querySelector('link[data-ai-css]')) return resolve();
    const link = document.createElement('link');
    link.rel = 'stylesheet';
    link.href = new URL('ai.css', import.meta.url).href;
    link.dataset.aiCss = '1';
    link.addEventListener('load', () => resolve(), { once: true });
    link.addEventListener('error', () => {
      link.remove();
      reject(new Error('ai.css 加载失败'));
    }, { once: true });
    document.head.appendChild(link);
  });
}

function ensureLoaded() {
  if (S.mounted) return Promise.resolve();
  if (S.loadPromise) return S.loadPromise;
  S.loadPromise = (async () => {
    const mount = $('view-ai');
    if (!mount) throw new Error('缺少 #view-ai 挂载点');
    await loadStylesheet();
    const response = await fetch(new URL('index.html', import.meta.url), { cache: 'no-cache' });
    if (!response.ok) throw new Error('片段请求失败：HTTP ' + response.status);
    const contentType = (response.headers.get('content-type') || '').toLowerCase();
    if (!contentType.includes('text/html')) throw new Error('片段返回类型不是 HTML：' + contentType);
    const text = await response.text();
    const doc = new DOMParser().parseFromString(text, 'text/html');
    const root = doc.querySelector('[data-ai-fragment="1"]');
    if (!root) throw new Error('片段缺少 AI 挂载标记');
    if (doc.querySelector('script, style, link')) throw new Error('片段包含脚本/样式/外部资源');
    for (const attr of ['onclick', 'onchange', 'oninput', 'onload', 'onerror', 'onsubmit']) {
      if (doc.querySelector('[' + attr + ']')) throw new Error('片段包含行内事件 ' + attr);
    }
    if (doc.querySelector('[style]')) throw new Error('片段包含行内样式');
    mount.replaceChildren(document.importNode(root, true));
    S.mounted = true;
  })().catch((error) => {
    S.mounted = false;
    throw error;
  }).finally(() => {
    S.loadPromise = null;
  });
  return S.loadPromise;
}

function renderLoadError(error) {
  const mount = $('view-ai');
  if (!mount) return;
  const card = el('div', { class: 'card ai-load-error' });
  card.append(el('div', { class: 'section-head' }, el('div', {}, el('h2', { text: 'AI 页面加载失败' }))));
  card.append(el('p', {
    class: 'muted',
    text: '无法加载 AI 前端资源：' + (error && error.message ? error.message : '未知错误') + '。官方其他页面不受影响。',
  }));
  card.append(el('div', { class: 'actions' }, actionButton('btn', 'retry-load', '重试')));
  mount.replaceChildren(card);
}

// AI 自己的模态框挂在 #view-ai 子树内，样式才能继续遵守 #view-ai 根边界。
function openAIModal(title, sub, body) {
  const bg = $('aiModalBg');
  const box = $('aiModal');
  if (!bg || !box) return;
  S.lastFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null;
  const head = el('div', { class: 'ai-modal-head' },
    el('div', {}, el('h3', { text: title }), el('div', { class: 'ai-modal-sub', text: sub || '' })),
    actionButton('btn alt sm', 'ai-modal-close', '关闭'));
  box.replaceChildren(head, body);
  bg.hidden = false;
  box.focus();
}

function closeAIModal() {
  const bg = $('aiModalBg');
  const box = $('aiModal');
  if (box) box.replaceChildren();
  if (bg) bg.hidden = true;
  const back = S.lastFocus;
  S.lastFocus = null;
  if (back && document.contains(back) && typeof back.focus === 'function') back.focus();
}

function trapModalFocus(event) {
  const box = $('aiModal');
  const bg = $('aiModalBg');
  if (!box || !bg || bg.hidden) return;
  const items = Array.from(box.querySelectorAll('button,input,select,textarea,summary,a[href]'))
    .filter((node) => !node.disabled && node.offsetParent !== null);
  if (!items.length) return;
  const first = items[0];
  const last = items[items.length - 1];
  if (event.shiftKey && document.activeElement === first) {
    event.preventDefault();
    last.focus();
  } else if (!event.shiftKey && document.activeElement === last) {
    event.preventDefault();
    first.focus();
  }
}

async function enter() {
  S.active = true;
  resetAPIController();
  bindMountEvents();
  try {
    await ensureLoaded();
  } catch (error) {
    renderLoadError(error);
    return;
  }
  if (!S.active) return;
  await loadAI();
}

async function refresh() {
  if (!S.mounted) return enter();
  await loadAI();
}

function leave() {
  S.active = false;
  closeAIModal();
  abortAPIController();
  abortPersonalityScore();
  cancelPending();
}

// ---------- 草稿 ----------
// 只存在内存：登录失效、视图切换或刷新失败都不清空；保存成功才丢弃对应字段。

function rememberDraft(id, value) {
  if (!id || value === undefined || value === null) return;
  S.drafts[id] = String(value);
}

function draftOr(id, fallback) {
  return Object.prototype.hasOwnProperty.call(S.drafts, id) ? S.drafts[id] : (fallback === undefined ? '' : fallback);
}

function clearDrafts(ids) {
  for (const id of ids) delete S.drafts[id];
}

function applyDraftInput(id, serverValue) {
  setInput(id, draftOr(id, serverValue));
}

// ---------- 事件委托 ----------

function onMountedClick(event) {
  if (!(event.target instanceof Element)) return;
  const trigger = event.target.closest('[data-action]');
  const mount = $('view-ai');
  if (!trigger || !mount || !mount.contains(trigger)) return;
  const key = trigger.dataset.key;
  switch (trigger.dataset.action) {
    case 'ai-tab':
      aiTab(trigger.dataset.tab);
      break;
    case 'test-ai':
      testAI();
      break;
    case 'save-ai-config':
      saveAIConfig();
      break;
    case 'save-emergency':
      saveEmergency();
      break;
    case 'refresh-status':
      loadAI().then((ok) => { if (ok) host.toast('已刷新'); });
      break;
    case 'overview-next':
      runOverviewNext();
      break;
    case 'copy-hello':
      copyHello();
      break;
    case 'goto-tab':
      aiTab(trigger.dataset.tab);
      if (trigger.dataset.focus === 'test') {
        const btn = document.querySelector('[data-action="test-ai"]');
        if (btn) btn.focus();
      }
      break;
    case 'subscribe-map':
      aiSubscribeMap(trigger.dataset.enable === '1');
      break;
    case 'subscribe-channel':
      aiSubscribe(trigger.dataset.enable === '1');
      break;
    case 'create-ai':
      createAI();
      break;
    case 'reload-agents':
      loadAI();
      break;
    case 'edit-ai':
      editAI(key);
      break;
    case 'save-ai-edit':
      saveAIEdit(key, 'all');
      break;
    case 'save-ai-edit-base':
      saveAIEdit(key, 'base');
      break;
    case 'save-ai-edit-thinking':
      saveAIEdit(key, 'thinking');
      break;
    case 'join-ai':
      joinAI(key, trigger.dataset.join === '1');
      break;
    case 'delete-ai':
      deleteAI(key);
      break;
    case 'select-agent':
      selectAgent(trigger.dataset.key);
      break;
    case 'clear-memory':
      clearAIMemory();
      break;
    case 'save-settings':
      saveSettingsGroup(trigger.dataset.module, trigger.dataset.group);
      break;
    case 'save-capabilities':
      saveCapabilities(false);
      break;
    case 'retry-capabilities':
      saveCapabilities(true);
      break;
    case 'reload-capabilities':
      loadCapabilities().then(() => host.toast('已刷新生效状态'));
      break;
    case 'suggest-personality':
      suggestPersonality();
      break;
    case 'cancel-personality-score':
      cancelPersonalityScore();
      break;
    case 'undo-personality-dim':
      undoPersonalityDimension(trigger.dataset.dim);
      break;
    case 'undo-personality-all':
      undoPersonalityAll();
      break;
    case 'save-personality':
      savePersonality();
      break;
    case 'reload-personality':
      reloadPersonality();
      break;
    case 'goto-agent-edit':
      editAI(S.selectedKey);
      break;
    case 'personality-switch-save':
      switchPersonalityWithSave(trigger.dataset.next);
      break;
    case 'personality-switch-discard':
      switchPersonalityDiscard(trigger.dataset.next);
      break;
    case 'personality-switch-cancel':
      switchPersonalityCancel();
      break;
    case 'ai-modal-close':
      closeAIModal();
      break;
    case 'retry-load':
      enter();
      break;
    default:
      break;
  }
}

function onMountedChange(event) {
  if (!(event.target instanceof Element)) return;
  const trigger = event.target.closest('[data-change]');
  if (!trigger) return;
  const field = event.target;
  if (field && field.id && field.dataset && field.dataset.draft === '1') {
    rememberDraft(field.id, field.type === 'checkbox' ? (field.checked ? '1' : '0') : field.value);
  }
  switch (trigger.dataset.change) {
    case 'load-ai-channels':
      loadChannels();
      break;
    case 'load-ai-maps':
      renderSubscriptions();
      break;
    case 'select-agent':
      selectAgent(trigger.value);
      break;
    case 'emergency-edit':
      // 只标记「待保存」：控件勾选值不代表运行事实，回读后才更新状态卡。
      S.emergency.pending = true;
      S.emergency.saving = false;
      setEmergencyCard('待保存');
      break;
    default:
      break;
  }
}

function onMountedInput(event) {
  if (!(event.target instanceof Element)) return;
  const trigger = event.target.closest('[data-input]');
  if (trigger && trigger.dataset.input === 'filter-agents') {
    renderAgentList(S.agents);
    return;
  }
  const field = event.target;
  if (field && field.id && field.dataset && field.dataset.draft === '1') {
    rememberDraft(field.id, field.value);
  }
  if (field && field.dataset && field.dataset.key) {
    clearFieldError(field.dataset.key);
  }
}

// ---------- 状态消费 ----------

// 页面只呈现后端给的判定与状态：急停、实际去向、修订、凭据、复测。
// 前端不解释「缺省」，也不保存任何密钥或域名清单。
function applyAIStatus(d) {
  const sec = d.security || {};
  S.settingStates = sec.keys || {};
  const previousBinding = (S.status.emergency_stop || {}).epoch;
  const latestBinding = (d.emergency_stop || {}).epoch;
  if (S.status.revision !== d.revision || previousBinding !== latestBinding ||
      (S.status.retest || {}).attempt !== (d.retest || {}).attempt) {
    setText('aiTestResult', '');
  }
  S.status = d;
  const actor = (sec.keys || {}).ai_actor_enabled || {};
  S.actorMaster = actor.state !== 'off' && actor.state !== 'invalid';
  applyEmergencyStatus(d);
  setText('aiStatusDestination', destDisplay(d.destination));
  setText('aiStatusRetest', (d.retest || {}).display || '待复测');
  setText('aiKeyState', (d.credential || {}).display || '尚未配置密钥');
  setText('aiSaveState', d.saved_display || '尚未保存可用连接');
  return sec;
}

// applyEmergencyStatus 只认三种来源：正在保存、待保存、后端最新回读。
// 控件勾选值、通用「已保存」提示都不能证明急停的运行状态。
function applyEmergencyStatus(d) {
  if (S.emergency.saving) {
    setEmergencyCard('正在保存');
    return;
  }
  if (S.emergency.pending) {
    setEmergencyCard('待保存');
    return;
  }
  const em = (d || {}).emergency_stop;
  if (!em) {
    setEmergencyCard('状态待确认，请刷新');
    return;
  }
  setEmergencyCard(em.display || (em.stopped ? '急停已开启' : '急停已解除'));
}

function setEmergencyCard(text) {
  setText('aiStatusEmergency', text);
}

// destDisplay 只显示后端给的域与模型，不在前端拼地址或推断主机。
function destDisplay(dest) {
  const d = dest || {};
  if (!d.host) return '未配置';
  return d.host + (d.model ? ' / ' + d.model : '');
}

function timeText(ts) {
  if (!ts) return '待确认';
  return new Date(Number(ts) * 1000).toLocaleString('zh-CN');
}

function relativeMinutes(ts) {
  if (!ts) return '';
  const minutes = Math.floor((Date.now() / 1000 - Number(ts)) / 60);
  if (minutes < 1) return '刚开始';
  if (minutes < 60) return '已 ' + minutes + ' 分钟';
  return '已 ' + Math.floor(minutes / 60) + ' 小时';
}

// ---------- 读取 ----------

async function loadAI() {
  const seq = ++aiStatusSeq;
  try {
    const d = await api('/api/ai/status');
    if (seq !== aiStatusSeq) return true;
    S.authStale = false;
    applyAIStatus(d);
    S.specs = d.specs || [];
    S.modules = d.modules || [];
    S.settings = d.settings || {};
    S.agents = d.agents || [];
    ensureSelected();
    setInput('aiBaseURL', '');
    setInput('aiModelID', '');
    setInput('aiKey', '');
    renderConn(d);
    renderBudget();
    renderFishing(d);
    renderAgentList(S.agents);
    renderOverview(d);
    renderStatus(d);
    await loadCapabilities();
    await loadPersonality();
    await loadChannels();
    renderOverview(S.status);
    return true;
  } catch (e) {
    if (isAbort(e)) return false;
    host.toast('读取 AI 状态失败：' + e.message, 'bad');
    renderOverview(S.status || {});
    return false;
  }
}

async function loadCapabilities() {
  try {
    const d = await api('/api/ai/capabilities');
    if (!d) return;
    S.capabilities = d;
    S.capByKey = {};
    for (const item of (d.agents || [])) S.capByKey[item.key] = item;
    renderCapabilities();
    if (S.agents && S.agents.length) renderAgentList(S.agents);
  } catch (e) {
    if (isAbort(e)) return;
    host.toast('读取能力目录失败：' + e.message, 'bad');
  }
}

async function loadChannels() {
  const select = $('aiChannelName');
  const name = (select && select.value) || S.channelName || 'nav1_03';
  try {
    const d = await api('/api/ai/channels?name=' + encodeURIComponent(name));
    if (!d) return;
    S.channels = d;
    S.channelName = d.channel || name;
    const channelSelect = $('aiChannelName');
    if (channelSelect) {
      const current = d.channel || name;
      const list = d.channels || [];
      channelSelect.replaceChildren(...(list.length ? list.map((channel) => el('option', {
        value: channel.name,
        text: channel.label ? channel.label + '（' + channel.name + '）' : channel.name,
        selected: channel.name === current,
      })) : [el('option', { value: current, text: current, selected: true })]));
      if (!channelSelect.value && channelSelect.options.length) channelSelect.selectedIndex = 0;
    }
    const mapSelect = $('aiMapName');
    if (mapSelect) {
      const previous = mapSelect.value;
      const list = d.maps || [];
      mapSelect.replaceChildren(...list.map((map) => el('option', {
        value: map.map_key,
        text: map.label || map.map_key,
      })));
      if (previous && list.some((map) => map.map_key === previous)) mapSelect.value = previous;
      else if (d.channel_map_key && list.some((map) => map.map_key === d.channel_map_key)) mapSelect.value = d.channel_map_key;
      else if (list.length) mapSelect.selectedIndex = 0;
    }
    renderSubscriptions();
  } catch (e) {
    if (isAbort(e)) return;
    host.toast('读取范围订阅失败：' + e.message, 'bad');
  }
}

function renderStatus(d) {
  const bridge = d.bridge || {};
  const queue = d.queue || {};
  const lastError = d.last_error || {};
  setText('aiBridgeStatus', bridge.online ? '在线' : '离线（看 logs/aibridge.log）');
  setText('aiLastError', lastError.error
    ? '最近一次失败：' + (lastError.agent || '') + ' · ' + (lastError.error || '')
    : '最近没有失败记录');
  const diag = [];
  diag.push('队列：' + (queue.pending || 0) + ' 待处理 / ' + (queue.processing || 0) + ' 处理中');
  diag.push('工具作业：' + (queue.tool_pending || 0) + ' 待处理 / ' + (queue.tool_processing || 0) + ' 处理中');
  if (lastError.error) diag.push('最近失败：' + (lastError.agent || '') + ' · ' + lastError.error);
  setText('aiFishingDiagnostics', diag.join('；'));
  const em = d.emergency_stop || {};
  const hint = $('aiEmergencyHint');
  if (hint) {
    hint.textContent = em.display
      ? (em.display + (em.fault ? '；状态待确认，请先刷新' : ''))
      : '状态待确认，请刷新';
  }
}

function aiTab(name) {
  document.querySelectorAll('#aiTabs .params-tab').forEach((btn) => {
    const on = btn.dataset.aiTab === name;
    btn.classList.toggle('on', on);
    btn.setAttribute('aria-selected', on ? 'true' : 'false');
  });
  document.querySelectorAll('#view-ai .ai-pane').forEach((pane) => {
    pane.classList.toggle('on', pane.id === 'aiPane-' + name);
  });
}

// ---------- 角色选择 ----------

function agentByKey(key) {
  return (S.agents || []).find((item) => item.key === key) || null;
}

function selectedAgent() {
  return agentByKey(S.selectedKey);
}

function ensureSelected() {
  const keys = (S.agents || []).map((item) => item.key);
  if (S.selectedKey && keys.indexOf(S.selectedKey) >= 0) return;
  const joined = (S.agents || []).find((item) => item.joined);
  S.selectedKey = joined ? joined.key : (keys[0] || '');
}

function syncAgentSelects() {
  const ids = ['aiSelectedAgent', 'aiFishingAgent', 'aiCapAgent'];
  const agents = S.agents || [];
  for (const id of ids) {
    const select = $(id);
    if (!select) continue;
    const previous = select.value;
    if (!agents.length) {
      select.replaceChildren(el('option', { value: '', text: '（还没有角色）' }));
      continue;
    }
    select.replaceChildren(...agents.map((agent) => el('option', {
      value: agent.key,
      text: (agent.display || agent.key) + (agent.joined ? ' · 已接入' : ' · 未接入'),
      selected: agent.key === S.selectedKey,
    })));
    if (previous && agents.some((agent) => agent.key === previous) && previous === S.selectedKey) {
      select.value = previous;
    }
    select.value = S.selectedKey || agents[0].key;
  }
}

function selectAgent(key, force) {
  if (!key || key === S.selectedKey) {
    syncAgentSelects();
    return;
  }
  // 性格草稿按角色分开：切换前必须显式选「保存 / 放弃 / 取消」，不能静默丢草稿。
  if (!force && personalityDraftDirty(S.selectedKey)) {
    syncAgentSelects();
    requestPersonalitySwitch(key);
    return;
  }
  if (S.capDirty && !force) {
    if (!window.confirm('当前角色有未保存的能力改动，切换会丢弃这些改动。继续？')) {
      syncAgentSelects();
      return;
    }
    S.capDirty = false;
  }
  S.selectedKey = key;
  syncAgentSelects();
  renderOverview(S.status);
  renderFishing(S.status);
  renderAgentList(S.agents);
  renderCapabilities();
  renderSubscriptions();
  renderPersonality();
}

function activityText(agent) {
  if (!agent) return '待确认';
  if (!agent.joined) return '未接入，暂无活动';
  const act = agent.actor || {};
  if (!act.activity) return '状态待确认（还没有运行记录）';
  const pair = ACTIVITY_LABELS[act.activity] || [act.activity, 'idle'];
  const bits = [pair[0]];
  if (act.loc_name) bits.push(act.loc_name);
  if (act.target_name) bits.push('目标 ' + act.target_name);
  const since = relativeMinutes(act.since_ts);
  if (since) bits.push(since);
  return bits.join(' · ');
}

function capabilityBrief(key) {
  const view = (S.capByKey || {})[key];
  if (!view) return '待确认';
  if (view.legacy) return '旧工具快照（只读 ' + ((view.read_tools || []).length) + ' 个）';
  if (!view.present) return '尚未保存能力（只保留基础私聊）';
  const label = CAPABILITY_AVAILABILITY[view.availability] || '待确认';
  return '修订 ' + view.revision + ' · ' + label;
}

// ---------- 总览 ----------

function overviewSteps(onboarding) {
  const steps = (onboarding && Array.isArray(onboarding.steps)) ? onboarding.steps.map((step) => ({ ...step })) : [];
  const agent = selectedAgent();
  if (!agent) return steps;
  const s3 = steps.find((step) => step.id === 'join');
  if (s3) {
    if (agent.contact_visible) {
      s3.state = 'done';
      s3.display = '玩家已在游戏里私聊过它，联系人可见';
    } else if (agent.joined) {
      s3.state = 'current';
      s3.display = '接入已保存（管理配置）；在游戏联系人里看到它并私聊后，才算玩家可见';
    } else {
      s3.state = 'blocked';
      s3.display = '选中角色还没接入游戏；先点下面的「接入游戏」';
    }
  }
  const s4 = steps.find((step) => step.id === 'hello');
  if (s4) {
    // 首句完成只认 GS 成功投递：后端把 last_delivered_ts 收敛成「status=done 且已投递」
    // 的历史证据。失败提示邮件也来自同一联系人（last_mail_ts 会变），不能单独算完成。
    const delivered = Number(agent.last_delivered_ts) > 0;
    if (delivered) {
      s4.state = 'done';
      s4.display = '已收到游戏内回复，游戏服已投递到个人消息';
      if (agent.last_reply_status === 'fallback') {
        s4.display += '；最近一次回复失败：' + (agent.last_reply_error || '原因待确认') + '，可让玩家再发一句重试';
      }
    } else if (agent.last_reply_status === 'fallback') {
      s4.state = 'current';
      s4.display = 'AI 回复失败：' + (agent.last_reply_error || '原因待确认') + '；可让玩家再发一句，或到「连接」页复测';
    } else if (Number(agent.last_dm_ts) > 0) {
      s4.state = 'current';
      s4.display = '私聊已入队，等待 AI 节点回复';
    } else if (agent.joined) {
      s4.state = 'current';
      s4.display = '在游戏私聊里给「' + (agent.display || agent.key) + '」发「你好」';
    } else {
      s4.state = 'blocked';
      s4.display = '先接入游戏，再在私聊里发首句';
    }
  }
  const s2 = steps.find((step) => step.id === 'create');
  if (s2 && (S.agents || []).length) s2.state = 'done';
  return steps;
}

function stepAction(step, agent) {
  if (step.id === 'join' && agent && !agent.joined) {
    return actionButton('btn sm', 'join-ai', '接入游戏', { key: agent.key, join: '1' });
  }
  const labels = {
    connection: step.state === 'done' ? '重新复测' : '去连接',
    create: '去创建',
    join: '查看联系人指引',
    hello: '复制首句',
  };
  if (step.id === 'hello') return actionButton('btn alt sm', 'copy-hello', labels.hello);
  if (step.id === 'connection') {
    return actionButton('btn sm', 'goto-tab', labels.connection, { tab: 'conn', focus: 'test' });
  }
  const tab = step.id === 'connection' ? 'conn' : (step.id === 'create' ? 'agents' : 'agents');
  return actionButton('btn alt sm', 'goto-tab', labels[step.id] || '去看看', { tab: tab });
}

function stepRow(step, index) {
  const stateText = { done: '完成', current: '当前', blocked: '被阻断', unknown: '待确认' }[step.state] || '待确认';
  const li = el('li', { class: 'ai-step ai-step-' + (step.state || 'unknown') });
  li.append(
    el('div', { class: 'ai-step-num', text: String(index + 1) }),
    el('div', {},
      el('div', { class: 'ai-step-title' },
        el('span', { text: step.title || '' }),
        el('span', { class: 'pill', text: stateText }),
        el('span', { class: 'ai-step-budget', text: (step.budget_s || 0) + ' 秒' })),
      el('div', { class: 'ai-step-detail', text: step.display || '待确认' })),
    el('div', { class: 'ai-step-actions' }, stepAction(step, selectedAgent())));
  return li;
}

function primaryIntent(onboarding) {
  const status = (onboarding && onboarding.status) || {};
  const priority = S.authStale ? 'login' : (status.priority || 'unknown');
  if (priority === 'emergency') return { label: '去解除急停', action: 'tab', tab: 'conn' };
  if (priority === 'login') return { label: '需登录（草稿已保留）', action: 'none' };
  if (priority === 'missing_key' || priority === 'connection_failed' || priority === 'unknown') {
    return { label: '去连接并复测', action: 'tab', tab: 'conn' };
  }
  if (priority === 'command_pending') return { label: '查看命令等待', action: 'tab', tab: 'personality' };
  if (!(S.agents || []).length) return { label: '创建角色', action: 'tab', tab: 'agents' };
  const agent = selectedAgent();
  if (!agent) return { label: '选择角色', action: 'tab', tab: 'agents' };
  if (!agent.joined) return { label: '接入游戏', action: 'join', key: agent.key };
  if (!agent.contact_visible) return { label: '查看联系人指引', action: 'tab', tab: 'agents' };
  if (!(Number(agent.last_dm_ts) > 0)) return { label: '复制「你好」', action: 'copy' };
  return { label: '查看首句证据', action: 'tab', tab: 'agents' };
}

function renderOverview(d) {
  const onboarding = (d && d.onboarding) || {};
  S.settingStates = (d.security || {}).keys || {};
  const status = onboarding.status || {};
  const priority = S.authStale ? 'login' : (status.priority || 'unknown');
  const display = S.authStale ? '需登录：登录已过期' : (status.display || '待确认');
  const next = S.authStale
    ? '登录后草稿仍在，可继续；未保存的输入不会丢。'
    : (status.next || '先刷新状态；读不到数据时按「待确认」处理。');
  setText('aiOverviewStatus', display);
  setText('aiOverviewNext', next);
  const pill = $('aiOverviewPriority');
  if (pill) {
    pill.textContent = {
      emergency: '急停', login: '需登录', missing_key: '缺 Key',
      connection_failed: '连接失败', command_pending: '命令等待',
      ready: '可试说', unknown: '待确认', incomplete: '未完成',
    }[priority] || '待确认';
    pill.className = 'pill ai-priority-' + (['emergency', 'login', 'missing_key', 'connection_failed', 'command_pending', 'ready', 'unknown'].indexOf(priority) >= 0 ? priority : 'unknown');
  }
  const intent = primaryIntent(onboarding);
  S.primaryIntent = intent;
  const primary = $('aiOverviewPrimary');
  if (primary) {
    primary.textContent = intent.label;
    primary.disabled = intent.action === 'none';
  }

  const steps = overviewSteps(onboarding);
  const list = $('aiSteps');
  if (list) {
    if (!steps.length) list.replaceChildren(el('li', { class: 'ai-step ai-step-unknown' }, el('div', { class: 'ai-step-num', text: '?' }), el('div', {}, el('div', { class: 'ai-step-title', text: '四步状态待确认' }), el('div', { class: 'ai-step-detail', text: '后端还没有给出四步状态；刷新后仍是空白请看服务日志。' }))));
    else list.replaceChildren(...steps.map(stepRow));
  }

  const agent = selectedAgent();
  setText('aiSelectedJoined', agent ? (agent.joined ? '已保存接入（管理配置）' : '未接入') : '待确认');
  setText('aiSelectedVisible', agent
    ? (agent.contact_visible ? '玩家可见（有私聊或邮件证据）' : (agent.joined ? '待确认（还没有游戏端证据）' : '未接入，谈不上可见'))
    : '待确认');
  setText('aiSelectedActivity', activityText(agent));
  setText('aiSelectedCapability', agent ? capabilityBrief(agent.key) : '待确认');
  setText('aiSubSelected', agent ? ('当前角色：' + (agent.display || agent.key)) : '未选择角色');
  setText('aiFishingSelected', agent ? ('当前角色：' + (agent.display || agent.key)) : '未选择角色');
  setText('aiCapSelected', agent ? ('当前角色：' + (agent.display || agent.key)) : '未选择角色');
  const guide = $('aiHelloGuide');
  if (guide) {
    if (agent) {
      guide.textContent = '在游戏里打开联系人，找到「' + (agent.display || agent.key) +
        '」，私聊发送「你好」；回复会进入个人消息。看不到联系人时，先确认已点「接入游戏」，再重登客户端。';
    } else {
      guide.textContent = (onboarding.first_sentence || {}).display || '先在「角色与订阅」创建一个角色，再接入游戏。';
    }
  }
}

function runOverviewNext() {
  const intent = S.primaryIntent || {};
  if (intent.action === 'tab') {
    aiTab(intent.tab);
    return;
  }
  if (intent.action === 'join') {
    joinAI(intent.key, true);
    return;
  }
  if (intent.action === 'copy') {
    copyHello();
  }
}

function copyHello() {
  const text = '你好';
  const done = () => host.toast('已复制「你好」；到游戏私聊里粘贴发送');
  if (navigator.clipboard && typeof navigator.clipboard.writeText === 'function') {
    navigator.clipboard.writeText(text).then(done).catch(() => fallbackCopy(text, done));
  } else {
    fallbackCopy(text, done);
  }
}

function fallbackCopy(text, done) {
  const input = el('textarea', { class: 'ai-copy-fallback' });
  input.value = text;
  document.body.appendChild(input);
  input.select();
  try {
    document.execCommand('copy');
    done();
  } catch (e) {
    host.toast('复制失败，请手动输入「你好」', 'bad');
  }
  input.remove();
}

// ---------- 连接 ----------

function renderConn(d) {
  applyDraftInput('aiBaseURL', d.base_url || '');
  applyDraftInput('aiModelID', d.model_id || '');
  applyDraftInput('aiKey', '');
  const em = d.emergency_stop || {};
  const control = $('aiSet_emergency_stop');
  if (control) {
    if (!S.emergency.pending) control.checked = !!em.stopped;
    if (!S.emergency.pending) S.emergency.formEpoch = em.epoch;
  }
  const box = $('aiSettings-conn');
  if (box) {
    const searchSpecs = specsFor('conn', 'search');
    box.replaceChildren(groupCard({
      title: '联网搜索',
      hint: '默认只查内部资料；打开联网搜索时才使用检索端点',
      module: 'conn',
      group: 'search',
      specs: searchSpecs,
      extra: null,
    }));
  }
}

const connectionInputs = ['aiBaseURL', 'aiModelID', 'aiKey'];
let connectionSavePending = false;

function connectionInputValues() {
  return connectionInputs.map((id) => (($(id) || {}).value || '').trim());
}

function savedConnectionText(d) {
  const target = destDisplay(d.destination);
  const suffix = (d.retest || {}).ok && (d.retest || {}).current ? '，已通过复测' : '，待复测';
  return '已保存 · ' + target + ' / 修订 ' + d.revision + suffix +
    '。Key 已加密保存在本机。复测会向该厂商发送固定测试文字，并使用 Key 鉴权。';
}

async function saveAIConfig() {
  if (connectionSavePending) return;
  connectionSavePending = true;
  ++aiRetestSeq;
  ++aiStatusSeq;
  const fields = connectionInputValues();
  const [base, model, key] = fields;
  const previousRevision = Number((S.status || {}).revision) || 0;
  setText('aiTestResult', '正在保存…');
  try {
    const d = await api('/api/ai/config', { method: 'POST', body: JSON.stringify({
      base_url: base, model_id: model, api_key: key, revision: previousRevision,
    }) });
    if (!d || !d.saved) return;
    const read = await loadAI();
    const current = S.status || {};
    if (!read || current.revision !== d.revision || (current.credential || {}).state !== 'configured') {
      setText('aiTestResult', '保存结果待确认，请刷新查看当前连接');
      return;
    }
    connectionInputValues().forEach((value, index) => {
      if (value !== fields[index]) return;
      clearDrafts([connectionInputs[index]]);
      if (index === 2) setInput('aiKey', '');
    });
    renderConn(current);
    setText('aiTestResult', savedConnectionText(current));
    host.toast(d.message || '已保存');
  } catch (e) {
    if (isAbort(e)) return;
    const read = await loadAI();
    const current = S.status || {};
    const updated = read && current.revision > previousRevision &&
      (current.credential || {}).state === 'configured';
    const notice = updated
      ? '已回读当前配置。' + savedConnectionText(current) + ' 请核对后再复测。'
      : (e instanceof TypeError ? '保存结果待确认，请刷新查看当前连接' : e.message);
    setText('aiTestResult', notice);
    host.toast(notice, updated ? undefined : 'bad');
  } finally {
    connectionSavePending = false;
  }
}

async function testAI() {
  if (connectionSavePending) return;
  const [base, model, key] = connectionInputValues();
  const current = S.status || {};
  if (key || (base && base !== current.base_url) || (model && model !== current.model_id)) {
    setText('aiTestResult', '当前输入尚未保存，请先点保存，再复测已保存连接');
    return;
  }
  const attempt = ++aiRetestSeq;
  const statusSeq = ++aiStatusSeq;
  setText('aiTestResult', '复测中…');
  try {
    const d = await api('/api/ai/test', { method: 'POST', body: '{}' });
    if (attempt !== aiRetestSeq || statusSeq !== aiStatusSeq) return;
    const read = loadAI();
    const confirmSeq = aiStatusSeq;
    if (!(await read) || attempt !== aiRetestSeq || confirmSeq !== aiStatusSeq) return;
    const em = S.status.emergency_stop || {};
    const retest = S.status.retest || {};
    if (d.ok && retest.current && retest.ok && d.attempt === retest.attempt &&
        d.epoch === em.epoch && d.revision === S.status.revision) {
      setText('aiTestResult', d.message);
      host.toast('复测通过');
    } else {
      const superseded = retest.current && d.attempt && d.attempt !== retest.attempt;
      setText('aiTestResult', (superseded ? retest.display : d.error) || retest.display || '待复测');
    }
  } catch (e) {
    if (isAbort(e) || attempt !== aiRetestSeq || statusSeq !== aiStatusSeq) return;
    setText('aiTestResult', '复测状态待确认，请刷新');
    loadAI();
  }
}

async function postAISettings(body, message) {
  const touchesEmergency = Object.prototype.hasOwnProperty.call(body || {}, 'emergency_stop');
  if (touchesEmergency) {
    if (S.emergency.saving) return;
    ++aiStatusSeq;
    ++aiRetestSeq;
    setText('aiTestResult', '');
    if (S.emergency.formEpoch) body.emergency_stop_epoch = S.emergency.formEpoch;
    S.emergency.saving = true;
    S.emergency.pending = false;
    setEmergencyCard('正在保存');
  }
  try {
    await api('/api/ai/settings', { method: 'POST', body: JSON.stringify(body) });
    if (touchesEmergency) S.emergency.saving = false;
    host.toast(message || '已保存');
    // 状态卡只认保存后的最新回读；回读失败显示「状态待确认」，不拿 toast 当证据。
    const ok = await loadAI();
    if (touchesEmergency && !ok) setEmergencyCard('状态待确认，请刷新');
  } catch (e) {
    if (isAbort(e)) return;
    if (touchesEmergency) {
      S.emergency.saving = false;
      S.emergency.pending = false;
      setEmergencyCard('状态待确认，请刷新');
    }
    host.toast(e.message, 'bad');
    if (touchesEmergency) loadAI();
  }
}

async function saveEmergency() {
  const control = $('aiSet_emergency_stop');
  if (!control) return;
  await postAISettings({ emergency_stop: control.checked }, control.checked ? '已保存：紧急停止已开启' : '已保存：紧急停止已解除');
}

// ---------- 通用参数卡片 ----------

function specsFor(module, group) {
  return (S.specs || []).filter((spec) => spec.module === module && !spec.hidden &&
    (!group || spec.group === group));
}

function settingValue(spec) {
  const value = S.settings[spec.key] !== undefined ? S.settings[spec.key] : spec.def;
  return draftOr('aiSet_' + spec.key, value);
}

function settingRow(spec) {
  const value = settingValue(spec);
  const row = el('div', { class: 'ai-toggle-row' });
  const title = el('b');
  title.append(document.createTextNode(spec.label));
  const badgeState = (S.settingStates || {})[spec.key];
  if (badgeState) title.append(document.createTextNode(' '), el('span', { class: 'pill', text: badgeState.display }));
  const field = fieldForSpec(spec, value);
  row.append(el('div', {}, title, el('small', { text: spec.hint || '' })), field);
  return row;
}

function fieldForSpec(spec, value) {
  const input = settingInput(spec, value);
  const error = el('span', { class: 'ai-field-error', id: 'aiErr_' + spec.key });
  return el('div', { class: 'ai-budget-field' }, input, error);
}

function settingInput(spec, value) {
  const id = 'aiSet_' + spec.key;
  if (spec.kind === 'bool') {
    return el('input', {
      type: 'checkbox', class: 'switch', id: id, 'data-key': spec.key, 'data-kind': 'bool',
      'data-draft': '1', checked: value === '1',
    });
  }
  if (spec.kind === 'string') {
    return el('input', {
      id: id, class: 'ai-setting-input-str', 'data-key': spec.key, 'data-kind': 'string',
      'data-draft': '1', value: value, placeholder: '可留空',
    });
  }
  return el('input', {
    id: id, class: 'ai-setting-input-num', type: 'number', 'data-key': spec.key, 'data-kind': spec.kind,
    'data-draft': '1', value: value, min: spec.min, max: spec.max, step: 'any',
  });
}

function groupCard(opts) {
  const card = el('div', { class: 'card ai-settings-card' });
  const head = el('div', { class: 'ai-group-head' },
    el('div', {}, el('h3', { text: opts.title }), el('small', { class: 'muted', text: opts.hint || '' })));
  if (opts.module) {
    head.append(actionButton('btn sm', 'save-settings', '保存' + (opts.title || ''), {
      module: opts.module, group: opts.group,
    }));
  }
  card.append(head);
  const items = opts.specs || [];
  const normal = items.filter((spec) => !spec.advanced);
  const advanced = items.filter((spec) => spec.advanced);
  card.append(settingsFields(normal));
  if (advanced.length) {
    card.append(el('details', { class: 'ai-adv' },
      el('summary', { text: '高级参数（' + advanced.length + ' 项，一般不用改）' }),
      settingsFields(advanced)));
  }
  if (opts.extra) card.append(opts.extra);
  return card;
}

function settingsFields(items) {
  const list = el('div', { class: 'ai-toggle-list' });
  for (const spec of items) list.append(settingRow(spec));
  return list;
}

function clearFieldError(key) {
  const error = $('aiErr_' + key);
  if (error) error.textContent = '';
  const input = $('aiSet_' + key);
  if (input) input.removeAttribute('aria-invalid');
}

function setFieldError(key, message) {
  const error = $('aiErr_' + key);
  if (error) error.textContent = message;
  const input = $('aiSet_' + key);
  if (input) input.setAttribute('aria-invalid', 'true');
}

function validateSpec(spec) {
  if (spec.kind !== 'int' && spec.kind !== 'float') return '';
  const input = $('aiSet_' + spec.key);
  if (!input) return '';
  const raw = String(input.value || '').trim();
  if (raw === '') return '请填写数值';
  const n = Number(raw);
  if (!Number.isFinite(n)) return '请填写数字';
  if (spec.kind === 'int' && !Number.isInteger(n)) return '必须是整数';
  if (n < spec.min || n > spec.max) return '需在 ' + spec.min + '–' + spec.max + ' 之间';
  return '';
}

function collectSettingBody(specs) {
  const body = {};
  const errors = [];
  for (const spec of specs) {
    const input = $('aiSet_' + spec.key);
    if (!input) continue;
    clearFieldError(spec.key);
    if (spec.kind === 'bool') {
      body[spec.key] = input.checked;
      continue;
    }
    if (spec.kind === 'string') {
      body[spec.key] = String(input.value || '').trim();
      continue;
    }
    const message = validateSpec(spec);
    if (message) {
      errors.push({ key: spec.key, message: message });
      setFieldError(spec.key, message);
      continue;
    }
    body[spec.key] = Number(input.value);
  }
  return { body: body, errors: errors };
}

function collectGroup(module, group) {
  const specs = specsFor(module, group);
  const result = collectSettingBody(specs);
  if (module === 'budget') {
    const prefix = group === 'channel' ? 'ai_profile_channel' : (group === 'dm' ? 'ai_profile_dm' : (group === 'roundtable' ? 'ai_profile_roundtable' : ''));
    if (prefix) {
      const softKey = prefix + '_reply_soft_chars';
      const hardKey = prefix + '_reply_chars';
      const soft = result.body[softKey];
      const hard = result.body[hardKey];
      if (typeof soft === 'number' && typeof hard === 'number' && soft > hard) {
        const message = '软目标不能大于硬上限（' + soft + ' > ' + hard + '）';
        result.errors.push({ key: softKey, message: message });
        setFieldError(softKey, message);
      }
    }
  }
  return result;
}

function focusFirstError(errors) {
  if (!errors.length) return;
  const input = $('aiSet_' + errors[0].key);
  if (input) {
    input.focus();
    if (typeof input.scrollIntoView === 'function') input.scrollIntoView({ block: 'center' });
  }
}

async function saveSettingsGroup(module, group) {
  const collected = collectGroup(module, group);
  if (collected.errors.length) {
    focusFirstError(collected.errors);
    host.toast('本组有 ' + collected.errors.length + ' 项不合法，未提交', 'bad');
    return;
  }
  if (!Object.keys(collected.body).length) {
    host.toast('没有可保存的项', 'bad');
    return;
  }
  try {
    await api('/api/ai/settings', { method: 'POST', body: JSON.stringify(collected.body) });
    clearDrafts(Object.keys(collected.body).map((key) => 'aiSet_' + key));
    host.toast('已保存；页面按回读值显示');
    await loadAI();
  } catch (e) {
    if (isAbort(e)) return;
    host.toast(e.message, 'bad');
  }
}

// ---------- 说话与上下文 ----------

function budgetRows(group) {
  const prefix = 'ai_profile_' + (group === 'channel' ? 'channel' : (group === 'dm' ? 'dm' : 'roundtable'));
  const first = group === 'roundtable'
    ? [prefix + '_context_tokens', prefix + '_context_messages']
    : [prefix + '_context_tokens', prefix + '_history_messages', prefix + '_history_chars'];
  return [first, [prefix + '_reply_soft_chars', prefix + '_reply_chars']];
}

function budgetCard(group) {
  const specs = specsFor('budget', group);
  const byKey = {};
  for (const spec of specs) byKey[spec.key] = spec;
  const card = el('div', { class: 'card ai-budget-card' });
  const title = BUDGET_GROUP_TITLE[group] || group;
  card.append(el('div', { class: 'ai-group-head' },
    el('div', {}, el('h3', { text: title + '预算' }), el('small', { class: 'muted', text: group === 'roundtable' ? '圆桌只按历史条数控制，没有字数额度' : '第一行上下文与历史，第二行回复软目标与硬上限' })),
    actionButton('btn sm', 'save-settings', '保存' + title, { module: 'budget', group: group })));
  const rows = el('div', { class: 'ai-budget-rows' });
  for (const keys of budgetRows(group)) {
    const row = el('div', { class: 'ai-budget-row' });
    for (const key of keys) {
      const spec = byKey[key];
      if (!spec) continue;
      row.append(fieldForSpec(spec, settingValue(spec)));
    }
    rows.append(row);
  }
  card.append(rows);
  card.append(el('div', { class: 'ai-budget-note', text: '软目标是提示词里的期望长度，硬上限超过会被截断；软目标必须 ≤ 硬上限。保存前逐项校验，不合法的一组不会提交。' }));
  return card;
}

function renderBudget() {
  const box = $('aiSettings-budget');
  if (!box) return;
  const groups = ['channel', 'dm', 'roundtable'];
  const cards = groups.map(budgetCard);
  const profileSpecs = specsFor('budget', 'profile');
  const profileCard = groupCard({
    title: '资料预算',
    hint: '内部资料进入提示词的上限，和对话历史分开',
    module: 'budget',
    group: 'profile',
    specs: profileSpecs,
  });
  profileCard.classList.add('ai-budget-card');
  cards.push(profileCard);
  box.replaceChildren(...cards);
}

// ---------- AI 虚拟钓鱼 ----------

function fishingStatsText(agent) {
  if (!agent) return '待确认';
  const total = Number(agent.catch_total);
  const today = Number(agent.catch_today);
  if (!Number.isFinite(total) || !Number.isFinite(today)) return '待确认';
  const totalKg = (Number(agent.catch_total_g || 0) / 1000).toFixed(2);
  const todayKg = (Number(agent.catch_today_g || 0) / 1000).toFixed(2);
  return '历史累计 ' + total + ' 条 · 总重 ' + totalKg + ' kg；今日（0 点起）' + today + ' 条 · ' + todayKg + ' kg';
}

function fishingCard(opts) {
  const card = el('div', { class: 'card ai-settings-card' });
  card.append(el('div', { class: 'ai-group-head' },
    el('div', {}, el('h3', { text: opts.title }), el('small', { class: 'muted', text: opts.hint || '' })),
    actionButton('btn sm', 'save-settings', '保存' + opts.title, { module: 'fishing', group: opts.group })));
  const specs = specsFor('fishing', opts.group);
  const rows = el('div', { class: 'ai-budget-rows' });
  for (const keys of (opts.rows || [])) {
    const row = el('div', { class: 'ai-budget-row' });
    for (const key of keys) {
      const spec = specs.find((item) => item.key === key);
      if (!spec) continue;
      row.append(fieldForSpec(spec, settingValue(spec)));
    }
    rows.append(row);
  }
  card.append(rows);
  if (opts.note) card.append(el('div', { class: 'ai-budget-note', text: opts.note }));
  if (opts.extra) card.append(opts.extra);
  return card;
}

function renderFishing(d) {
  const agent = selectedAgent();
  setText('aiFishingSelected', agent ? ('当前角色：' + (agent.display || agent.key)) : '未选择角色');
  setText('aiFishingStats', fishingStatsText(agent));
  setText('aiFishingActor', activityText(agent) + (S.actorMaster ? '' : '；服务端运行开关：已关闭（只读）'));
  if (d) renderStatus(d);
  const box = $('aiSettings-fishing');
  if (!box) return;
  const sim = fishingCard({
    title: '模拟与状态',
    hint: '关闭总开关不会删除已有渔获；活动状态是回读值，状态未知时不生成「正在钓鱼」',
    group: 'sim',
    rows: [['ai_fishing_enabled', 'ai_fishing_hourly_limit'], ['ai_fishing_base_interval_ms', 'ai_fishing_max_interval_ms']],
    note: '节拍与配额沿用原游戏服逻辑；报鱼发言计入配额，静默档不占配额。',
  });
  const rare = fishingCard({
    title: '稀有档与普通余量',
    hint: '构成比例，不是每次发言概率；四项权重与两条分界共同决定抽到什么档',
    group: 'rare',
    rows: [['ai_fishing_star_weight', 'ai_fishing_super_weight', 'ai_fishing_miss_weight'], ['ai_fishing_near_weight', 'ai_fishing_miss_ratio', 'ai_fishing_near_ratio']],
  });
  const stats = el('div', { class: 'ai-note' },
    el('b', { text: '渔获口径：' }), fishingStatsText(agent),
    '。历史渔获是全部记录；今日渔获从本地 0 点起算。常驻简报只按历史累计说「曾有渔获」，不推断今天；只有玩家明确问渔获时才读今日与最近记录。');
  const inject = fishingCard({
    title: '渔获与战绩注入',
    hint: '战绩摘要（星/蓝）与渔获简报在这里配置；关闭注入不删除记录',
    group: 'inject',
    rows: [['ai_memory_inject', 'ai_memory_recent', 'ai_memory_best', 'ai_memory_max_rows'], ['ai_catch_brief_inject']],
    note: '「知道自己上过鱼」只注入「曾有渔获」这一句，不报数字、不推断今天；「清除战绩摘要」只清 ai_memory，渔获流水仍保留。',
    extra: stats,
  });
  box.replaceChildren(sim, rare, inject);
}

async function clearAIMemory() {
  const key = S.selectedKey;
  if (!key) {
    host.toast('先选择一个角色', 'bad');
    return;
  }
  if (!window.confirm('清除此角色的星蓝战绩摘要，渔获流水仍保留。\n\n继续？')) return;
  try {
    const d = await api('/api/ai/memory/clear', { method: 'POST', body: JSON.stringify({ key: key }) });
    host.toast('已受理，等待游戏服应用…');
    await pollAICommand(d.command_id, '战绩摘要已清除（渔获流水保留）');
    loadAI();
  } catch (e) {
    if (isAbort(e)) return;
    host.toast(e.message, 'bad');
  }
}

// ---------- 角色与订阅 ----------

function renderAgentList(agents) {
  const box = $('aiAgentList');
  if (!box) return;
  S.agents = agents || [];
  const all = S.agents;
  syncAgentSelects();
  if (!all.length) {
    box.replaceChildren(el('div', { class: 'empty', text: '还没有 Agent。先填 Key、展示名与提示词再新增；UID 由服务端分配。' }));
    setText('aiAgentCount', '共 0 个');
    return;
  }
  const query = (($('aiAgentSearch') || {}).value || '').trim().toLowerCase();
  const shown = query
    ? all.filter((agent) => [agent.display, agent.key, agent.role]
      .some((value) => String(value || '').toLowerCase().includes(query)))
    : all;
  setText('aiAgentCount', query ? ('匹配 ' + shown.length + ' / ' + all.length) : ('共 ' + all.length + ' 个'));
  if (!shown.length) {
    box.replaceChildren(el('div', { class: 'empty', text: '没有匹配的 Agent，换个关键词试试。' }));
    return;
  }
  box.replaceChildren(...shown.map(agentCard));
}

function agentCapabilityText(agent) {
  const view = (S.capByKey || {})[agent.key];
  if (!view) return '待确认';
  if (view.legacy) return '旧工具快照（只读 ' + ((view.read_tools || []).length) + ' 个）';
  if (!view.present) return '尚未保存（基础私聊）';
  return '修订 ' + view.revision + ' · ' + (CAPABILITY_AVAILABILITY[view.availability] || '待确认');
}

function agentCard(agent) {
  const item = el('div', { class: 'ai-agent-item' + (agent.key === S.selectedKey ? ' ai-agent-selected' : '') });
  const head = el('div', { class: 'ai-agent-head' });
  const pills = el('div', { class: 'ai-agent-name' });
  pills.append(el('b', { text: agent.display || agent.key }));
  pills.append(el('span', { class: 'pill ' + (agent.joined ? 'ok' : ''), text: agent.joined ? '已接入' : '未接入' }));
  pills.append(el('span', { class: 'pill', text: agent.contact_visible ? '玩家可见' : '玩家可见待确认' }));
  if (agent.key === S.selectedKey) pills.append(el('span', { class: 'pill', text: '当前角色' }));
  const identity = el('div', { class: 'ai-agent-id' },
    el('div', { class: 'ai-agent-avatar', text: (agent.display || '?').slice(0, 1) }),
    el('div', {},
      pills,
      el('div', { class: 'ai-agent-meta' },
        el('span', { class: 'tag mono', text: agent.key }),
        el('span', { class: 'tag', text: 'UID ' + (agent.contact_uid || '—') }),
        el('span', { class: 'tag', text: agent.role || '未设角色' }),
        el('span', { class: 'tag', text: '思考 ' + (agent.thinking_effort || 'off') }))));
  const actions = el('div', { class: 'ai-agent-actions' });
  if (agent.key !== S.selectedKey) {
    actions.append(actionButton('btn alt sm', 'select-agent', '设为主角', { key: agent.key }));
  }
  actions.append(
    actionButton('btn sm', 'edit-ai', '编辑', { key: agent.key }),
    actionButton('btn alt sm', 'join-ai', agent.joined ? '移出游戏' : '接入游戏', { key: agent.key, join: agent.joined ? '0' : '1' }),
    actionButton('btn bad sm', 'delete-ai', '删除', { key: agent.key }));
  head.append(identity, actions);
  const facts = el('div', { class: 'ai-agent-facts' },
    el('div', { class: 'ai-agent-fact' }, el('span', { text: '接入（管理配置）' }), el('b', { text: agent.joined ? '已保存接入' : '未接入' })),
    el('div', { class: 'ai-agent-fact' }, el('span', { text: '玩家可见' }), el('b', { text: agent.contact_visible ? '有游戏端证据' : '待确认' })),
    el('div', { class: 'ai-agent-fact' }, el('span', { text: '能力' }), el('b', { text: agentCapabilityText(agent) })),
    el('div', { class: 'ai-agent-fact' }, el('span', { text: '渔获' }), el('b', { text: '历史 ' + (agent.catch_total || 0) + ' 条 · 今日 ' + (agent.catch_today || 0) + ' 条' })));
  const prompt = el('div', { class: 'ai-agent-prompt', text: agent.prompt ? ('提示词：' + agent.prompt) : '提示词：未填写' });
  item.append(head, facts, prompt);
  return item;
}

async function createAI() {
  const effort = $('aiNewEffort') ? $('aiNewEffort').value : 'off';
  const body = {
    key: ($('aiNewKey') || {}).value ? $('aiNewKey').value.trim() : '',
    display: ($('aiNewDisplay') || {}).value ? $('aiNewDisplay').value.trim() : '',
    role: ($('aiNewRole') || {}).value ? $('aiNewRole').value.trim() : '',
    prompt: ($('aiNewPrompt') || {}).value ? $('aiNewPrompt').value.trim() : '',
    thinking_effort: effort,
    thinking_enabled: effort !== 'off',
  };
  try {
    const d = await api('/api/ai/agent/create', { method: 'POST', body: JSON.stringify(body) });
    if (!d) return;
    host.toast(d.revived
      ? ('已恢复 ' + d.key + '（UID ' + d.contact_uid + '，原联系人与历史保留；需重新接入）')
      : ('已创建 ' + d.key + '，UID ' + d.contact_uid + '（未接入）'));
    clearDrafts(['aiNewKey', 'aiNewDisplay', 'aiNewRole', 'aiNewPrompt', 'aiNewEffort']);
    ['aiNewKey', 'aiNewDisplay', 'aiNewRole', 'aiNewPrompt'].forEach((id) => setInput(id, ''));
    S.selectedKey = d.key;
    S.capDirty = false;
    await loadAI();
    renderOverview(S.status);
  } catch (e) {
    if (isAbort(e)) return;
    host.toast(e.message, 'bad');
  }
}

function editAI(key) {
  const agent = agentByKey(key);
  if (!agent) {
    host.toast('未找到 ' + key, 'bad');
    return;
  }
  const form = el('div', {},
    el('div', { class: 'ai-edit-readonly', text: 'Key ' + agent.key + ' · 联系人 UID ' + (agent.contact_uid || '—') + '（自动分配，不可改）' }),
    el('div', { class: 'form-grid' },
      el('div', { class: 'field' }, el('label', { text: '展示名' }), el('input', { id: 'aiEditDisplay', 'data-draft': '1' })),
      el('div', { class: 'field' }, el('label', { text: '角色' }), el('input', { id: 'aiEditRole' })),
      el('div', { class: 'field span2' }, el('label', { text: '系统提示词' }), el('textarea', { id: 'aiEditPrompt', rows: '8' })),
      el('div', { class: 'field' }, el('label', { text: '思考' }),
        el('div', { class: 'ai-switch-label' },
          el('input', { type: 'checkbox', id: 'aiEditThinking', class: 'switch' }), ' 启用思考')),
      el('div', { class: 'field' }, el('label', { text: '思考档' }),
        el('select', { id: 'aiEditEffort' },
          ...['off', 'low', 'medium', 'high', 'max'].map((level) => el('option', { value: level, text: level }))))),
    el('div', { class: 'ai-edit-status' },
      el('div', { id: 'aiEditStatusBase', text: '基础资料：未保存' }),
      el('div', { id: 'aiEditStatusThinking', text: '思考档：未保存' }),
      el('div', { id: 'aiEditStatusError' })),
    el('div', { class: 'ai-overview-actions' },
      actionButton('btn', 'save-ai-edit', '保存', { key: agent.key }),
      actionButton('btn alt', 'save-ai-edit-base', '只保存基础资料', { key: agent.key }),
      actionButton('btn alt', 'save-ai-edit-thinking', '只保存思考档', { key: agent.key })));
  openAIModal('编辑 Agent · ' + agent.display, '编辑省略工具字段；已保存的能力集合不会被旧表单覆盖', form);
  clearDrafts(['aiEditDisplay']);
  applyDraftInput('aiEditDisplay', agent.display || '');
  setInput('aiEditRole', agent.role || '');
  setInput('aiEditPrompt', agent.prompt || '');
  const thinking = $('aiEditThinking');
  if (thinking) thinking.checked = !!agent.thinking_enabled;
  const effort = $('aiEditEffort');
  if (effort) effort.value = agent.thinking_effort || 'off';
  const base = $('aiEditStatusBase');
  if (base) base.textContent = '基础资料：未保存';
  const think = $('aiEditStatusThinking');
  if (think) think.textContent = '思考档：未保存';
  if (thinking) thinking.focus();
}

async function saveAIEdit(key, part) {
  const display = (($('aiEditDisplay') || {}).value || '').trim();
  const role = (($('aiEditRole') || {}).value || '').trim();
  const prompt = ($('aiEditPrompt') || {}).value || '';
  const thinkingEnabled = !!($('aiEditThinking') ? $('aiEditThinking').checked : false);
  const effort = ($('aiEditEffort') || {}).value || 'off';
  const statusBase = $('aiEditStatusBase');
  const statusThinking = $('aiEditStatusThinking');
  const statusError = $('aiEditStatusError');
  if (statusError) statusError.textContent = '';
  let baseSaved = null;
  let thinkingSaved = null;
  if (part === 'all' || part === 'base') {
    if (!display) {
      if (statusBase) statusBase.textContent = '基础资料：未保存（展示名不能为空）';
      host.toast('展示名不能为空', 'bad');
      return;
    }
    try {
      await api('/api/ai/agent/update', {
        method: 'POST',
        body: JSON.stringify({ key: key, display: display, role: role, prompt: prompt }),
      });
      baseSaved = true;
      if (statusBase) statusBase.textContent = '基础资料：已保存';
    } catch (e) {
      if (isAbort(e)) return;
      baseSaved = false;
      if (statusBase) statusBase.textContent = '基础资料：未保存（' + e.message + '）';
      if (statusError) statusError.textContent = '基础资料未保存；可以只重试基础资料。';
    }
  }
  if (part === 'all' || part === 'thinking') {
    try {
      await api('/api/ai/agent', {
        method: 'POST',
        body: JSON.stringify({ key: key, thinking_enabled: thinkingEnabled, thinking_effort: effort }),
      });
      thinkingSaved = true;
      if (statusThinking) statusThinking.textContent = '思考档：已保存';
    } catch (e) {
      if (isAbort(e)) return;
      thinkingSaved = false;
      if (statusThinking) statusThinking.textContent = '思考档：未保存（' + e.message + '）';
      if (statusError) statusError.textContent = '思考档未保存；可以只重试思考档。';
    }
  }
  if (baseSaved === false || thinkingSaved === false) {
    host.toast('有未保存项，按提示单独重试', 'bad');
    if (baseSaved !== false) clearDrafts(['aiEditDisplay']);
    return;
  }
  clearDrafts(['aiEditDisplay']);
  host.toast(part === 'base' ? '基础资料已保存' : (part === 'thinking' ? '思考档已保存' : '已保存'));
  closeAIModal();
  loadAI();
}

async function joinAI(key, joined) {
  if (!joined && !window.confirm('把 ' + key + ' 移出游戏？\n\n频道接话会停止，游戏联系人也不再出现；历史私聊与渔获记录保留，重新接入可恢复。')) return;
  try {
    const d = await api('/api/ai/agent/join', { method: 'POST', body: JSON.stringify({ key: key, joined: joined }) });
    if (!d) return;
    host.toast(joined
      ? ('已保存接入，UID ' + d.contact_uid + '；玩家可见待游戏端确认')
      : '已移出游戏（历史保留）');
    await loadAI();
  } catch (e) {
    if (isAbort(e)) return;
    host.toast(e.message, 'bad');
  }
}

async function deleteAI(key) {
  if (!window.confirm('删除 Agent「' + key + '」？\n\n聊天对象和聊天记录会保留，之后用同一个 Key 新建可以恢复原身份。')) return;
  try {
    const d = await api('/api/ai/agent/delete', { method: 'POST', body: JSON.stringify({ key: key }) });
    host.toast('已受理，等待游戏服应用…');
    await pollAICommand(d.command_id, '已删除（历史保留）');
    await loadAI();
  } catch (e) {
    if (isAbort(e)) return;
    host.toast(e.message, 'bad');
  }
}

// 运行态动作只提交意图：后台返回 202，真正生效要等游戏服在同一事务里应用。
// 轮询到终态再回显，30 秒后停止自动轮询。
async function pollAICommand(id, label) {
  if (!id) {
    host.toast(label);
    return null;
  }
  const epoch = S.epoch;
  const deadline = Date.now() + 30000;
  while (Date.now() < deadline) {
    if (epoch !== S.epoch) return null;
    let d = {};
    try {
      d = await api('/api/ai/commands/' + encodeURIComponent(id));
    } catch (e) {
      if (isAbort(e)) return;
      host.toast(label + '：查询失败 ' + e.message, 'bad');
      return null;
    }
    if (epoch !== S.epoch) return null;
    if (d.status && d.status !== 'pending') {
      if (d.status === 'applied') host.toast(label);
      else if (d.status === 'rejected') host.toast(label + '：被拒绝（' + (d.reason || '规则不允许') + '）', 'bad');
      else host.toast(label + '：已过期，请刷新后重试', 'bad');
      return d;
    }
    await sleep(1000);
  }
  host.toast(label + '：仍待游戏服应用，可刷新', 'bad');
  return null;
}

// ---------- 范围订阅 ----------

function renderSubscriptions() {
  const agent = selectedAgent();
  const mapsBox = $('aiMapList');
  const channelsBox = $('aiChannelList');
  if (!agent) {
    if (mapsBox) mapsBox.textContent = '先在「角色与订阅」创建并选择一个角色。';
    if (channelsBox) channelsBox.textContent = '未选择角色。';
    return;
  }
  const data = S.channels;
  if (!data) {
    if (mapsBox) mapsBox.textContent = '读取中…';
    if (channelsBox) channelsBox.textContent = '读取中…';
    return;
  }
  const maps = (data.maps || []).filter((map) => (map.agent_keys || []).includes(agent.key));
  const channels = (data.channels || []).filter((channel) => (channel.agent_keys || []).includes(agent.key));
  if (mapsBox) {
    mapsBox.textContent = maps.length
      ? ('普通范围：' + maps.map((map) => map.map_key === '*' ? '全局（任何频道）' : (map.label || map.map_key) + '（整张图）').join('、'))
      : '普通范围：未订阅任何地图 / 全局。';
  }
  if (channelsBox) {
    channelsBox.textContent = channels.length
      ? ('精确频道：' + channels.map((channel) => channel.name).join('、'))
      : '精确频道：未订阅。';
  }
  const hint = $('aiMapList');
  if (hint && maps.length === 0 && channels.length === 0) {
    hint.textContent += ' 取消一个范围只影响该范围，不代表已退出全部；移出游戏才会停止所有频道接话。';
  }
}

async function aiSubscribeMap(enable) {
  const agent = selectedAgent();
  const mapKey = $('aiMapName') ? $('aiMapName').value : '';
  if (!agent) {
    host.toast('先选择一个角色', 'bad');
    return;
  }
  if (!mapKey) {
    host.toast('请先选择范围', 'bad');
    return;
  }
  const isGlobal = mapKey === '*';
  try {
    await api('/api/ai/channel', {
      method: 'POST',
      body: JSON.stringify({ map_key: mapKey, agent: agent.key, enabled: enable }),
    });
    host.toast(enable
      ? (isGlobal ? '已加入全局范围' : '已拉进该地图范围')
      : (isGlobal ? '已移出全局范围（其他范围不变）' : '已移出该地图范围（其他范围不变）'));
    await loadChannels();
  } catch (e) {
    if (isAbort(e)) return;
    host.toast(e.message, 'bad');
  }
}

async function aiSubscribe(enable) {
  const agent = selectedAgent();
  if (!agent) {
    host.toast('先选择一个角色', 'bad');
    return;
  }
  const body = {
    channel: ($('aiChannelName') && $('aiChannelName').value) || S.channelName || 'nav1_03',
    agent: agent.key,
    enabled: enable,
  };
  try {
    const d = await api('/api/ai/channel', { method: 'POST', body: JSON.stringify(body) });
    host.toast('已受理，等待游戏服应用…');
    await pollAICommand(d.command_id, enable ? '已拉进频道' : '已移出频道（其他范围不变）');
    await loadChannels();
  } catch (e) {
    if (isAbort(e)) return;
    host.toast(e.message, 'bad');
  }
}

// ---------- 性格与交互（能力区） ----------

function capView(key) {
  return (S.capByKey || {})[key] || null;
}

function capToolNames(cap) {
  return (cap.read_tools || []).concat(cap.write_tools || []);
}

function renderCapabilities() {
  const agent = selectedAgent();
  const list = $('aiCapList');
  const result = $('aiCapResult');
  const commands = $('aiCapCommands');
  if (!agent) {
    if (list) list.textContent = '先在「角色与订阅」创建并选择一个角色。';
    if (result) result.textContent = '未选择角色';
    if (commands) commands.replaceChildren();
    setText('aiCapState', '待确认');
    return;
  }
  const view = capView(agent.key);
  const catalog = (S.capabilities && S.capabilities.catalog) || [];
  const checked = view && Array.isArray(view.capabilities) ? view.capabilities : [];
  if (result) {
    if (!view) result.textContent = '能力状态待确认（刷新后仍为空请检查服务日志）';
    else if (view.legacy) result.textContent = '旧工具快照：当前只读工具 ' + ((view.read_tools || []).length) + ' 个。改用独立授权时请取消旧攻略授权。';
    else if (!view.present) result.textContent = '尚未保存能力：按缺配置处理，只保留基础私聊。';
  }
  if (list) {
    const nodes = catalog.filter((cap) => !cap.legacy || checked.includes(cap.id)).map((cap) => {
      const box = el('input', { type: 'checkbox', value: cap.id, 'data-cap': cap.id });
      box.checked = checked.indexOf(cap.id) >= 0;
      box.addEventListener('change', () => { S.capDirty = true; renderCapWarnings(); });
      const title = el('div', { class: 'ai-cap-title' }, el('b', { text: cap.label || cap.id }));
      if (cap.event) title.append(el('span', { class: 'pill', text: '事件' }));
      if (cap.read_tools && cap.read_tools.length) title.append(el('span', { class: 'pill', text: '只读工具 ' + cap.read_tools.length }));
      if (cap.write_tools && cap.write_tools.length) title.append(el('span', { class: 'pill', text: '写工具 ' + cap.write_tools.length }));
      const desc = el('div', { class: 'ai-cap-desc', text: cap.desc || '' });
      const warn = el('div', { class: 'ai-cap-warn', 'data-cap-warn': cap.id });
      if (cap.requires_roundtable) warn.textContent = '依赖：需要同时具备「参加多人对话」；未满足时保存会被拒绝。';
      if (cap.id === 'broadcast_catch') warn.textContent += ' 播报只在自己已提交渔获后发生。';
      return el('label', { class: 'ai-cap-item' }, el('div', {}, box), el('div', {}, title, desc, warn));
    });
    const competition = el('div', { class: 'ai-cap-item ai-cap-disabled' },
      el('div', {}, el('input', { type: 'checkbox', disabled: true })),
      el('div', {}, el('div', { class: 'ai-cap-title' }, el('b', { text: '参加比赛' }), el('span', { class: 'pill', text: '未接入' })),
        el('div', { class: 'ai-cap-desc', text: '比赛能力尚未接入：事件运行消费归 T-108，参赛按已批范围处理；当前不能勾选，也不提供占位 ID。' })));
    list.replaceChildren(...nodes, competition);
  }
  const stateLine = !view
    ? '待确认'
    : (view.present
      ? ('已保存修订 ' + view.revision + ' · ' + (CAPABILITY_AVAILABILITY[view.availability] || '待确认') + (view.runtime && view.runtime.command_id ? (' · 命令 #' + view.runtime.command_id) : ''))
      : '缺配置（基础私聊）');
  setText('aiCapState', stateLine);
  setText('aiSelectedCapability', capabilityBrief(agent.key));
  if (commands) {
    const items = [];
    if (view && view.runtime) {
      items.push(el('div', { class: 'ai-cap-command' },
        '运行态：期望 ' + (view.runtime.desired ? '开' : '关') + ' · 实际 ' + (view.runtime.actual ? '开' : '关') +
        ' · ' + (CAPABILITY_AVAILABILITY[view.runtime.availability] || '待确认') +
        (view.runtime.reason ? '（' + view.runtime.reason + '）' : '')));
    }
    for (const cmd of ((view && view.commands) || []).slice(-6)) {
      items.push(el('div', { class: 'ai-cap-command' },
        '命令 #' + cmd.id + '：' + commandStatusText(cmd.status) +
        ' · 修订 ' + cmd.revision + ' · ' + (cmd.enabled ? '启用' : '停用') +
        (cmd.reason ? '（' + cmd.reason + '）' : '')));
    }
    commands.replaceChildren(...items);
  }
  renderCapWarnings();
}

function commandStatusText(status) {
  return { pending: '待应用', applied: '已生效', rejected: '被拒绝', expired: '已过期' }[status] || (status || '待确认');
}

function renderCapWarnings() {
  const roundtable = document.querySelector('[data-cap="join_roundtable"]');
  const broadcast = document.querySelector('[data-cap="broadcast_catch"]');
  const warn = document.querySelector('[data-cap-warn="broadcast_catch"]');
  if (!broadcast || !warn) return;
  const conflict = broadcast.checked && roundtable && !roundtable.checked;
  if (conflict) warn.textContent = '冲突：播报自己渔获需要先勾选「参加多人对话」；当前组合不会被保存。';
  else warn.textContent = '依赖：需要同时具备「参加多人对话」；播报只在自己已提交渔获后发生。';
}

function readCapabilityChecks() {
  const boxes = Array.from(document.querySelectorAll('#aiCapList input[type=checkbox][data-cap]'));
  return boxes.filter((box) => box.checked).map((box) => box.getAttribute('data-cap'));
}

async function saveCapabilities(retry) {
  const agent = selectedAgent();
  if (!agent) {
    host.toast('先选择一个角色', 'bad');
    return;
  }
  const view = capView(agent.key);
  const caps = retry
    ? ((view && view.capabilities) || [])
    : readCapabilityChecks();
  if (!retry) {
    if (caps.indexOf('broadcast_catch') >= 0 && caps.indexOf('join_roundtable') < 0) {
      renderCapWarnings();
      const warn = document.querySelector('[data-cap-warn="broadcast_catch"]');
      if (warn) warn.focus();
      host.toast('「播报自己渔获」需要同时勾选「参加多人对话」', 'bad');
      return;
    }
  }
  const body = {
    agent_key: agent.key,
    base_revision: view ? view.revision : 0,
    capabilities: caps,
  };
  try {
    const d = await api('/api/ai/capabilities', { method: 'POST', body: JSON.stringify(body) });
    S.capDirty = false;
    const effective = CAPABILITY_AVAILABILITY[d.availability] || '待确认';
    const changed = d.changed ? '有变更' : '无变更';
    host.toast('已保存修订 ' + d.revision + '（' + changed + '）；' + effective);
    await loadCapabilities();
    setText('aiCapResult',
      '已保存修订 ' + d.revision + '（' + changed + '）；' + effective +
      (d.command_id ? ('；命令 #' + d.command_id + ' ' + commandStatusText(d.command_status || 'pending')) : '') +
      (d.runtime_error ? ('；运行错误：' + d.runtime_error) : ''));
  } catch (e) {
    if (isAbort(e)) return;
    host.toast(e.message, 'bad');
    const message = '保存失败：' + e.message;
    if (/已被其他管理员更新/.test(e.message)) {
      // 修订冲突必须以服务端为准；其余失败保留勾选，方便直接重试。
      await loadCapabilities();
      S.capDirty = false;
      setText('aiCapResult', message + '；已按服务端最新修订刷新，请重新选择后保存。');
    } else {
      setText('aiCapResult', message + '；能力勾选保留，可直接重试。');
    }
  }
}

// ---------- 四维性格与评分草稿（T-107） ----------
//
// 数据来源只有 /api/ai/personality 与 /api/ai/personality/score：
// 定义、分段、端点与派生预览都由服务端给（T-103），页面只负责渲染与草稿。
// 草稿按角色分开放：评分建议只改本页草稿，落库只发生在「保存性格」。
// 「已保存」以库里的修订为证；生效只在服务端给出同版消费证据时显示。

const PERSONALITY_HEAD_HINT = '选角色 → 读四维 → 修改 → 保存；评分建议只进草稿';
const PERSONALITY_HEAD_NOTE = '① 在「角色与订阅」写清角色描述 → ② 点「按描述建议旋钮」拿建议 → ' +
  '③ 拖动滑块或用方向键微调 → ④ 点「保存性格」才落库。评分建议只填草稿，可逐项或全部撤销；' +
  '描述改了旧建议会过期，手调值保留。';

function personalityView(key) {
  return (S.personality.views || {})[key] || null;
}

// 维度顺序与 ID 只认服务端：先看这个角色的 bands（T-103 定义），再退回顶层定义。
function personalityDimensionIDs(view) {
  const dims = (view && view.bands) || [];
  if (dims.length) return dims.map((dim) => dim.id);
  return (S.personality.dimensions || []).map((dim) => dim.id);
}

function personalityValueIDs() {
  return personalityDimensionIDs(personalityView(S.selectedKey));
}

function personalityValuesOf(view) {
  const src = (view && view.values) || {};
  const out = {};
  for (const id of personalityDimensionIDs(view)) out[id] = Number(src[id]) || 0;
  return out;
}

function clonePersonalityValues(values) {
  const src = values || {};
  const out = {};
  for (const id of personalityValueIDs()) out[id] = Number(src[id]) || 0;
  return out;
}

function personalityValuesEqual(a, b) {
  if (!a || !b) return false;
  const keys = Object.keys(a);
  if (keys.length !== Object.keys(b).length) return false;
  for (const key of keys) {
    if (Number(a[key]) !== Number(b[key])) return false;
  }
  return true;
}

function personalityDraftFor(key) {
  const view = personalityView(key);
  if (!view) return null;
  let draft = (S.personality.draft || {})[key];
  if (!draft) {
    draft = {
      values: personalityValuesOf(view),
      baseRevision: Number(view.revision) || 0,
      description: view.description || '',
      ranges: null,
      reasons: null,
      before: null,
      errors: {},
      notice: '',
      noticeBad: false,
      // 草稿版本：每次人工改动 +1；评分回包必须与发起时的版本一致才允许回填。
      version: 0,
    };
    S.personality.draft[key] = draft;
  }
  return draft;
}

// touchPersonalityDraft 标记草稿被人工改过：在途的评分建议随之作废。
function touchPersonalityDraft(draft) {
  if (draft) draft.version = (draft.version || 0) + 1;
}

function personalityDraftDirty(key) {
  const view = personalityView(key);
  const draft = (S.personality.draft || {})[key];
  if (!view || !draft) return false;
  return !personalityValuesEqual(draft.values, personalityValuesOf(view));
}

function resetPersonalityDraft(key, view) {
  const draft = (S.personality.draft || {})[key];
  if (!draft || !view) return;
  draft.values = personalityValuesOf(view);
  draft.baseRevision = Number(view.revision) || 0;
  draft.description = view.description || '';
  draft.ranges = null;
  draft.reasons = null;
  draft.before = null;
  draft.errors = {};
  touchPersonalityDraft(draft);
}

function discardPersonalityDraft(key) {
  if (!key) return;
  delete S.personality.draft[key];
  S.personality.ui = null;
}

// applyPersonality 应用一次回读：草稿按角色保留，但描述改变时旧建议作废。
//
// 每个角色的「已保存修订/代次」只增不减：回读响应可能比页面上已有的状态更旧
// （保存后迟到的旧 GET），这类角色保留页面上更新的视图，不用旧响应覆盖，
// 也不能把旧回执的生效标记盖到新版或另一个角色上。
function personalityViewSuperseded(incoming, current) {
  if (!current) return false;
  const inRev = Number(incoming && incoming.revision) || 0;
  const curRev = Number(current.revision) || 0;
  if (inRev < curRev) return true;
  const inGen = Number(incoming && incoming.current_generation) || 0;
  const curGen = Number(current.current_generation) || 0;
  return inGen > 0 && curGen > 0 && inGen < curGen;
}

function applyPersonality(d) {
  const views = {};
  const superseded = [];
  for (const view of (d.agents || [])) {
    const key = view.agent_key;
    const current = (S.personality.views || {})[key];
    if (personalityViewSuperseded(view, current)) {
      views[key] = current;
      superseded.push(key);
      continue;
    }
    views[key] = view;
  }
  S.personality.views = views;
  S.personality.superseded = superseded;
  S.personality.dimensions = d.dimensions || [];
  S.personality.bands = d.bands || [];
  S.personality.schemaVersion = Number(d.schema_version) || 1;
  S.personality.destination = d.destination || null;
  S.personality.emergency = d.emergency || null;
  S.personality.loaded = true;
  for (const key of Object.keys(S.personality.draft || {})) {
    const view = views[key];
    const draft = S.personality.draft[key];
    if (!view || !draft) continue;
    if ((view.description || '') !== draft.description) {
      // 描述是评分的输入：换过描述，旧建议区间与「原值」都不再对应当前描述。
      draft.ranges = null;
      draft.reasons = null;
      draft.before = null;
      draft.description = view.description || '';
      touchPersonalityDraft(draft);
      if (!personalityValuesEqual(draft.values, personalityValuesOf(view))) {
        draft.notice = '描述已改变：旧建议已过期，手调值保留';
        draft.noticeBad = false;
      }
    }
    if (Number(view.revision) === Number(draft.baseRevision)) continue;
    if (personalityValuesEqual(draft.values, personalityValuesOf(view))) {
      // 草稿没有改动：直接跟到新修订，不制造一次假冲突。
      draft.baseRevision = Number(view.revision) || 0;
    } else {
      draft.notice = '配置已被其他管理员更新：保存会提示冲突，可先「重新载入」';
      draft.noticeBad = true;
    }
  }
}

async function loadPersonality() {
  const seq = ++aiPersonalitySeq;
  try {
    const d = await api('/api/ai/personality');
    // 迟到：整份丢弃。否则旧修订/旧角色的生效标记会盖掉刚保存的新版本。
    if (seq !== aiPersonalitySeq) return true;
    applyPersonality(d);
    renderPersonality(true);
    return true;
  } catch (e) {
    if (isAbort(e)) return false;
    if (seq !== aiPersonalitySeq) return false;
    S.personality.loaded = false;
    host.toast('读取性格配置失败：' + e.message, 'bad');
    renderPersonality(true);
    return false;
  }
}

// ensurePersonalityMount 建四维卡片的挂载点，并把片段里的 T-107 接线占位说明
// 换成管理员可读的说明。片段（index.html）不属本票白名单，所以改写在模块里，
// 只改这两行文案，不动角色选择与能力区。
function ensurePersonalityMount() {
  const pane = $('aiPane-personality');
  if (!pane) return null;
  let mount = $('aiPersonalityMount');
  if (mount) return mount;
  mount = el('div', { id: 'aiPersonalityMount' });
  const cards = pane.querySelectorAll('.card');
  pane.insertBefore(mount, cards[1] || null);
  const first = cards[0];
  if (first) {
    const head = first.querySelector('.section-head small');
    if (head) head.textContent = PERSONALITY_HEAD_HINT;
    const note = first.querySelector('.ai-note');
    if (note) note.textContent = PERSONALITY_HEAD_NOTE;
  }
  return mount;
}

function personalityEmptyCard(text) {
  return el('div', { class: 'card' },
    el('div', { class: 'section-head' }, el('div', {}, el('h2', { text: '四维性格' }))),
    el('div', { class: 'empty', text: text }));
}

function personalityBandOf(value) {
  const bands = S.personality.bands || [];
  if (!bands.length) return null;
  return bands.find((band) => value >= band.min && value <= band.max) || bands[1] || null;
}

function personalityBandText(dim, band) {
  if (band === 'low') return dim.low_band || '';
  if (band === 'high') return dim.high_band || '';
  return dim.mid_band || '';
}

function buildPersonalityCard(mount, view) {
  const ui = { key: view.agent_key, rows: {} };
  ui.status = el('span', { class: 'pill', id: 'aiPersonStatus', text: '读取中…' });
  ui.suggest = el('button', { class: 'btn', type: 'button', 'data-action': 'suggest-personality', text: '按描述建议旋钮' });
  ui.cancel = el('button', { class: 'btn alt', type: 'button', 'data-action': 'cancel-personality-score', text: '取消评分', hidden: true });
  ui.undoAll = el('button', { class: 'btn alt', type: 'button', 'data-action': 'undo-personality-all', text: '全部撤销' });
  ui.save = el('button', { class: 'btn', type: 'button', 'data-action': 'save-personality', text: '保存性格' });
  ui.reload = el('button', { class: 'btn alt', type: 'button', 'data-action': 'reload-personality', text: '重新载入' });
  ui.notice = el('div', { class: 'ai-person-notice', id: 'aiPersonNotice', hidden: true });
  ui.effect = el('div', { class: 'muted ai-person-effect', id: 'aiPersonEffect' });
  ui.rowsBox = el('div', { class: 'ai-person-rows', id: 'aiPersonRows' });

  for (const dim of (view.bands || [])) {
    const range = el('input', {
      type: 'range', min: '0', max: '100', step: '1', 'data-role': 'range',
      'aria-label': dim.label + '（0–100，方向键微调）',
    });
    const number = el('input', {
      type: 'number', min: '0', max: '100', step: '1', class: 'ai-person-number',
      'data-role': 'number', 'aria-label': dim.label + '数值（0–100 整数）',
    });
    const undo = actionButton('btn alt sm', 'undo-personality-dim', '撤销这一项', { dim: dim.id });
    const value = el('span', { class: 'ai-person-value', 'data-role': 'value' });
    const bandText = el('span', { class: 'ai-person-bandtext', 'data-role': 'bandtext' });
    const suggest = el('span', { class: 'ai-person-suggest', 'data-role': 'suggest' });
    const bar = el('i', { class: 'ai-person-band-bar' });
    const mark = el('i', { class: 'ai-person-band-mark' });
    const band = el('div', { class: 'ai-person-band', 'data-role': 'band', hidden: true },
      el('div', { class: 'ai-person-band-track' }, bar), mark, suggest);
    const reason = el('div', { class: 'ai-person-reason', 'data-role': 'reason' });
    const warn = el('div', { class: 'ai-person-warn', 'data-role': 'warn' });
    const row = el('div', { class: 'ai-person-row', 'data-dim': dim.id },
      el('div', { class: 'ai-person-row-head' },
        el('div', { class: 'ai-person-row-title' }, el('b', { text: dim.label }), value),
        bandText),
      el('div', { class: 'ai-person-control' }, range, number, undo),
      band, reason, warn);
    range.addEventListener('input', () => onPersonalityRange(dim.id, range.value));
    number.addEventListener('input', () => onPersonalityNumber(dim.id, number.value));
    ui.rows[dim.id] = { id: dim.id, dim: dim, row: row, range: range, number: number, undo: undo,
      value: value, bandText: bandText, band: band, bar: bar, mark: mark, suggest: suggest,
      reason: reason, warn: warn };
    ui.rowsBox.append(row);
  }

  const card = el('div', { class: 'card ai-person-card' },
    el('div', { class: 'section-head' },
      el('div', {}, el('h2', { text: '四维性格' }), el('small', { text: PERSONALITY_HEAD_HINT })),
      ui.status),
    el('div', { class: 'ai-person-desc' },
      el('div', { class: 'ai-person-desc-head' },
        el('label', { text: '当前描述（来自「角色与订阅」的角色页，本页只读）' }),
        actionButton('btn alt sm', 'goto-agent-edit', '去改描述')),
      el('div', { class: 'ai-person-desc-view', id: 'aiPersonDescription' })),
    el('div', { class: 'muted ai-person-meta', id: 'aiPersonMeta' }),
    ui.rowsBox,
    el('div', { class: 'ai-person-actions' }, ui.suggest, ui.cancel, ui.undoAll, ui.save, ui.reload),
    ui.notice,
    ui.effect);
  mount.replaceChildren(card);
  S.personality.ui = ui;
}

function renderPersonality(rebuild) {
  const mount = ensurePersonalityMount();
  if (!mount) return;
  const agent = selectedAgent();
  if (!agent) {
    S.personality.ui = null;
    mount.replaceChildren(personalityEmptyCard('先在「角色与订阅」创建并选择一个角色。'));
    return;
  }
  const view = personalityView(agent.key);
  if (!view) {
    S.personality.ui = null;
    mount.replaceChildren(personalityEmptyCard(S.personality.loaded
      ? '这个角色没有读到性格配置，请刷新后重试。'
      : '读取性格配置…'));
    return;
  }
  if (!S.personality.ui || S.personality.ui.key !== agent.key || rebuild) {
    buildPersonalityCard(mount, view);
  }
  syncPersonality(view, personalityDraftFor(agent.key));
}

function syncPersonality(view, draft) {
  const ui = S.personality.ui;
  if (!ui || !view || !draft) return;
  const saved = personalityValuesOf(view);
  const dirty = !personalityValuesEqual(draft.values, saved);
  for (const id of Object.keys(ui.rows)) {
    syncPersonalityRow(ui.rows[id], view, draft, saved, false);
  }
  syncPersonalityStatus(view, dirty);
  syncPersonalityButtons(view, draft, saved, dirty);
  syncPersonalityNotice(draft);
  renderPersonalityDescription(view);
  renderPersonalityMeta(view);
  renderPersonalityEffect(view);
}

// refreshPersonalityIndicators 只刷新文字/区间/按钮，不回写输入框：
// 拖动与输入时输入框是用户的真源，回写会打断打字。
function refreshPersonalityIndicators() {
  const view = personalityView(S.selectedKey);
  const draft = personalityDraftFor(S.selectedKey);
  const ui = S.personality.ui;
  if (!ui || !view || !draft) return;
  const saved = personalityValuesOf(view);
  const dirty = !personalityValuesEqual(draft.values, saved);
  for (const id of Object.keys(ui.rows)) {
    syncPersonalityRow(ui.rows[id], view, draft, saved, true);
  }
  syncPersonalityStatus(view, dirty);
  syncPersonalityButtons(view, draft, saved, dirty);
  syncPersonalityNotice(draft);
  renderPersonalityEffect(view);
}

function syncPersonalityRow(row, view, draft, saved, skipInputs) {
  const id = row.id;
  const value = Number(draft.values[id]) || 0;
  const band = personalityBandOf(value);
  row.value.textContent = band ? (value + '/100（' + band.label + '）') : (value + '/100');
  row.bandText.textContent = band ? personalityBandText(row.dim, band.band) : '';
  const ranges = draft.ranges || null;
  const range = ranges ? ranges[id] : null;
  const reason = (draft.reasons || {})[id] || '';
  if (range && Array.isArray(range) && range.length === 2) {
    const low = Number(range[0]);
    const high = Number(range[1]);
    const before = draft.before ? Number(draft.before[id]) : Number(saved[id]);
    row.band.hidden = false;
    row.bar.style.left = Math.max(0, Math.min(100, low)) + '%';
    row.bar.style.width = Math.max(0, Math.min(100, high) - Math.max(0, low)) + '%';
    row.mark.style.left = Math.max(0, Math.min(100, value)) + '%';
    row.suggest.textContent = '建议 ' + low + '–' + high + '（原 ' + before + ' → ' + value + '）';
    row.reason.textContent = reason || '';
  } else {
    row.band.hidden = true;
    row.reason.textContent = '';
  }
  const error = (draft.errors || {})[id] || '';
  if (error) {
    row.warn.textContent = error;
    row.row.classList.add('ai-person-row-warn');
  } else if (range && (value < Number(range[0]) || value > Number(range[1]))) {
    row.warn.textContent = '超出建议区间 ' + range[0] + '–' + range[1] + '：仍可保存，只是模型认为把握不大。';
    row.row.classList.add('ai-person-row-warn');
  } else {
    row.warn.textContent = '';
    row.row.classList.remove('ai-person-row-warn');
  }
  row.undo.disabled = !ranges && value === Number(saved[id]);
  if (skipInputs) return;
  row.range.value = String(value);
  row.number.value = String(value);
}

function syncPersonalityStatus(view, dirty) {
  const node = $('aiPersonStatus');
  if (!node) return;
  let text = view.save_display || '尚未保存';
  let cls = 'pill';
  if (view.needs_repair) {
    text = '配置需修复';
    cls = 'pill bad';
  } else if (view.inactive) {
    text = '已停用';
    cls = 'pill bad';
  }
  if (dirty) {
    text = '待保存（未落库）';
    cls = 'pill ai-person-pill-warn';
  } else if (view.present && !view.needs_repair && !view.inactive) {
    cls = 'pill ok';
  }
  node.textContent = text;
  node.className = cls;
}

function syncPersonalityButtons(view, draft, saved, dirty) {
  const ui = S.personality.ui;
  if (!ui) return;
  const pending = S.personality.score.pending;
  const em = S.personality.emergency || {};
  const canScore = !!String(view.description || '').trim() && !view.inactive && !view.needs_repair && !em.blocked;
  ui.suggest.disabled = pending || !canScore;
  ui.suggest.textContent = pending ? '评分中…' : '按描述建议旋钮';
  ui.suggest.title = canScore ? '按当前描述请求一次建议；结果只填草稿'
    : (em.blocked ? '急停已开启：先到「连接」解除急停再评分'
      : (String(view.description || '').trim() ? '当前角色不能评分' : '先到「角色与订阅」给这个角色写一句描述'));
  ui.cancel.hidden = !pending;
  ui.undoAll.disabled = pending || (!dirty && !draft.ranges);
  ui.save.disabled = pending;
}

function syncPersonalityNotice(draft) {
  const node = $('aiPersonNotice');
  if (!node) return;
  const text = draft.notice || '';
  node.textContent = text;
  node.hidden = !text;
  node.classList.toggle('ai-person-notice-bad', !!draft.noticeBad);
}

function renderPersonalityDescription(view) {
  const box = $('aiPersonDescription');
  if (!box) return;
  const text = String(view.description || '').trim();
  if (!text) {
    box.replaceChildren(
      el('div', { class: 'ai-person-desc-empty', text: '这个角色还没有描述：先到「角色与订阅」给它写一句人设，再回来评分。' }),
      el('div', { class: 'ai-person-desc-hint', text: '空例：喜欢安静钓鱼，愿意尝试新水域，做事有耐心。' }));
    return;
  }
  box.replaceChildren(el('div', { class: 'ai-person-desc-text', text: text }));
}

function renderPersonalityMeta(view) {
  const node = $('aiPersonMeta');
  if (!node) return;
  const dest = S.personality.destination || {};
  const em = S.personality.emergency || {};
  const bits = ['角色：' + (view.display || view.agent_key)];
  bits.push('模型：' + (dest.display || '未配置连接'));
  bits.push(em.display || '急停状态待确认');
  const derived = view.derived || {};
  if (derived.dwell_floor_seconds || derived.rest_interval_seconds) {
    bits.push('派生预览（按已保存值）：驻留底线 ' + Math.round(Number(derived.dwell_floor_seconds) || 0) +
      ' 秒 · 休整间隔 ' + Math.round(Number(derived.rest_interval_seconds) || 0) + ' 秒');
  }
  node.textContent = bits.join(' · ');
}

function renderPersonalityEffect(view) {
  const node = $('aiPersonEffect');
  if (!node) return;
  const effect = view.effect || {};
  const bits = [view.save_display || '尚未保存'];
  if (effect.effective) {
    // 生效文案由服务端给（含被消费的修订号与真实任务/结果）；
    // 页面只在证据为真时渲染它，不自己拼一个不带版本的「已生效」。
    bits.push(effect.display || '已生效（同版运行消费已取证）');
  } else {
    bits.push(effect.display || '运行消费证据待确认');
  }
  node.textContent = bits.join(' · ');
  node.classList.toggle('ai-person-effect-warn', !effect.effective);
}

function onPersonalityRange(id, raw) {
  const view = personalityView(S.selectedKey);
  const draft = personalityDraftFor(S.selectedKey);
  const ui = S.personality.ui;
  if (!view || !draft || !ui) return;
  const value = Number(raw);
  if (!Number.isInteger(value) || value < 0 || value > 100) return;
  draft.values[id] = value;
  draft.errors[id] = '';
  touchPersonalityDraft(draft);
  const row = ui.rows[id];
  if (row) row.number.value = String(value);
  refreshPersonalityIndicators();
}

function onPersonalityNumber(id, raw) {
  const view = personalityView(S.selectedKey);
  const draft = personalityDraftFor(S.selectedKey);
  const ui = S.personality.ui;
  if (!view || !draft || !ui) return;
  const text = String(raw === undefined || raw === null ? '' : raw).trim();
  const value = Number(text);
  if (text === '' || !Number.isInteger(value) || value < 0 || value > 100) {
    draft.errors[id] = text === ''
      ? '需要 0–100 的整数（现在是空的）'
      : '需要 0–100 的整数（现在填的是「' + text + '」）';
    refreshPersonalityIndicators();
    return;
  }
  draft.values[id] = value;
  draft.errors[id] = '';
  touchPersonalityDraft(draft);
  const row = ui.rows[id];
  if (row) row.range.value = String(value);
  refreshPersonalityIndicators();
}

function personalityFirstError() {
  const draft = personalityDraftFor(S.selectedKey);
  if (!draft) return '';
  for (const id of Object.keys(draft.errors || {})) {
    if (draft.errors[id]) return id;
  }
  return '';
}

function undoPersonalityDimension(id) {
  const view = personalityView(S.selectedKey);
  const draft = personalityDraftFor(S.selectedKey);
  const ui = S.personality.ui;
  if (!view || !draft || !ui || !id) return;
  const saved = personalityValuesOf(view);
  if (draft.ranges && draft.before && Object.prototype.hasOwnProperty.call(draft.before, id)) {
    draft.values[id] = Number(draft.before[id]) || 0;
    delete draft.ranges[id];
    if (draft.reasons) delete draft.reasons[id];
    if (Object.keys(draft.ranges).length === 0) {
      draft.ranges = null;
      draft.reasons = null;
      draft.before = null;
    }
    draft.notice = '已撤销这一项的建议，回到建议前的值';
  } else {
    draft.values[id] = saved[id];
    draft.notice = '已回到已保存的值';
  }
  draft.noticeBad = false;
  draft.errors[id] = '';
  touchPersonalityDraft(draft);
  syncPersonality(view, draft);
}

function undoPersonalityAll() {
  const view = personalityView(S.selectedKey);
  const draft = personalityDraftFor(S.selectedKey);
  if (!view || !draft) return;
  draft.values = personalityValuesOf(view);
  draft.ranges = null;
  draft.reasons = null;
  draft.before = null;
  draft.errors = {};
  draft.notice = '已撤销全部草稿，回到已保存的值';
  draft.noticeBad = false;
  touchPersonalityDraft(draft);
  syncPersonality(view, draft);
}

function abortPersonalityScore() {
  S.personality.score.seq += 1;
  const controller = S.personality.score.controller;
  S.personality.score.controller = null;
  S.personality.score.pending = false;
  if (controller) {
    try {
      controller.abort();
    } catch (e) {
      /* 已结束的请求不需要再中止 */
    }
  }
}

function cancelPersonalityScore() {
  abortPersonalityScore();
  const view = personalityView(S.selectedKey);
  const draft = personalityDraftFor(S.selectedKey);
  if (view && draft) {
    draft.notice = '已取消评分：草稿未改动';
    draft.noticeBad = false;
    syncPersonality(view, draft);
  }
}

// applyPersonalitySuggestion 只把合法建议写进草稿，并保留建议前的值供逐项撤销。
function applyPersonalitySuggestion(draft, res, before) {
  const values = res.values || {};
  const next = clonePersonalityValues(draft.values);
  let changed = false;
  for (const id of Object.keys(values)) {
    const suggested = Number(values[id]);
    if (!Number.isInteger(suggested) || suggested < 0 || suggested > 100) continue;
    if (!Object.prototype.hasOwnProperty.call(next, id)) continue;
    if (suggested !== draft.values[id]) changed = true;
    next[id] = suggested;
  }
  draft.before = draft.before && draft.ranges ? draft.before : before;
  draft.values = next;
  draft.ranges = res.ranges || null;
  draft.reasons = res.reasons || null;
  draft.errors = {};
  touchPersonalityDraft(draft);
  draft.notice = changed
    ? '已按描述建议填进草稿：可逐项或全部撤销，保存才落库'
    : '建议与当前草稿一致：区间只表示把握范围，可直接保存或微调';
  draft.noticeBad = false;
}

async function suggestPersonality() {
  const key = S.selectedKey;
  const view = personalityView(key);
  const draft = personalityDraftFor(key);
  if (!view || !draft) {
    host.toast('先选择一个角色', 'bad');
    return;
  }
  if (S.personality.score.pending) return;
  if (view.inactive || view.needs_repair) {
    host.toast('这个角色的性格配置不可用于评分：先保存一次修复', 'bad');
    return;
  }
  const description = view.description || '';
  if (!description.trim()) {
    host.toast('这个角色还没有描述：先到「角色与订阅」写一句人设', 'bad');
    return;
  }
  const seq = S.personality.score.seq + 1;
  S.personality.score.seq = seq;
  S.personality.score.pending = true;
  S.personality.score.key = key;
  const requestId = 'ps-' + seq + '-' + Date.now().toString(36);
  const valuesBefore = clonePersonalityValues(draft.values);
  const descriptionBefore = description;
  const revisionBefore = Number(view.revision) || 0;
  const draftVersionBefore = draft.version || 0;
  // 本次发出时的维度 schema 版本：响应必须按同一版回显，才允许回填。
  const schemaVersionBefore = S.personality.schemaVersion;
  const controller = new AbortController();
  S.personality.score.controller = controller;
  draft.notice = '评分中：最多 30 秒；结果只填草稿';
  draft.noticeBad = false;
  syncPersonality(view, draft);
  try {
    const res = await host.api('/api/ai/personality/score', {
      method: 'POST',
      signal: controller.signal,
      body: JSON.stringify({
        request_id: requestId,
        agent_key: key,
        base_revision: revisionBefore,
        description: descriptionBefore,
        dimension_schema_version: schemaVersionBefore,
        current_values: valuesBefore,
      }),
    });
    // 迟到/被取消的响应一律丢弃：只认最新一次请求。
    if (seq !== S.personality.score.seq) return;
    // 已经切到别的角色：旧角色的评分不得再落到当前页。
    if (S.selectedKey !== key) return;
    const liveView = personalityView(key);
    const liveDraft = personalityDraftFor(key);
    if (!liveView || !liveDraft) return;
    // 先核响应身份：服务端回显的 request_id 必须是本次发出的 ID，
    // schema_version 必须是本次发出时的整数版本（不是「现在页面上的」版本）。
    // 任一不符说明响应不属于这个请求（或按另一版维度定义生成），整份拒收：
    // 不改草稿值、不写范围/理由、不动撤销历史，也不自动重评，只提示重评或手调。
    if (res.request_id !== requestId) {
      liveDraft.notice = '评分响应的 request_id 与本次请求不符：已拒收，草稿、范围与撤销历史保留；可重新评分或手动调整';
      liveDraft.noticeBad = true;
      return;
    }
    // 版本按整数类型严格比较：Number(true) / Number('1') 都能等于 1，不能靠类型收窄冒充版本。
    if (!Number.isInteger(res.schema_version) || res.schema_version !== schemaVersionBefore) {
      const gotSchema = res.schema_version === undefined || res.schema_version === null
        ? '缺失'
        : (typeof res.schema_version === 'number' ? String(res.schema_version)
          : JSON.stringify(res.schema_version));
      liveDraft.notice = '评分响应的 schema_version 为 ' + gotSchema +
        '（本次发出 ' + schemaVersionBefore + '）：已拒收，草稿、范围与撤销历史保留；可重新评分或手动调整';
      liveDraft.noticeBad = true;
      return;
    }
    if ((liveView.description || '') !== descriptionBefore) {
      liveDraft.notice = '描述已改变：本次建议已过期，草稿未改动';
      liveDraft.noticeBad = true;
      return;
    }
    if ((liveDraft.version || 0) !== draftVersionBefore) {
      liveDraft.notice = '草稿已改动：本次建议已过期，当前手调值保留';
      liveDraft.noticeBad = true;
      return;
    }
    if (Number(liveView.revision) !== revisionBefore ||
        Number(res.base_revision) !== revisionBefore) {
      liveDraft.notice = '配置已更新：本次建议已过期，草稿未改动；可先「重新载入」';
      liveDraft.noticeBad = true;
      return;
    }
    if (res.status === 'unassessable') {
      // 「无法评估」是合法拒绝：只显示固定文案，不碰草稿里已有的值与建议。
      liveDraft.notice = res.reason || '请审查提示词：描述不足或互相矛盾';
      liveDraft.noticeBad = false;
      return;
    }
    applyPersonalitySuggestion(liveDraft, res, valuesBefore);
  } catch (e) {
    if (isAbort(e)) return;
    if (seq !== S.personality.score.seq) return;
    if (authErrorText(e)) S.authStale = true;
    const liveDraft = personalityDraftFor(key);
    if (liveDraft) {
      liveDraft.notice = '评分失败：' + e.message + '（草稿未改动，可重试或手动调整）';
      liveDraft.noticeBad = true;
    }
  } finally {
    if (seq === S.personality.score.seq) {
      S.personality.score.pending = false;
      S.personality.score.controller = null;
      const liveView = personalityView(S.selectedKey);
      const liveDraft = personalityDraftFor(S.selectedKey);
      if (liveView && liveDraft) syncPersonality(liveView, liveDraft);
    }
  }
}

async function savePersonality() {
  const key = S.selectedKey;
  const view = personalityView(key);
  const draft = personalityDraftFor(key);
  if (!view || !draft) return false;
  const badID = personalityFirstError();
  if (badID) {
    const ui = S.personality.ui;
    if (ui && ui.rows[badID]) ui.rows[badID].number.focus();
    draft.notice = '有字段不合法：先改正再保存（' + draft.errors[badID] + '）';
    draft.noticeBad = true;
    syncPersonality(view, draft);
    return false;
  }
  try {
    const d = await api('/api/ai/personality', {
      method: 'POST',
      body: JSON.stringify({
        agent_key: key,
        base_revision: draft.baseRevision,
        schema_version: S.personality.schemaVersion,
        values: clonePersonalityValues(draft.values),
      }),
    });
    // 保存回执里的视图也可能迟到（另一管理员已保存更新版本）：
    // 只按角色、代次、修订前进，不用旧回执盖掉更新的版本。
    if (d.view && !personalityViewSuperseded(d.view, S.personality.views[key])) {
      S.personality.views[key] = d.view;
    }
    const current = S.personality.views[key] || view;
    resetPersonalityDraft(key, current);
    const after = S.personality.draft[key];
    if (after) {
      after.notice = d.changed
        ? ('已保存修订 ' + d.revision + '；生效以同版运行证据为准')
        : ('没有变更：仍是修订 ' + d.revision);
      after.noticeBad = false;
    }
    host.toast('已保存性格修订 ' + d.revision + (d.changed ? '' : '（无变更）'));
    await loadPersonality();
    return true;
  } catch (e) {
    if (isAbort(e)) return false;
    if (authErrorText(e)) S.authStale = true;
    const message = String(e && e.message ? e.message : '保存失败');
    const current = personalityDraftFor(key);
    if (current) {
      if (/已被其他管理员更新/.test(message)) {
        current.notice = message + '；草稿已保留，可点「重新载入」按最新修订重填';
      } else {
        current.notice = '保存失败：' + message + '（草稿保留）';
      }
      current.noticeBad = true;
    }
    host.toast(message, 'bad');
    const liveView = personalityView(key);
    if (liveView && current) syncPersonality(liveView, current);
    return false;
  }
}

async function reloadPersonality() {
  const key = S.selectedKey;
  discardPersonalityDraft(key);
  const ok = await loadPersonality();
  if (!ok) return;
  const view = personalityView(key);
  const draft = personalityDraftFor(key);
  if (view && draft) {
    draft.notice = '已按最新配置重新载入';
    draft.noticeBad = false;
    syncPersonality(view, draft);
  }
  host.toast('已重新载入性格配置');
}

// ---------- 切换角色时的三选一（保存 / 放弃 / 取消） ----------

function requestPersonalitySwitch(nextKey) {
  const key = S.selectedKey;
  const agent = agentByKey(key);
  const name = (agent && (agent.display || agent.key)) || key;
  const body = el('div', { class: 'ai-person-switch' },
    el('p', { class: 'muted', text: '「' + name + '」有四维草稿还没保存。切到另一个角色之前，先决定这份草稿怎么办：' }),
    el('div', { class: 'ai-person-actions' },
      actionButton('btn', 'personality-switch-save', '保存并切换', { next: nextKey }),
      actionButton('btn alt', 'personality-switch-discard', '放弃并切换', { next: nextKey }),
      actionButton('btn alt', 'personality-switch-cancel', '取消', { next: nextKey })));
  openAIModal('有四维草稿未保存', '草稿只存在这个页面里，刷新或关页会丢', body);
}

async function switchPersonalityWithSave(nextKey) {
  closeAIModal();
  const draft = personalityDraftFor(S.selectedKey);
  if (draft) draft.notice = '';
  const ok = await savePersonality();
  if (!ok || personalityDraftDirty(S.selectedKey)) return;
  selectAgent(nextKey, true);
}

function switchPersonalityDiscard(nextKey) {
  closeAIModal();
  discardPersonalityDraft(S.selectedKey);
  selectAgent(nextKey, true);
}

function switchPersonalityCancel() {
  closeAIModal();
  syncAgentSelects();
}

// ---------- 路由注册 ----------

if (host && typeof host.api === 'function' && typeof host.registerExternalView === 'function') {
  host.registerExternalView('ai', {
    title: 'AI 助手',
    sub: '让 AI 角色在游戏里陪玩家钓鱼、聊天',
    enter: enter,
    refresh: refresh,
    leave: leave,
  });
  if (typeof host.onLogout === 'function') host.onLogout(() => { S.authStale = true; });
} else {
  console.error('AI 前端模块缺少后台窄桥 RF4Admin');
}
