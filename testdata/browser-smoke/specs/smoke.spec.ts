import { expect, test } from '@playwright/test';

const adminToken = 'pw-admin-fixture';
const pageRoutes = ['/', '/setup', '/progress', '/providers', '/collections', '/worker-setup', '/workers'];

async function login(page: import('@playwright/test').Page) {
  await page.goto('/');
  const token = page.locator('#admin-token');
  await expect(token).toBeVisible();
  await token.fill(adminToken);
  await Promise.all([
    page.waitForResponse((response) => response.url().endsWith('/api/v1/auth/admin/session') && response.request().method() === 'POST'),
    page.locator('#admin-login').click(),
  ]);
  await expect(page.locator('#admin-session-state')).toHaveText('已登录');
}

test.describe('core browser surface', () => {
  test('serves the primary pages from the offline fixture', async ({ page }) => {
    for (const route of pageRoutes) {
      const response = await page.goto(route);
      expect(response?.status(), route).toBe(200);
      await expect(page.locator('[data-app-shell]')).toBeVisible();
      expect(await page.evaluate(() => ({
        local: window.localStorage.length,
        session: window.sessionStorage.length,
        token: document.body.innerText.includes('pw-admin-fixture'),
      }))).toEqual({ local: 0, session: 0, token: false });
    }
  });

  test.describe('session security', () => {
    test('uses an opaque secure browser session and no browser token storage', async ({ page, context }) => {
      await login(page);
      const storage = await page.evaluate(() => ({
        local: window.localStorage.length,
        session: window.sessionStorage.length,
        token: document.body.innerText.includes('pw-admin-fixture'),
      }));
      expect(storage).toEqual({ local: 0, session: 0, token: false });

      const cookies = await context.cookies();
      const session = cookies.find((cookie) => cookie.name === '__Host-timingdex_admin_session');
      const csrf = cookies.find((cookie) => cookie.name === '__Host-timingdex_csrf');
      expect(session).toMatchObject({ secure: true, httpOnly: true, sameSite: 'Strict', path: '/' });
      expect(csrf).toMatchObject({ secure: true, httpOnly: false, sameSite: 'Strict', path: '/' });
      expect(session?.value).not.toBe(adminToken);
      expect(page.url()).toMatch(/^https:\/\//);
    });

    test('rejects a session mutation without the CSRF header', async ({ page }) => {
      await login(page);
      const response = await page.evaluate(async () => {
        const result = await fetch('/api/v1/roots', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ path: '/tmp/browser-smoke' }),
        });
        return { status: result.status, body: await result.text() };
      });
      expect(response.status).toBe(403);
      expect(response.body).toContain('csrf');
    });
  });

  test('escapes hostile fixture content and keeps the page within the viewport', async ({ page }) => {
    await page.goto('/');
    await expect(page.locator('[data-library-browser]')).toBeVisible();
    await page.waitForTimeout(150);
    const result = await page.evaluate(() => ({
      xss: Boolean((window as unknown as { __xss?: unknown }).__xss),
      eventAttribute: document.querySelector('[onerror], [onclick*="__xss"]') !== null,
      overflow: document.documentElement.scrollWidth > window.innerWidth + 1,
    }));
    expect(result.xss).toBe(false);
    expect(result.eventAttribute).toBe(false);
    expect(result.overflow).toBe(false);
  });

  test('renders worker setup over HTTPS without exposing a pairing token', async ({ page }) => {
    await page.goto('/worker-setup');
    await expect(page.locator('[data-worker-setup-wizard]')).toBeVisible();
    await expect(page.locator('#hub-info')).toContainText('TLS');
    await expect(page.locator('#hub-info')).toContainText('已启用');
    expect(await page.locator('body').innerText()).not.toContain(adminToken);
  });

  test('renders the seeded collection with escaped hostile labels', async ({ page }) => {
    await page.goto('/collections');
    await expect(page.locator('#collections')).toBeVisible();
    await expect(page.locator('[data-coll="collection-fixture"]')).toBeVisible();
    expect(await page.locator('#collections').innerText()).toContain('<img src=x onerror=window.__xss=1>');
    await expect(page.locator('#collections [onerror]')).toHaveCount(0);
  });

  test('keeps provider configuration and queue failure details out of public page text', async ({ page }) => {
    await page.goto('/progress');
    await expect(page.locator('#jobs')).toBeVisible();
    const progressText = await page.locator('body').innerText();
    expect(progressText).not.toContain('fixture provider failure');
    expect(progressText).not.toContain(adminToken);

    await login(page);
    await page.goto('/providers');
    await expect(page.locator('#channels')).toBeVisible();
    await page.getByRole('button', { name: '加载通道' }).click();
    await expect(page.locator('[data-channel="channel-fixture"]')).toBeVisible();
    const providerText = await page.locator('body').innerText();
    expect(providerText).toContain('Fixture channel');
    expect(providerText).not.toContain('fixture/secret');
    expect(providerText).not.toContain(adminToken);
  });

  test('searches from the library shell without leaking credentials into requests', async ({ page }) => {
    const urls: string[] = [];
    page.on('request', (request) => urls.push(request.url()));
    await page.goto('/');
    const query = page.locator('#q');
    await expect(query).toBeVisible();
    await query.fill('camera');
    const responsePromise = page.waitForResponse((response) => response.url().includes('/api/v1/search'));
    await query.press('Enter');
    expect((await responsePromise).status()).toBe(200);
    expect(urls.join('\n')).not.toContain(adminToken);
  });
});
