// 运行 node --test，需要 Playwright 和 Chromium，可用 RF4_TEST_BROWSER 指定浏览器。
const { test } = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');
const fs = require('node:fs/promises');

let chromium;
try { ({ chromium } = require('playwright')); }
catch { ({ chromium } = require('playwright-core')); }

test('管理页可维护资料能力、本地资料和搜索方式', async (t) => {
  const browser = await chromium.launch({ executablePath: process.env.RF4_TEST_BROWSER });
  t.after(() => browser.close());
  const page = await browser.newPage();
  page.setDefaultTimeout(5000);
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  await page.addInitScript(() => localStorage.setItem('rf4_admin_token', 'fixture'));
  const agent = { key: 'shop_fisher', display: '装备助手', joined: true };
  const catalog = [
    { id: 'guide_help', group: 'knowledge', legacy: true, label: '旧攻略授权' },
    { id: 'fishing_knowledge', group: 'knowledge', label: '垂钓资料' },
    { id: 'shop_knowledge', group: 'knowledge', label: '装备与商店' },
    { id: 'craft_knowledge', group: 'knowledge', label: '制作配方' },
    { id: 'online_search', group: 'knowledge', label: '联网搜索' },
    { id: 'virtual_fishing', label: '虚拟垂钓' },
  ];
  let capabilities = { key: agent.key, present: false, legacy: true, revision: 3, capabilities: ['guide_help', 'online_search'], availability: 'unavailable' };
  let snapshot = { revision: 0, document: { item_aliases: [], notes: [] } };
  const grantRequests = [];
  const knowledgeRequests = [];
  const settingsRequests = [];
  const searchSettings = { web_search_enabled: '0', search_endpoint: 'https://custom.example/search' };
  let nativeSupported = true;
  await page.route('http://rf4.test/**', async (route) => {
    const request = route.request();
    const { pathname } = new URL(request.url());
    const filename = pathname === '/' ? '/index.html' : pathname;
    const json = (body, status = 200) => route.fulfill({ status, json: body });
    if (filename === '/index.html' || filename.startsWith('/app/')) {
      return route.fulfill({ path: path.join(__dirname, 'web', filename) });
    }
    if (pathname === '/api/me') return json({ username: 'admin' });
    if (pathname === '/api/catalog/items') return json([{ item_id: 'rod_test', name: '测试钓竿' }]);
    if (pathname === '/api/ai/status') return json({
      agents: [agent], settings: searchSettings,
      specs: [
        { key:'web_search_enabled', kind:'bool', def:'0' },
        { key:'search_endpoint', kind:'string', def:'' },
      ],
      web_search: { mode: searchSettings.web_search_mode || 'custom', native_supported:nativeSupported, native_confirmed_at:1710000000 },
    });
    if (pathname === '/api/ai/settings' && request.method() === 'POST') {
      const changes = request.postDataJSON();
      settingsRequests.push(changes);
      Object.assign(searchSettings, changes);
      return json({ ok: true });
    }
    if (pathname === '/api/ai/capabilities') {
      if (request.method() === 'POST') {
        const req = request.postDataJSON();
        grantRequests.push(req);
        capabilities = { ...capabilities, revision: req.base_revision + 1, capabilities: req.capabilities };
        return json({ changed: true });
      }
      return json({ catalog, agents: [capabilities] });
    }
    if (pathname === '/api/ai/local-knowledge') {
      if (request.method() === 'POST') {
        const req = request.postDataJSON();
        knowledgeRequests.push(req);
        snapshot = { revision: req.base_revision + 1, document: req.document };
      }
      return json(snapshot);
    }
    return json({});
  });

  await page.goto('http://rf4.test/#ai/agents');
  await page.getByRole('button', { name: /装备助手/ }).click();
  const drawer = page.locator('.drawer');
  await drawer.getByRole('tab', { name: '能力', exact: true }).click();
  assert.equal(await drawer.getByRole('checkbox', { name: '旧攻略授权' }).count(), 0);
  assert.equal(await drawer.getByRole('checkbox', { name: '联网搜索' }).count(), 0);
  await drawer.getByText('沿用历史只读范围').waitFor();
  await drawer.getByRole('checkbox', { name: '装备与商店' }).check();
  await drawer.getByRole('button', { name: '保存能力' }).click();
  await page.getByText('已保存', { exact:true }).waitFor();
  assert.deepEqual(new Set(grantRequests[0].capabilities), new Set(['guide_help', 'online_search', 'shop_knowledge']));
  await drawer.getByRole('button', { name: '去配置联网' }).click();
  await page.getByRole('combobox', { name: '搜索方式' }).waitFor();
  searchSettings.web_search_enabled = '1';
  await page.goto('http://rf4.test/#ai/agents');
  await page.getByRole('button', { name: /装备助手/ }).click();
  await drawer.getByRole('tab', { name: '能力', exact: true }).click();
  assert.equal(await drawer.getByRole('checkbox', { name: '联网搜索' }).isChecked(), true);
  assert.equal(await drawer.getByRole('button', { name: '去配置联网' }).count(), 0);
  assert.equal(await drawer.getByRole('checkbox', { name: '装备与商店' }).isChecked(), true);
  await drawer.getByRole('button', { name: '移除历史权限' }).click();
  await drawer.getByRole('button', { name: '保存能力' }).click();
  await drawer.getByRole('button', { name: '移除历史权限' }).waitFor({ state: 'detached' });
  assert.deepEqual(grantRequests[1], { agent_key:agent.key, base_revision:4, capabilities:['shop_knowledge', 'online_search'] });
  await drawer.getByRole('button', { name: '关闭' }).click();

  await page.goto('http://rf4.test/#ai/talk');
  await page.getByRole('button', { name: '添加社区别名' }).click();
  const dialog = page.getByRole('dialog');
  await dialog.getByLabel('社区别名').fill('蓝竿');
  await dialog.getByLabel('物品名称或 ID').fill('测试钓竿');
  await dialog.getByRole('button', { name: /测试钓竿/ }).click();
  await dialog.getByRole('button', { name: '保存', exact:true }).click();
  await dialog.waitFor({ state: 'detached' });
  await page.getByText('蓝竿', { exact:true }).waitFor();
  await page.getByRole('button', { name: '编辑 蓝竿' }).click();
  await dialog.getByLabel('社区别名').fill('小蓝竿');
  await dialog.getByRole('button', { name: '保存', exact:true }).click();
  await dialog.waitFor({ state: 'detached' });
  await page.getByRole('searchbox', { name: '搜索本地资料' }).fill('不存在');
  await page.getByText('没有匹配的资料').waitFor();
  await page.getByRole('searchbox', { name: '搜索本地资料' }).fill('');
  await page.getByText('小蓝竿', { exact:true }).waitFor();
  await page.getByRole('button', { name: '本地心得', exact:true }).click();
  await page.getByRole('button', { name: '添加本地心得' }).click();
  await dialog.getByLabel('资料主题').selectOption('shop');
  await dialog.getByLabel('标题').fill('先确认型号');
  await dialog.getByLabel('关键词').fill('蓝竿，规格');
  await dialog.getByLabel('来源').fill('门店实测');
  await dialog.getByLabel('心得正文').fill('核对装备规格。');
  await dialog.getByRole('button', { name: '保存', exact:true }).click();
  await dialog.waitFor({ state: 'detached' });
  assert.deepEqual(knowledgeRequests, [
    { base_revision: 0, document: { item_aliases: [{ alias:'蓝竿', item_id:'rod_test' }], notes: [] } },
    { base_revision: 1, document: { item_aliases: [{ alias:'小蓝竿', item_id:'rod_test' }], notes: [] } },
    { base_revision: 2, document: snapshot.document },
  ]);
  assert.deepEqual(snapshot.document.notes, [
    { topic:'shop', title:'先确认型号', keywords:['蓝竿', '规格'], source:'门店实测', text:'核对装备规格。' },
  ]);
  const [download] = await Promise.all([page.waitForEvent('download'), page.getByRole('button', { name: '备份', exact:true }).click()]);
  assert.deepEqual(JSON.parse(await fs.readFile(await download.path(), 'utf8')), snapshot.document);
  const doc = { item_aliases: [], notes: [] };
  await page.locator('input[type=file]').setInputFiles({ name: 'knowledge.json', mimeType: 'application/json', buffer: Buffer.from(JSON.stringify(doc)) });
  await dialog.getByRole('button', { name: '替换资料' }).click();
  await page.getByText('资料已导入', { exact:true }).waitFor();
  assert.deepEqual(snapshot.document, doc);
  await page.setViewportSize({ width:390, height:844 });
  const knowledge = page.locator('section').filter({ has: page.getByRole('heading', { name:'本地资料', exact:true }) });
  assert.equal(await knowledge.evaluate((el) => el.scrollWidth <= el.clientWidth), true);
  await page.getByRole('button', { name: '添加本地心得' }).click();
  assert.ok((await dialog.getByLabel('心得正文').boundingBox()).height >= 100);
  await dialog.getByRole('button', { name:'取消' }).click();
  await page.setViewportSize({ width:1280, height:900 });

  await page.goto('http://rf4.test/#ai/conn');
  const mode = page.getByRole('combobox', { name: '搜索方式' });
  const endpoint = page.getByLabel('搜索服务地址');
  assert.equal(await mode.inputValue(), 'custom');
  assert.equal(await endpoint.isVisible(), true);
  await mode.selectOption('native');
  assert.equal(await endpoint.isVisible(), false);
  await page.getByText(/当前连接已确认执行过搜索/).waitFor();
  await Promise.all([
    page.waitForResponse((response) => response.url().endsWith('/api/ai/status')),
    page.getByRole('button', { name: '保存修改' }).click(),
  ]);
  assert.deepEqual(settingsRequests, [{ web_search_mode:'native' }]);

  nativeSupported = false;
  await page.reload();
  await page.getByText(/当前连接不支持原生搜索/).waitFor();
  assert.equal(await mode.inputValue(), 'native');
  await mode.selectOption('custom');
  assert.equal(await endpoint.inputValue(), searchSettings.search_endpoint);
  assert.deepEqual(errors, []);
});
