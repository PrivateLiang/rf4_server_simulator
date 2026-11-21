// 新后台活动范围的浏览器回归。用 node --test 运行，需要 Playwright 和 Chromium。
const { test } = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');

let chromium;
try {
  ({ chromium } = require('playwright'));
} catch {
  ({ chromium } = require('playwright-core'));
}

test('角色活动范围按标识回显，并等待游戏服确认频道操作', async (t) => {
  const browser = await chromium.launch({ executablePath: process.env.RF4_TEST_BROWSER });
  t.after(() => browser.close());
  const page = await browser.newPage();
  page.setDefaultTimeout(5000);
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  await page.addInitScript(() => localStorage.setItem('rf4_admin_token', 'fixture'));

  const agent = { key: 'fisher', display: '同名钓友', role: '测频道角色', joined: true };
  const peer = { key: 'peer', display: agent.display, role: '对照角色', joined: true };
  const channel = 'nav1_03';
  let channels = [];
  let mapKeys = [];
  let pending;
  let receipt = 'pending';
  let reads = 0;
  let posts = 0;
  const members = (keys) => ({ count: keys.length, agent_keys: keys, agents: keys.map(() => agent.display) });
  await page.route('http://rf4.test/**', async (route) => {
    const request = route.request();
    const { pathname } = new URL(request.url());
    const file = pathname === '/' ? '/index.html' : pathname;
    const json = (body, status = 200) => route.fulfill({ status, json: body });
    if (file === '/index.html' || file.startsWith('/app/')) {
      return route.fulfill({ path: path.join(__dirname, 'web', file) });
    }
    if (pathname === '/api/me') return json({ username: 'admin' });
    if (pathname === '/api/ai/status') return json({ agents: [agent, peer] });
    if (pathname === '/api/ai/channels') {
      return json({ channels, maps: [{ map_key: '*', ...members(mapKeys) }] });
    }
    if (pathname === '/api/ai/channel' && request.method() === 'POST') {
      const body = request.postDataJSON();
      if (body.map_key) {
        mapKeys = body.enabled ? [body.agent] : [];
        return json({ ok: true });
      }
      pending = body;
      posts++;
      return json({ status: 'pending', command_id: 17 }, 202);
    }
    if (pathname === '/api/ai/commands/17') {
      reads++;
      if (receipt === 'applied') {
        channels = [{ name: channel, ...members(pending.enabled ? [agent.key, peer.key] : [peer.key]) }];
      }
      return json({ status: receipt, reason: receipt === 'rejected' ? 'emergency_stop' : '' }, receipt === 'pending' ? 202 : 200);
    }
    return json({});
  });

  await page.goto('http://rf4.test/#ai/agents');
  await page.getByRole('button', { name: /测频道角色/ }).click();
  const drawer = page.locator('.drawer');
  await drawer.getByRole('tab', { name: '活动范围', exact: true }).click();
  await drawer.getByRole('button', { name: '添加', exact: true }).click();
  await drawer.getByText('所有地图（任何频道）', { exact: true }).waitFor();
  await drawer.getByText('按频道设置').click();
  const advanced = drawer.locator('details');
  await advanced.getByRole('textbox', { name: '频道标识' }).fill(channel);
  const add = advanced.getByRole('button', { name: '添加频道' });
  await add.click();
  await page.getByText('已提交，正在应用到游戏…').waitFor();
  assert.equal(await add.isDisabled(), true);
  assert.equal(posts, 1);
  assert.deepEqual(pending, { channel, agent: agent.key, enabled: true });
  assert.equal(await advanced.getByRole('button', { name: '移除' }).count(), 0);

  receipt = 'applied';
  const remove = advanced.getByRole('button', { name: '移除' });
  await remove.waitFor();
  assert.equal(await add.isDisabled(), true);
  assert.ok(reads > 0);

  receipt = 'rejected';
  await remove.click();
  await page.getByText(/AI 紧急停止已开启，请解除后重试/).waitFor();
  assert.equal(await remove.isEnabled(), true);
  assert.equal(posts, 2);

  receipt = 'applied';
  await remove.click();
  await remove.waitFor({ state: 'detached' });
  assert.equal(await advanced.getByRole('combobox', { name: '频道' }).inputValue(), channel);
  assert.equal(await add.isEnabled(), true);
  assert.deepEqual(errors, []);
});
