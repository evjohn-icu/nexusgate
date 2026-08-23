import { expect, test } from '@playwright/test';
import type { Page } from '@playwright/test';

const adminToken = 'pw-admin-fixture';
// Every grounded browser route. The list mirrors the route table rather than a
// hand-picked sample; when a page route is added or removed, this must follow.
const pageRoutes = ['/', '/setup', '/progress', '/workers', '/library-roots', '/repurpose', '/tags', '/providers', '/collections', '/settings', '/worker-setup'];
// The closed set of UI locales; the config's default context locale is zh-CN.
const supportedLocales = ['zh-CN', 'ja-JP', 'en-US', 'fr-FR', 'es-ES'];

const fixtureBaseUrl = process.env.TIMINGDEX_PLAYWRIGHT_BASE_URL ?? 'https://127.0.0.1:4173';

async function login(page: Page) {
  await page.goto('/');
  const token = page.locator('#admin-token');
  // On narrow viewports the auth controls live in the collapsed sidebar menu;
  // open it so the token field is reachable.
  if (!(await token.isVisible())) {
    const toggle = page.locator('#shell-menu-toggle');
    if (await toggle.isVisible()) {
      await toggle.click();
    }
  }
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
    await page.locator('#refresh-channels').click();
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

test.describe('localization', () => {
  test('renders every route in every locale cookie with matching lang and selector', async ({ browser }) => {
    for (const locale of supportedLocales) {
      const context = await browser.newContext();
      await context.addCookies([{ name: 'timingdex_locale', value: locale, url: fixtureBaseUrl }]);
      const page = await context.newPage();
      for (const route of pageRoutes) {
        const label = `${route} in ${locale}`;
        const response = await page.goto(route);
        expect(response?.status(), label).toBe(200);
        await expect(page.locator('[data-app-shell]'), label).toBeVisible();
        expect(await page.locator('html').getAttribute('lang'), label).toBe(locale);
        await expect(page.locator('#shell-locale'), label).toHaveValue(locale);
        const overflow = await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth + 1);
        expect(overflow, `${label} overflows horizontally`).toBe(false);
      }
      await context.close();
    }
  });

  test('renders representative fetched/dynamic copy in each glossary locale', async ({ browser }) => {
    const cases = [
      { locale: 'ja-JP', route: '/', term: '素材ライブラリ' },
      { locale: 'en-US', route: '/providers', term: 'Model services' },
      { locale: 'fr-FR', route: '/settings', term: 'Paramètres' },
      { locale: 'es-ES', route: '/collections', term: 'Colecciones' },
    ];
    for (const c of cases) {
      const context = await browser.newContext();
      await context.addCookies([{ name: 'timingdex_locale', value: c.locale, url: fixtureBaseUrl }]);
      const page = await context.newPage();
      await page.goto(c.route);
      await expect(page.locator('body'), `${c.locale} on ${c.route}`).toContainText(c.term);
      await context.close();
    }
  });

  test('negotiates from Accept-Language, persists a switch, and falls back to zh-CN', async ({ browser }) => {
    // Without a locale cookie, Accept-Language negotiates the UI language.
    const ja = await browser.newContext({ locale: 'ja-JP' });
    const jaPage = await ja.newPage();
    await jaPage.goto('/');
    await expect(jaPage.locator('html')).toHaveAttribute('lang', 'ja-JP');
    await expect(jaPage.locator('body')).toContainText('素材ライブラリ');

    // Switching through the shell selector writes the preference cookie and
    // reloads the current path/query. On narrow viewports the selector lives
    // inside the collapsed sidebar, so open the menu first.
    const menuToggle = jaPage.locator('#shell-menu-toggle');
    if (await menuToggle.isVisible()) {
      await menuToggle.click();
      await expect(jaPage.locator('#shell-locale')).toBeVisible();
    }
    await jaPage.locator('#shell-locale').selectOption('en-US');
    await expect(jaPage.locator('html')).toHaveAttribute('lang', 'en-US', { timeout: 15000 });

    // The preference persists into a fresh page of the same context.
    const persisted = await ja.newPage();
    await persisted.goto('/');
    await expect(persisted.locator('html')).toHaveAttribute('lang', 'en-US');
    await expect(persisted.locator('body')).toContainText('Library');

    // The cookie value is the canonical supported tag, and no browser storage
    // was used to remember the preference.
    const cookies = await ja.cookies(fixtureBaseUrl);
    const localeCookie = cookies.find((cookie) => cookie.name === 'timingdex_locale');
    expect(localeCookie?.value).toBe('en-US');
    const storage = await persisted.evaluate(() => ({
      local: window.localStorage.length,
      session: window.sessionStorage.length,
    }));
    expect(storage).toEqual({ local: 0, session: 0 });

    // An unsupported browser language falls back to the zh-CN default.
    const de = await browser.newContext({ locale: 'de-DE' });
    const dePage = await de.newPage();
    await dePage.goto('/');
    await expect(dePage.locator('html')).toHaveAttribute('lang', 'zh-CN');

    await de.close();
    await ja.close();
  });
});
