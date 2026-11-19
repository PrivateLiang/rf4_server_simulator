const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { test } = require('node:test');

let playwright;
try {
  playwright = require('playwright');
} catch {
  playwright = require('playwright-core');
}

test('DR-109 连接页保存、校验草稿、丢回包及脱敏', async () => {
  const browser = await playwright.chromium.launch({ headless: true, executablePath: process.env.RF4_TEST_BROWSER });
  try {
    const page = await browser.newPage();
    await page.addInitScript(() => localStorage.setItem('rf4_admin_token', 'fixture'));
    const web = path.join(__dirname, 'web');
    const files = {
      '/': ['index.html', 'text/html'],
      '/ai/index.html': ['ai/index.html', 'text/html'],
      '/ai/ai.js': ['ai/ai.js', 'text/javascript'],
      '/ai/ai.css': ['ai/ai.css', 'text/css'],
    };
    const destination = { host: 'first.example.test', model: 'model-a' };
    const secret = 'private-test-marker';
    let revision = 0;
    let posts = 0;
    let retests = 0;
    let failure = '没有可用连接';
    let lost = false;
    await page.route('**/*', async (route) => {
      const request = route.request();
      const pathname = new URL(request.url()).pathname;
      const file = files[pathname];
      if (file) return route.fulfill({ contentType: file[1], body: fs.readFileSync(path.join(web, file[0])) });
      if (pathname === '/api/me') return route.fulfill({ json: { username: 'admin' } });
      if (pathname === '/api/ai/status') {
        return route.fulfill({ json: {
          revision, base_url: 'https://' + destination.host, model_id: revision ? destination.model : '',
          destination, credential: { state: revision ? 'configured' : 'missing' },
          retest: {}, bridge: { online: false }, onboarding: { steps: [] },
        } });
      }
      if (pathname === '/api/ai/config') {
        posts++;
        assert.equal(request.postDataJSON().api_key, secret);
        if (failure) return route.fulfill({ status: 500, json: { error: failure } });
        revision++;
        if (lost) return route.abort('failed');
        return route.fulfill({ json: { ok: true, saved: true, revision } });
      }
      if (pathname === '/api/ai/test') {
        retests++;
        return route.fulfill({ json: { error: '厂商拒绝鉴权，请核对测试 Key 和模型权限' } });
      }
      return route.fulfill({ json: {} });
    });
    await page.goto('http://127.0.0.1:19217/#ai');
    await page.locator('[data-ai-tab="conn"]').click();
    await page.locator('#aiModelID').fill(destination.model);
    await page.locator('#aiKey').fill(secret);
    const save = page.locator('[data-action="save-ai-config"]');
    const testConnection = page.locator('[data-action="test-ai"]');
    const result = page.locator('#aiTestResult');

    await testConnection.click();
    await result.getByText('当前输入尚未保存', { exact: false }).waitFor();
    assert.equal(retests, 0);
    await save.click();
    await page.waitForFunction(() => document.querySelector('#aiTestResult').textContent.includes('没有可用连接'));
    assert.equal(await page.locator('#aiKey').inputValue(), secret);

    failure = '';
    await save.click();
    await page.waitForFunction(() => document.querySelector('#aiTestResult').textContent.includes('修订 1'));
    assert.equal(await page.locator('#aiKey').inputValue(), '');
    assert.match(await result.textContent(), /first\.example\.test.*待复测.*加密.*测试文字/);
    assert.doesNotMatch(await page.locator('body').textContent(), new RegExp(secret));
    await testConnection.click();
    await page.waitForFunction(() => document.querySelector('#aiTestResult').textContent.includes('核对测试 Key'));
    assert.equal(retests, 1);

    lost = true;
    await page.locator('#aiKey').fill(secret);
    await save.click();
    await page.waitForFunction(() => document.querySelector('#aiTestResult').textContent.includes('已回读当前配置'));
    assert.match(await result.textContent(), /修订 2/);
    assert.equal(posts, 3);
    assert.doesNotMatch(await page.locator('body').textContent(), new RegExp(secret));
  } finally {
    await browser.close();
  }
});
