import { expect, test } from '@playwright/test';
import type { Page } from '@playwright/test';

const adminToken = 'pw-admin-fixture';
// Every grounded browser route. The list mirrors the route table rather than a
// hand-picked sample; when a page route is added or removed, this must follow.
const pageRoutes = ['/', '/setup', '/progress', '/workers', '/library-roots', '/repurpose', '/tags', '/providers', '/collections', '/settings', '/worker-setup'];
// The closed set of UI locales; the config's default context locale is zh-CN.
const supportedLocales = ['zh-CN', 'ja-JP', 'en-US', 'fr-FR', 'es-ES'];

const fixtureBaseUrl = process.env.NEXUSSLATE_PLAYWRIGHT_BASE_URL ?? 'https://127.0.0.1:4173';

async function login(page: Page) {
  await page.goto('/');
  // The admin token input lives inside the compact auth dialog (UI-002): open
  // the dialog via its trigger, opening the collapsed mobile menu first when
  // the trigger is hidden behind it on narrow viewports.
  const trigger = page.locator('#admin-trigger');
  if (!(await trigger.isVisible())) {
    const toggle = page.locator('#shell-menu-toggle');
    if (await toggle.isVisible()) {
      await toggle.click();
    }
  }
  await trigger.click();
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
      const session = cookies.find((cookie) => cookie.name === '__Host-nexusslate_admin_session');
      const csrf = cookies.find((cookie) => cookie.name === '__Host-nexusslate_csrf');
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

test.describe('consolidated UI (UI-001..UI-010)', () => {
  test('sidebar IA: nine links, processing under core, wizards out of nav, auth behind the dialog', async ({ page }) => {
    await page.goto('/');
    // 3 core + 5 system + 1 labs = 9 nav links.
    await expect(page.locator('.nav-link')).toHaveCount(9);
    // /progress carries the core "处理" label; the wizard links left the nav.
    await expect(page.locator('.nav-link[href="/progress"]')).toHaveText('处理');
    await expect(page.locator('.nav-link[href="/worker-setup"]')).toHaveCount(0);
    await expect(page.locator('.nav-link[href="/setup"]')).toHaveCount(0);
    // The system group is a collapsed <details> whose links exist in the DOM.
    await expect(page.locator('.nav-group-system summary')).toHaveText('系统');
    await expect(page.locator('.nav-link[href="/library-roots"]')).toHaveText('素材目录');
    await expect(page.locator('.nav-link[href="/providers"]')).toHaveText('模型服务');
    await expect(page.locator('.nav-link[href="/workers"]')).toHaveText('处理节点');
    // Admin auth is a compact trigger + dialog; the token field is not
    // part of the default-visible page.
    await expect(page.locator('.shell-auth')).not.toHaveAttribute('hidden', '');
    await expect(page.locator('#admin-token')).not.toBeVisible();
  });

  test('library search-first surface and URL filter persistence', async ({ page }) => {
    await page.goto('/');
    await expect(page.locator('#q')).toBeVisible();
    await expect(page.locator('#filters-toggle')).toBeAttached();
    await expect(page.locator('#date-from')).toBeAttached();
    await expect(page.locator('#status-filter')).toBeAttached();
    await expect(page.locator('#collection-filter')).toBeAttached();
    // The filters drawer opens with both the SHOT and ASSET groups.
    await page.locator('#filters-toggle').click();
    await expect(page.locator('.filter-drawer')).toHaveClass(/open/);
    await expect(page.locator('.filter-drawer .filter-group-title')).toHaveText(['镜头', '素材']);
    // URL persistence: ?status=ready restores the status filter.
    await page.goto('/?status=ready');
    await expect(page.locator('#status-filter')).toHaveValue('ready');
  });

  test('shot drawer ships the 720px/48vw panel and detailed evidence slot', async ({ page }) => {
    await page.goto('/');
    await expect(page.locator('#shot-drawer-evidence')).toHaveCount(1);
    const html = await page.content();
    expect(html).toContain('width:min(720px,48vw)');
    expect(html).toContain('.shot-result-tc');
    expect(html).toContain('evidenceBox.innerHTML=shotEvidence(shot)');
  });
  test('search renders shot cards with a timecode overlay and folded evidence', async ({ page }) => {
    await page.goto('/');
    await page.locator('#q').fill('camera');
    await page.locator('#q').press('Enter');
    await expect(page.locator('.shot-result').first()).toBeVisible();
    await expect(page.locator('.shot-result .shot-result-tc').first()).toBeVisible();
    await expect(page.locator('.shot-result .shot-result-tc').first()).toContainText('00:00');
    // Evidence is folded: the first card shows at most 3 non-negated marks.
    expect(await page.locator('.shot-result .shot-evidence .ev-confirmed, .shot-result .shot-evidence .ev-possible, .shot-result .shot-evidence .ev-contradicted').count()).toBeLessThanOrEqual(3);
  });

  test('processing page shows one action, a metrics trio, and collapsed panels', async ({ page }) => {
    await page.goto('/progress');
    // The fixture seeds pending (not failed-state) jobs, so the run action is
    // the single visible one — exactly one of run/resume/retry is shown.
    await expect(page.locator('#run')).toBeVisible();
    await expect(page.locator('#retry')).toBeHidden();
    await expect(page.locator('#resume')).toBeHidden();
    // Metrics render exactly three cells (pending/running/failed).
    await expect(page.locator('#metrics .metric')).toHaveCount(3);
    // Activity + Supervisor are collapsed <details class="panel"> folds.
    await expect(page.locator('#log-panel')).toHaveCount(1);
    await expect(page.locator('#supervisor-panel')).toHaveCount(1);
    await expect(page.locator('#log-panel summary')).toContainText('本次操作');
  });

  test('providers page leads with the beginner wizard and drops quick-config', async ({ page }) => {
    await login(page);
    await page.goto('/providers');
    await expect(page.locator('#add-provider-dialog')).toHaveCount(1);
    await expect(page.locator('#advanced-btn')).toHaveCount(1);
    await expect(page.locator('#quick-dialog')).toHaveCount(0);
    await expect(page.locator('body')).toContainText('添加服务');
    await expect(page.locator('body')).toContainText('高级路由');
    await expect(page.locator('.wizard-cap')).toHaveCount(0);
  });

  test('settings page groups with a sticky save bar and hidden off-peak rows', async ({ page }) => {
    await page.goto('/settings');
    await expect(page.locator('#read-rate')).toHaveValue('0'); // wait for the server snapshot
    await expect(page.locator('#sticky-save')).toBeHidden();
    await expect(page.locator('#off-peak')).not.toBeChecked();
    await expect(page.locator('#off-start')).toBeHidden();
    // Editing a control reveals the sticky save bar.
    await page.locator('#read-rate').fill('2');
    await expect(page.locator('#sticky-save')).toBeVisible();
    await expect(page.locator('#sticky-save')).toContainText('未保存的更改');
  });
});

test.describe('localization', () => {
  test('renders every route in every locale cookie with matching lang and selector', async ({ browser }) => {
    for (const locale of supportedLocales) {
      const context = await browser.newContext();
      await context.addCookies([{ name: 'nexusslate_locale', value: locale, url: fixtureBaseUrl }]);
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
      await context.addCookies([{ name: 'nexusslate_locale', value: c.locale, url: fixtureBaseUrl }]);
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
    const localeCookie = cookies.find((cookie) => cookie.name === 'nexusslate_locale');
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
test.describe('usability repair (2026-08-28 audit)', () => {
  test('providers wizard is capability-first with localized presets and a prefilled editable model', async ({ page }) => {
    await login(page);
    await page.goto('/providers');
    await page.locator('#new-channel-btn').click();
    await expect(page.locator('#add-provider-dialog')).toBeVisible();
    // Capability is chosen first; video_analysis is the default.
    await expect(page.locator('#wizard-capability')).toHaveValue('video_analysis');
    await expect(page.locator('#wizard-provider option')).toHaveCount(3);
    await expect(page.locator('#wizard-provider')).toContainText('Gemini');
    // The model is an editable input prefilled from the preset; the endpoint
    // preset is applied too.
    await expect(page.locator('#wizard-model')).toHaveValue('gemini-2.5-flash');
    await expect(page.locator('#wizard-endpoint')).toHaveValue('https://generativelanguage.googleapis.com/v1beta');
    // Switching to ASR swaps the grounded presets and prefills the ASR model.
    await page.locator('#wizard-capability').selectOption('asr');
    await expect(page.locator('#wizard-provider option')).toHaveCount(3);
    await page.locator('#wizard-provider').selectOption('volcengine_asr');
    await expect(page.locator('#wizard-model')).toHaveValue('doubao-seed-asr-2.0');
    await expect(page.locator('#wizard-endpoint')).toHaveValue('wss://openspeech.bytedance.com/api/v3/plan/sauc/bigmodel_nostream');
  });

  test('search POST carries semantic facets plus the asset context filter', async ({ page }) => {
    await page.goto('/');
    await page.locator('#status-filter').selectOption('ready');
    await page.locator('#date-from').fill('2026-01-01');
    await page.locator('#date-to').fill('2026-01-02');
    const requestPromise = page.waitForRequest((r) => r.url().includes('/api/v1/search/shots') && r.method() === 'POST');
    await page.locator('#q').fill('camera');
    await page.locator('#q').press('Enter');
    const request = await requestPromise;
    const body = JSON.parse(request.postData() ?? '{}');
    expect(body.asset_filter).toEqual({
      captured_from: '2026-01-01T00:00:00.000Z',
      captured_to: '2026-01-03T00:00:00.000Z', // date_to advanced one day (exclusive bound)
      region_label: '',
      camera_model: '',
      session_id: '',
      status: 'ready',
    });
    expect(body.facets).toBeDefined();
  });

  test('settings save shows a visible mutation callout', async ({ page }) => {
    await login(page);
    await page.goto('/settings');
    await expect(page.locator('#read-rate')).toHaveValue('0');
    await page.locator('#read-rate').fill('2');
    await page.locator('#save').click();
    await expect(page.locator('#status')).toHaveClass(/callout callout--confirmed/);
    await expect(page.locator('#status')).toContainText('已保存');
    // Restore the default so the shared fixture stays deterministic for the
    // tests that assert the untouched settings state.
    await page.locator('#read-rate').fill('0');
    await page.locator('#save').click();
    await expect(page.locator('#read-rate')).toHaveValue('0');
  });

  test('collections mutation shows a visible callout', async ({ page }) => {
    await login(page);
    // Create a scratch collection through the admin API so the seeded fixture
    // collection survives this destructive test.
    const created = await page.evaluate(async () => {
      const csrf = document.cookie.split('; ').find((c) => c.startsWith('__Host-nexusslate_csrf='));
      const token = csrf ? decodeURIComponent(csrf.slice('__Host-nexusslate_csrf='.length)) : '';
      const r = await fetch('/api/v1/collections', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': token },
        body: JSON.stringify({ name: 'scratch-callout' }),
      });
      return r.status === 201 ? await r.json() : null;
    });
    expect(created?.id).toBeTruthy();
    await page.goto('/collections');
    await expect(page.locator(`[data-coll="${created.id}"]`)).toBeVisible();
    page.on('dialog', (d) => d.accept());
    await page.locator(`[data-coll="${created.id}"] [data-action="delete-collection"]`).click();
    await expect(page.locator('#collections-status')).toHaveClass(/callout callout--confirmed/);
    await expect(page.locator('#collections-status')).toContainText('收藏已删除');
  });

  test('progress issue repair routes are category-specific', async ({ page }) => {
    await login(page);
    await page.goto('/progress');
    // The fixture seeds a failed job with last_error_code=configuration, whose
    // repair route is the providers page — never a blanket link.
    await expect(page.locator('#issues')).toBeVisible();
    const link = page.locator('#issues a.btn');
    await expect(link).toHaveAttribute('href', '/providers');
    await expect(link).toContainText('打开模型服务');
  });

  test('dialog surfaces close on Escape with focus restored to the opener', async ({ page }) => {
    await page.goto('/providers');
    // Admin dialog is a native <dialog>; Escape closes it. On narrow
    // viewports the trigger hides behind the collapsed mobile menu.
    const trigger = page.locator('#admin-trigger');
    if (!(await trigger.isVisible())) {
      const toggle = page.locator('#shell-menu-toggle');
      if (await toggle.isVisible()) await toggle.click();
    }
    await trigger.click();
    await expect(page.locator('#admin-dialog')).toBeVisible();
    await page.keyboard.press('Escape');
    await expect(page.locator('#admin-dialog')).not.toBeVisible();
    // Provider wizard opens from the new-channel button; Escape returns focus.
    await login(page);
    await page.goto('/providers');
    await page.locator('#new-channel-btn').click();
    await expect(page.locator('#add-provider-dialog')).toBeVisible();
    await page.keyboard.press('Escape');
    await expect(page.locator('#add-provider-dialog')).not.toBeVisible();
    await expect(page.locator('#new-channel-btn')).toBeFocused();
  });

  test('shot result cards open the drawer on Enter/Space and Escape closes it', async ({ page }) => {
    await page.goto('/');
    await page.locator('#q').fill('camera');
    await page.locator('#q').press('Enter');
    const card = page.locator('.shot-result').first();
    await expect(card).toBeVisible();
    await card.focus();
    await page.keyboard.press('Enter');
    await expect(page.locator('#shot-drawer')).toHaveClass(/open/);
    await page.keyboard.press('Escape');
    await expect(page.locator('#shot-drawer')).not.toHaveClass(/open/);
  });

  test('wide tables scroll inside .table-scroll without document overflow at 375x812', async ({ page }) => {
    await login(page);
    await page.setViewportSize({ width: 375, height: 812 });
    for (const route of ['/progress', '/library-roots', '/tags']) {
      await page.goto(route);
      await expect(page.locator('.table-scroll').first()).toBeAttached();
      const overflow = await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth + 1);
      expect(overflow, `${route} overflows horizontally`).toBe(false);
    }
  });
});

// The fixture serves one Hub configured with admin_auth "required" (see
// browserfixture.New). Production's default is "trusted_network", so the two
// waived branches of the v0.33 access UI — the Access status cell, the
// /providers and /workers callouts, and the hiding of the login control —
// had no browser coverage at all: every assertion above exercises the third
// branch only.
//
// These stub GET /api/v1/setup/status, which is the single place the shell
// reads adminAuthMode from, and patch only that field so the rest of the page
// behaves exactly as it does live. This covers the UI branch in a real
// browser; whether the server actually waives the credential is a different
// claim, pinned server-side by TestAdminAuthModeRootWriteGuard.
test.describe('admin-auth UI branches (audit U4-01)', () => {
  async function withAdminAuth(page: Page, mode: string) {
    await page.route('**/api/v1/setup/status', async (route) => {
      const response = await route.fetch();
      const body = await response.json();
      body.admin_auth = mode;
      await route.fulfill({ response, json: body });
    });
  }

  test('trusted_network hides the login control and warns on both admin pages', async ({ page }) => {
    await withAdminAuth(page, 'trusted_network');
    await page.goto('/');
    await expect(page.locator('#status-access')).toHaveText('内网免口令');
    await expect(page.locator('.status-cell', { has: page.locator('#status-access') }).locator('.dot')).toHaveClass(/warn/);
    // The password field is gone because no password is being asked for.
    // Assert the attribute rather than visibility: on the mobile project the
    // control also sits inside the collapsed menu, so toBeHidden() would pass
    // in every mode and prove nothing.
    await expect(page.locator('.shell-auth')).toHaveAttribute('hidden', '');

    for (const route of ['/providers', '/workers']) {
      await page.goto(route);
      const callout = page.locator('#admin-auth-callout');
      await expect(callout, route).toBeVisible();
      await expect(callout, route).toHaveClass(/callout--attention/);
      await expect(callout, route).toContainText('内网免口令');
    }
  });

  test('off states plainly that anyone reaching the port is an administrator', async ({ page }) => {
    await withAdminAuth(page, 'off');
    await page.goto('/');
    await expect(page.locator('#status-access')).toHaveText('完全开放');
    await expect(page.locator('.status-cell', { has: page.locator('#status-access') }).locator('.dot')).toHaveClass(/err/);
    await expect(page.locator('.shell-auth')).toHaveAttribute('hidden', '');

    for (const route of ['/providers', '/workers']) {
      await page.goto(route);
      const callout = page.locator('#admin-auth-callout');
      await expect(callout, route).toBeVisible();
      // "off" escalates the same callout from attention to contradicted and
      // replaces its text: the trusted_network copy would understate this.
      await expect(callout, route).toHaveClass(/callout--contradicted/);
      await expect(callout, route).toContainText('口令已关闭');
    }
  });

  test('required keeps the login control and the ok state (regression floor)', async ({ page }) => {
    await page.goto('/');
    await expect(page.locator('#status-access')).toHaveText('需口令');
    await expect(page.locator('.status-cell', { has: page.locator('#status-access') }).locator('.dot')).toHaveClass(/ok/);
    await expect(page.locator('.shell-auth')).not.toHaveAttribute('hidden', '');
    for (const route of ['/providers', '/workers']) {
      await page.goto(route);
      await expect(page.locator('#admin-auth-callout'), route).toBeHidden();
    }
  });
});

// The four P1s the 2026-08-29 audit filed against the first-use and daily
// flows. Each one is a claim about what a person sees, so each is asserted in
// a real browser rather than by grepping the page constant.
test.describe('usability repair (2026-08-29 audit, tranche 2)', () => {
  test('a shared search URL comes back as a search, not a browse listing (U3-01)', async ({ page }) => {
    const searchPost = page.waitForRequest((r) => r.url().includes('/api/v1/search/shots') && r.method() === 'POST');
    await page.goto('/?q=camera&status=ready');
    const body = JSON.parse((await searchPost).postData() ?? '{}');
    expect(body.query).toBe('camera');
    // The filters that produced the link must be in the request, not merely
    // restored into the controls after the fact.
    expect(body.asset_filter?.status).toBe('ready');
    await expect(page.locator('#q')).toHaveValue('camera');
    await expect(page.locator('#status-filter')).toHaveValue('ready');
    // ...and syncURL must not strip them back out of the address bar.
    expect(new URL(page.url()).searchParams.get('status')).toBe('ready');
  });

  test('more results than one page are reachable and honestly labelled (U3-02)', async ({ page }) => {
    const shot = (id: string) => ({
      shot_id: id, asset_id: 'pw-asset', filename: 'fixture.mp4',
      start_ms: 0, end_ms: 1000, description: id, score: 1, rank: 1,
    });
    const offsets: number[] = [];
    await page.route('**/api/v1/search/shots', async (route) => {
      const body = JSON.parse(route.request().postData() ?? '{}');
      const offset = body.offset ?? 0;
      offsets.push(offset);
      await route.fulfill({
        json: {
          query: { raw: body.query, intent: 'auto' }, search_id: 's', query_hash: 'h',
          results: [shot(`shot-${offset}-a`), shot(`shot-${offset}-b`)],
          offset, limit: body.limit, has_more: offset === 0,
          ...(offset === 0 ? { next_offset: 40 } : {}),
          window_exhausted: offset !== 0,
        },
      });
    });
    await page.goto('/');
    await page.locator('#q').fill('camera');
    await page.locator('#q').press('Enter');
    await expect(page.locator('.shot-result')).toHaveCount(2);
    const more = page.locator('#shot-more-btn');
    await expect(more).toBeVisible();

    await more.click();
    // The second page is appended, not swapped in.
    await expect(page.locator('.shot-result')).toHaveCount(4);
    expect(offsets).toEqual([0, 40]);
    await expect(more).toHaveCount(0);
    // window_exhausted is a different sentence from "that is everything".
    await expect(page.locator('.shot-results-foot')).toContainText('检索窗口上限');
    await expect(page.locator('.shot-results-foot')).not.toContainText('已经是全部结果');
  });

  test('/library-roots offers a way in when the Hub answers 401 (U2-01)', async ({ page }) => {
    await page.goto('/library-roots');
    const notice = page.locator('#root-health-wrap .callout');
    await expect(notice).toBeVisible();
    await expect(notice).toContainText('需要 Hub 管理口令');
    // The login control is reachable from the failure itself, and it opens the
    // shared dialog rather than sending the reader off to find it.
    await notice.getByRole('button', { name: '登录' }).click();
    await expect(page.locator('#admin-dialog')).toBeVisible();
    await expect(page.locator('#admin-token')).toBeVisible();
  });

  test('/setup names each failed check and what to do about it (U2-02)', async ({ page }) => {
    // Stub the whole snapshot rather than patching a live one: the count in
    // the status line must be exactly the number of failing checks, and the
    // host running the fixture has its own environment.
    await page.route('**/api/v1/setup/status', async (route) => {
      await route.fulfill({
        json: {
          ffmpeg: false, ffprobe: true, exiftool: true,
          data_dir_writable: true, cache_writable: true,
          free_disk_bytes: 8 * 1024 * 1024 * 1024, free_disk_ok: true,
          db_healthy: false,
          root_count: 1, healthy_root_count: 1, provider_count: 1, provider_ready: true,
          asset_count: 1, searchable_shot_count: 1, search_index_ready: true,
          next_step: 'search', ready: false, admin_auth: 'required',
        },
      });
    });
    await page.goto('/setup');
    const fixes = page.locator('#env-fixes');
    await expect(fixes).toBeVisible();
    await expect(fixes).toContainText('FFmpeg');
    await expect(fixes).toContainText('PATH');
    await expect(fixes).toContainText('数据库健康');
    await expect(fixes).toContainText('nexusslate doctor');
    // A check that passed must not be listed as needing work.
    await expect(fixes).not.toContainText('FFprobe');
    // ...and the status line must stop claiming success.
    await expect(page.locator('#setup-status')).toHaveClass(/status bad/);
    await expect(page.locator('#setup-status')).toContainText('2 项检查需要处理');
  });
});

// Found while doing the accessibility pass on /library-roots, not by the
// original audit: goStep() toggled .active on .step and nothing anywhere ever
// declared .step{display:none}, so the four-step wizard rendered all four
// panels at once. A first-time visitor met an empty mount-point field, an
// empty verify result and a scan panel before typing anything, under a
// numbered nav describing a sequence that was not happening.
test.describe('library-roots wizard (audit U2-05 + the step-visibility defect it surfaced)', () => {
  test('shows exactly one step at a time and moves aria-current with it', async ({ page }) => {
    await page.goto('/library-roots');
    await expect(page.locator('#step-1')).toBeVisible();
    for (const id of ['#step-2', '#step-3', '#step-4']) {
      await expect(page.locator(id), id).toBeHidden();
    }
    await expect(page.locator('.steps span[aria-current="step"]')).toHaveCount(1);
    await expect(page.locator('.steps span[data-step="1"]')).toHaveAttribute('aria-current', 'step');

    // Drive the wizard forward through its own entry point rather than by
    // calling goStep directly, so the assertion covers the real transition.
    await page.evaluate(() => (window as unknown as { goStep(n: number): void }).goStep(3));
    await expect(page.locator('#step-3')).toBeVisible();
    for (const id of ['#step-1', '#step-2', '#step-4']) {
      await expect(page.locator(id), id).toBeHidden();
    }
    await expect(page.locator('.steps span[aria-current="step"]')).toHaveCount(1);
    await expect(page.locator('.steps span[data-step="3"]')).toHaveAttribute('aria-current', 'step');
    // Steps already passed read as done, not as pending.
    await expect(page.locator('.steps span[data-step="1"]')).toHaveClass('is-done');
  });

  test('announces every asynchronous outcome (U2-05)', async ({ page }) => {
    await page.goto('/library-roots');
    // Each container the wizard writes an outcome into must be a live region;
    // without this the whole feedback loop is silent to a screen reader.
    for (const id of ['#root-health-wrap', '#discover-status', '#discover-results', '#step1-status', '#verify-result', '#added-summary', '#scan-result']) {
      await expect(page.locator(id), id).toHaveAttribute('aria-live', 'polite');
      await expect(page.locator(id), id).toHaveAttribute('role', 'status');
    }
    // The wizard nav is a landmark and each panel is a labelled group.
    await expect(page.locator('nav#step-nav')).toHaveAttribute('aria-label', /.+/);
    for (const id of ['#step-1', '#step-2', '#step-3', '#step-4']) {
      await expect(page.locator(id), id).toHaveAttribute('role', 'group');
      await expect(page.locator(id), id).toHaveAttribute('aria-label', /.+/);
    }
  });
});

// U4-03: the same aria-current defect the roots wizard had, on the wizard that
// hands out a one-time pairing token. .step-panel already had display:none, so
// only the announcement was wrong here — the visual sequence was fine.
test.describe('worker-setup wizard accessibility (audit U4-03)', () => {
  test('moves aria-current with the step and marks passed steps done', async ({ page }) => {
    await page.goto('/worker-setup');
    await expect(page.locator('#step-1')).toBeVisible();
    await expect(page.locator('.steps span[aria-current="step"]')).toHaveCount(1);
    await expect(page.locator('.steps span[data-step="1"]')).toHaveAttribute('aria-current', 'step');

    await page.evaluate(() => (window as unknown as { goStep(n: number): void }).goStep(3));
    await expect(page.locator('#step-3')).toBeVisible();
    for (const id of ['#step-1', '#step-2', '#step-4']) {
      await expect(page.locator(id), id).toBeHidden();
    }
    await expect(page.locator('.steps span[aria-current="step"]')).toHaveCount(1);
    await expect(page.locator('.steps span[data-step="3"]')).toHaveAttribute('aria-current', 'step');
    await expect(page.locator('.steps span[data-step="1"]')).toHaveClass('is-done');
  });

  test('announces every asynchronous outcome', async ({ page }) => {
    await page.goto('/worker-setup');
    for (const id of ['#hub-info', '#binary-grid', '#mounts-panel', '#script-area']) {
      await expect(page.locator(id), id).toHaveAttribute('aria-live', 'polite');
      await expect(page.locator(id), id).toHaveAttribute('role', 'status');
    }
    await expect(page.locator('nav#step-nav')).toHaveAttribute('aria-label', /.+/);
    for (const id of ['#step-1', '#step-2', '#step-3', '#step-4']) {
      await expect(page.locator(id), id).toHaveAttribute('role', 'group');
      await expect(page.locator(id), id).toHaveAttribute('aria-label', /.+/);
    }
  });
});

// U3-04: "相似镜头" is a different code path from the search POST, and it
// carried none of the caller's filters — so a shot the operator had just
// filtered out of the result list could come straight back through the
// drawer. The request itself is the assertion here: what the endpoint does
// with the params is pinned in Go, but only a browser proves the page sends
// them, and that filterQuery()'s leading "&" became a "?".
test.describe('similar shots carry the active filters (audit U3-04)', () => {
  test('the /similar request carries the drawer-visible filters as a query string', async ({ page }) => {
    let similarURL = '';
    await page.route('**/api/v1/shots/*/similar*', async (route) => {
      similarURL = route.request().url();
      await route.fulfill({ json: [] });
    });
    // The search itself is stubbed so the card stays on screen whatever the
    // filter is; what this test is about is the request the drawer sends.
    await page.route('**/api/v1/search/shots', async (route) => {
      const body = JSON.parse(route.request().postData() ?? '{}');
      await route.fulfill({
        json: {
          query: { raw: body.query, intent: 'auto' }, search_id: 's', query_hash: 'h',
          results: [{
            shot_id: 'pw-shot', asset_id: 'pw-asset', filename: 'fixture.mp4',
            start_ms: 0, end_ms: 1000, description: 'stub', score: 1, rank: 1,
          }],
          offset: 0, limit: body.limit, has_more: false, window_exhausted: false,
        },
      });
    });
    await page.goto('/');
    await page.locator('#q').fill('camera');
    await page.locator('#q').press('Enter');
    await expect(page.locator('.shot-result')).toHaveCount(1);

    // Set filters the way an operator would, then close the drawer with
    // Escape rather than Apply — either re-runs the search now (U3-05).
    await page.locator('#filters-toggle').click();
    await page.locator('#date-from').fill('2024-01-02');
    await page.locator('#status-filter').selectOption('ready');
    await page.keyboard.press('Escape');
    await expect(page.locator('.shot-result')).toHaveCount(1);

    const card = page.locator('.shot-result').first();
    await card.click();
    await expect(page.locator('#shot-drawer')).toHaveClass(/open/);
    await page.locator('#shot-drawer').getByRole('button', { name: '相似镜头' }).click();

    await expect.poll(() => similarURL).not.toBe('');
    const url = new URL(similarURL);
    // The join is a "?" — filterQuery() hands back a leading "&" and this
    // endpoint has no query string of its own to append to.
    expect(url.search.startsWith('?')).toBe(true);
    expect(url.searchParams.get('date_from')).toBe('2024-01-02');
    expect(url.searchParams.get('status')).toBe('ready');
  });
});

test.describe('narrowing a filter refines the search instead of discarding it (audit U3-05)', () => {
  test('shot results survive a filter change and the new filter reaches the request', async ({ page }) => {
    const bodies: Array<Record<string, unknown>> = [];
    await page.route('**/api/v1/search/shots', async (route) => {
      const body = JSON.parse(route.request().postData() ?? '{}');
      bodies.push(body);
      await route.fulfill({
        json: {
          query: { raw: body.query, intent: 'auto' }, search_id: 's', query_hash: 'h',
          results: [{
            shot_id: 'kept', asset_id: 'pw-asset', filename: 'fixture.mp4',
            start_ms: 0, end_ms: 1000, description: 'kept', score: 1, rank: 1,
          }],
          offset: 0, limit: body.limit, has_more: false, window_exhausted: false,
        },
      });
    });
    await page.goto('/');
    await page.locator('#q').fill('camera');
    await page.locator('#q').press('Enter');
    await expect(page.locator('.shot-result')).toHaveCount(1);

    await page.locator('#filters-toggle').click();
    await page.locator('#status-filter').selectOption('ready');
    await page.keyboard.press('Escape');

    // The search re-ran rather than being replaced by the browse listing…
    await expect.poll(() => bodies.length).toBeGreaterThan(1);
    await expect(page.locator('.shot-result')).toHaveCount(1);
    // …and it re-ran with the filter that was just set.
    const last = bodies[bodies.length - 1] as { asset_filter?: { status?: string } };
    expect(last.asset_filter?.status).toBe('ready');
  });
});
