import { expect, test, type Page, type Route } from '@playwright/test';

// Partition 18 browser workflows: required-auth reference upload with CSRF,
// browse/saved-view pagination, timeline continuation and invalidation. The
// stable Provider member editor case lives in smoke.spec.ts (partition 10).
const adminToken = 'pw-admin-fixture';
const fixtureBaseUrl = process.env.NEXUSGATE_PLAYWRIGHT_BASE_URL ?? 'https://127.0.0.1:4173';

// Valid 1x1 PNGs generated per upload in a distinct color: small enough for
// MaxSearchReferenceBytes, decodable by CreateSearchReference, and — because
// the Hub deduplicates references by normalized content hash — each upload
// carries different pixels so its search really starts pending instead of
// reusing a reference a previous test (or project) already encoded.
import zlib from 'node:zlib';

const crcTable = (() => {
  const table = new Uint32Array(256);
  for (let n = 0; n < 256; n++) {
    let c = n;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    table[n] = c >>> 0;
  }
  return table;
})();

function crc32(buf: Buffer): number {
  let c = 0xffffffff;
  for (const byte of buf) c = crcTable[(c ^ byte) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
}

function pngChunk(type: string, data: Buffer): Buffer {
  const head = Buffer.alloc(4);
  head.writeUInt32BE(data.length, 0);
  const body = Buffer.concat([Buffer.from(type, 'ascii'), data]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(body), 0);
  return Buffer.concat([head, body, crc]);
}

let probePngIndex = 0;
function nextProbePng(): Buffer {
  // Unique across worker processes and projects: both share one fixture
  // server, and identical pixels deduplicate to an instantly ready search.
  const seed = Math.floor(Math.random() * 0x1000000) + probePngIndex++;
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(1, 0);
  ihdr.writeUInt32BE(1, 4);
  ihdr[8] = 8;
  ihdr[9] = 2;
  const raw = Buffer.from([0, seed & 0xff, (seed >> 3) & 0xff, (seed >> 6) & 0xff]);
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    pngChunk('IHDR', ihdr),
    pngChunk('IDAT', zlib.deflateSync(raw)),
    pngChunk('IEND', Buffer.alloc(0)),
  ]);
}

const zh = {
  loginRequiredTop: '需要 Hub 管理口令：请先在顶部登录',
  imageSearchUploading: '正在上传参考图…',
  imageSearchPending: '参考图仍在编码，稍后自动重试',
  imageSearchReady: '已按参考图检索',
  imageSearchError: '以图搜索失败：',
  noMatch: '没有找到匹配的镜头',
  http403: '没有权限（HTTP 403）',
  browseLoadError: '加载更多素材失败',
  loadMore: '加载更多',
  timelinePartial: '时间轴仍有部分内容未加载',
  timelinePartialStalled: '时间轴的剩余部分加载失败',
  timelineEmpty: '此素材没有可显示的时间数据',
};

async function login(page: Page) {
  await page.goto('/');
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

// browseReady waits for the initial browse page (300 cards) to render.
async function browseReady(page: Page) {
  await expect(page.locator('[data-asset-timeline="asset-card-0300"]')).toBeVisible();
  await expect(page.locator('[data-browse-foot]')).toBeVisible();
}

test.describe('required-auth reference upload (partition 18)', () => {
  test('uploads a fixture image with CSRF and advances into the pending-to-ready search', async ({ page }) => {
    await login(page);
    await page.goto('/');
    await browseReady(page);

    const searchRequests: string[] = [];
    const uploadPromise = page.waitForResponse(
      (response) => response.url().includes('/api/v1/admin/search-references') && response.request().method() === 'POST',
    );
    const firstSearchPromise = page.waitForResponse(
      (response) => response.url().includes('/api/v1/search/shots') && response.request().method() === 'POST',
    );
    page.on('request', (request) => {
      if (request.url().includes('/api/v1/search/shots') && request.method() === 'POST') {
        searchRequests.push(request.postData() ?? '');
      }
    });
    await page.setInputFiles('#image-search', { name: 'probe.png', mimeType: 'image/png', buffer: nextProbePng() });

    const upload = await uploadPromise;
    expect(upload.status()).toBe(201);
    expect(upload.request().headers()['x-csrf-token']).toBeTruthy();
    expect(upload.request().headers()['content-type']).toBe('image/png');
    const reference = (await upload.json()) as { id?: string };
    expect(reference.id).toBeTruthy();

    // The search continues into the existing pending-to-ready flow: the first
    // request carries the reference id and the visual modality and answers
    // 202 pending while the fixture encoder finishes.
    const firstSearch = await firstSearchPromise;
    expect(firstSearch.status()).toBe(202);
    expect(JSON.parse(searchRequests[0])).toMatchObject({ modalities: ['visual'], reference_id: reference.id });
    await expect(page.locator('#image-search-status')).toHaveText(zh.imageSearchPending, { timeout: 5000 });

    // The fixture encodes the reference within a second; the page's poll then
    // renders the shot-result view. No fixture asset carries a visual vector,
    // so the honest result page is the no-match state.
    await expect(page.locator('#image-search-status')).toHaveText(zh.imageSearchReady, { timeout: 20000 });
    await expect(page.locator('#library')).toContainText(zh.noMatch);
    await expect(page.locator('[data-asset-timeline="asset-card-0300"]')).toHaveCount(0);
  });

  test('shows the login-required control on 401 and retries with the selected file after sign-in', async ({ page }) => {
    await page.goto('/');
    await expect(page.locator('[data-library-browser]')).toBeVisible();
    await expect(page.locator('[data-asset-timeline="asset-card-0300"]')).toBeVisible({ timeout: 20000 });

    const uploads: string[] = [];
    page.on('request', (request) => {
      if (request.url().includes('/api/v1/admin/search-references') && request.method() === 'POST') uploads.push(request.url());
    });
    const firstUpload = page.waitForResponse(
      (response) => response.url().includes('/api/v1/admin/search-references') && response.request().method() === 'POST',
    );
    await page.setInputFiles('#image-search', { name: 'probe.png', mimeType: 'image/png', buffer: nextProbePng() });
    const response = await firstUpload;
    expect(response.status()).toBe(401);

    // The existing login-required control: the wording plus the shared admin
    // dialog, with a retry control bound to the still-selected file. The
    // dialog opens on every viewport; on narrow ones it sits inside the
    // collapsed sidebar menu, so dismiss it, open the menu and sign in the
    // way the shell's own control works.
    await expect(page.locator('#image-search-status')).toContainText(zh.loginRequiredTop);
    await expect(page.locator('#image-search-status button', { hasText: '重试' })).toBeVisible();
    await page.keyboard.press('Escape');
    const trigger = page.locator('#admin-trigger');
    if (!(await trigger.isVisible())) {
      await page.locator('#shell-menu-toggle').click();
    }
    await trigger.click();
    await expect(page.locator('#admin-token')).toBeVisible();
    await page.locator('#admin-token').fill(adminToken);
    const retryUpload = page.waitForResponse(
      (candidate) => candidate.url().includes('/api/v1/admin/search-references') && candidate.request().method() === 'POST',
    );
    await page.locator('#admin-login').click();
    const retried = await retryUpload;
    expect(retried.status()).toBe(201);
    expect(retried.request().headers()['x-csrf-token']).toBeTruthy();
    expect(uploads).toHaveLength(2);
    await expect(page.locator('#image-search-status')).toHaveText(zh.imageSearchReady, { timeout: 20000 });
  });

  test('shows the authorization wording on 403 and never re-uploads', async ({ page }) => {
    await login(page);
    await page.goto('/');
    await browseReady(page);
    await page.route('**/api/v1/admin/search-references', (route: Route) =>
      route.fulfill({ status: 403, contentType: 'application/json', body: JSON.stringify({ error: { code: 'forbidden', message: 'denied' } }) }),
    );
    const uploads: string[] = [];
    page.on('request', (request) => {
      if (request.url().includes('/api/v1/admin/search-references') && request.method() === 'POST') uploads.push(request.url());
    });
    await page.setInputFiles('#image-search', { name: 'probe.png', mimeType: 'image/png', buffer: nextProbePng() });
    await expect(page.locator('#image-search-status')).toHaveText(zh.http403);
    await page.waitForTimeout(2500);
    expect(uploads).toHaveLength(1);
  });

  test('shows the upload error on a non-auth failure and never re-uploads', async ({ page }) => {
    await login(page);
    await page.goto('/');
    await browseReady(page);
    await page.route('**/api/v1/admin/search-references', (route: Route) =>
      route.fulfill({ status: 413, contentType: 'application/json', body: JSON.stringify({ error: { code: 'invalid_request', message: 'too large' } }) }),
    );
    const uploads: string[] = [];
    page.on('request', (request) => {
      if (request.url().includes('/api/v1/admin/search-references') && request.method() === 'POST') uploads.push(request.url());
    });
    await page.setInputFiles('#image-search', { name: 'probe.png', mimeType: 'image/png', buffer: nextProbePng() });
    await expect(page.locator('#image-search-status')).toContainText(zh.imageSearchError);
    await page.waitForTimeout(2500);
    expect(uploads).toHaveLength(1);
  });
});

test.describe('browse and saved-view pagination (partition 18)', () => {
  test('ordinary browse shows card 301 via Load more without replacing page 1', async ({ page }) => {
    await login(page);
    const browseUrls: string[] = [];
    page.on('request', (request) => {
      if (request.url().includes('/api/v1/assets?limit=300')) browseUrls.push(request.url());
    });
    await page.goto('/');
    await browseReady(page);
    await expect(page.locator('[data-asset-timeline="asset-card-0301"]')).toHaveCount(0);

    await page.locator('#browse-more-btn').click();
    await expect(page.locator('[data-asset-timeline="asset-card-0301"]')).toBeVisible();
    // Page 1 is retained, not replaced.
    await expect(page.locator('[data-asset-timeline="asset-card-0300"]')).toBeVisible();
    await expect(page.locator('#browse-more-btn')).toHaveCount(0);
    expect(browseUrls.filter((url) => url.includes('offset=0')).length).toBe(1);
    expect(browseUrls.filter((url) => url.includes('offset=300')).length).toBe(1);
  });

  test('saved-view collection pages its 301 cards with Load more', async ({ page }) => {
    await login(page);
    const viewUrls: string[] = [];
    page.on('request', (request) => {
      if (request.url().includes('/api/v1/collections/view-discovered/assets')) viewUrls.push(request.url());
    });
    await page.goto('/');
    await browseReady(page);
    await page.locator('#collection-filter').selectOption('view-discovered');
    await expect(page.locator('[data-asset-timeline="asset-card-0300"]')).toBeVisible();
    await expect(page.locator('#collection-filter')).toHaveValue('view-discovered');
    expect(viewUrls.filter((url) => url.includes('offset=0')).length).toBeGreaterThanOrEqual(1);
    expect(viewUrls.filter((url) => url.includes('offset=300')).length).toBe(0);

    await page.locator('#browse-more-btn').click();
    await expect(page.locator('[data-asset-timeline="asset-card-0301"]')).toBeVisible();
    await expect(page.locator('[data-asset-timeline="asset-card-0300"]')).toBeVisible();
    expect(await page.locator('[data-asset-timeline]').count()).toBe(301);
    expect(viewUrls.filter((url) => url.includes('offset=300')).length).toBe(1);
  });

  test('a failed continuation retains the current page and retries the same offset', async ({ page }) => {
    await login(page);
    await page.goto('/');
    await browseReady(page);
    const browseUrls: string[] = [];
    page.on('request', (request) => {
      if (request.url().includes('/api/v1/assets?limit=300')) browseUrls.push(request.url());
    });
    await page.route('**/api/v1/assets?limit=300&offset=300*', (route: Route) =>
      route.fulfill({ status: 500, contentType: 'application/json', body: JSON.stringify({ error: { code: 'internal_error', message: 'boom' } }) }),
    );
    await page.locator('#browse-more-btn').click();
    await expect(page.locator('[data-browse-error]')).toContainText(zh.browseLoadError);
    // Page 1 stays rendered and the retry control remains.
    await expect(page.locator('[data-asset-timeline="asset-card-0300"]')).toBeVisible();
    await expect(page.locator('#browse-more-btn')).toBeVisible();

    await page.unroute('**/api/v1/assets?limit=300&offset=300*');
    await page.locator('#browse-more-btn').click();
    await expect(page.locator('[data-asset-timeline="asset-card-0301"]')).toBeVisible();
    // The failed attempt never advanced the offset: only one continuation.
    expect(browseUrls.filter((url) => url.includes('offset=300')).length).toBe(2);
  });

  test('a late browse response is discarded after the view changes', async ({ page }) => {
    await login(page);
    await page.goto('/');
    await browseReady(page);
    await page.route('**/api/v1/assets?limit=300&offset=300*', async (route: Route) => {
      { const { promise, resolve } = Promise.withResolvers<void>(); setTimeout(resolve, 1500); await promise; }
      await route.continue();
    });
    await page.locator('#browse-more-btn').click();
    // Narrow the view while the continuation is still in flight; the fresh
    // load replaces the browse state and the late response must not render.
    await page.locator('#status-filter').selectOption('queued');
    await expect(page.locator('[data-asset-timeline="asset-fixture"]')).toBeVisible();
    await page.waitForTimeout(2200);
    await expect(page.locator('[data-asset-timeline="asset-card-0301"]')).toHaveCount(0);
    await expect(page.locator('#status-filter')).toHaveValue('queued');
  });
});

test.describe('timeline continuation and invalidation (partition 18)', () => {
  test('shows the partial state and loads the final word through Load more', async ({ page }) => {
    await login(page);
    const urls: string[] = [];
    page.on('request', (request) => urls.push(request.url()));
    await page.goto('/');
    await browseReady(page);
    await page.locator('[data-asset-timeline="asset-timeline-long"]').click();
    await expect(page.locator('#asset-timeline-panel')).toBeVisible();
    const transcript = page.locator('[data-tl-track="transcript"] [data-tl-interval]');
    await expect(transcript).toHaveCount(1000, { timeout: 20000 });
    // The words alone exceed the per-track page cap: the partial state and
    // its Load more control are visible while a cursor remains.
    await expect(page.locator('#asset-timeline-partial')).toContainText(zh.timelinePartial);
    await expect(page.locator('#asset-timeline-partial button')).toContainText(zh.loadMore);
    await expect(page.locator('[data-tl-track="transcript"] .tl-meta-label').filter({ hasText: 'word-1001' })).toHaveCount(0);

    await page.locator('#asset-timeline-partial button', { hasText: zh.loadMore }).click();
    await expect(transcript).toHaveCount(1001, { timeout: 20000 });
    await expect(page.locator('[data-tl-track="transcript"] .tl-meta-label').filter({ hasText: 'word-1001' })).toHaveCount(1);
    await expect(page.locator('#asset-timeline-partial')).toBeHidden();
    const fetched = urls.filter((url) => url.includes('/api/v1/assets/asset-timeline-long/timeline'));
    expect(fetched.some((url) => !url.includes('cursor='))).toBe(true);
    expect(fetched.some((url) => url.includes('cursor='))).toBe(true);
  });

  test('switching assets after a delayed load never renders obsolete results', async ({ page }) => {
    await login(page);
    await page.goto('/');
    await browseReady(page);
    await page.route('**/api/v1/assets/asset-timeline-long/timeline*', async (route: Route) => {
      { const { promise, resolve } = Promise.withResolvers<void>(); setTimeout(resolve, 1200); await promise; }
      await route.continue();
    });
    await page.locator('[data-asset-timeline="asset-timeline-long"]').click();
    await expect(page.locator('#asset-timeline-panel')).toBeVisible();

    // Close the stalled drawer and open another asset's timeline while the
    // first response is still in flight.
    await page.locator('.shot-drawer-close').click();
    await page.locator('[data-asset-timeline="asset-fixture"]').click();
    await expect(page.locator('#asset-timeline-panel')).toBeVisible();
    await expect(page.locator('#asset-timeline-empty')).toContainText(zh.timelineEmpty);
    await page.waitForTimeout(2000);
    // The late long-asset response must not render over the fixture asset.
    await expect(page.locator('#asset-timeline-empty')).toContainText(zh.timelineEmpty);
    await expect(page.locator('[data-tl-track="transcript"]')).toHaveCount(0);
  });

  test('a stalled continuation shows the retryable error instead of looping', async ({ page }) => {
    await login(page);
    await page.goto('/');
    await browseReady(page);
    let firstPageBody: unknown = null;
    // The local fixture answers in milliseconds, so the initial response is
    // awaited from before the click onward.
    const initialPromise = page.waitForResponse(
      (response) => response.url().includes('/api/v1/assets/asset-timeline-long/timeline') && !response.url().includes('cursor='),
    );
    await page.locator('[data-asset-timeline="asset-timeline-long"]').click();
    await expect(page.locator('#asset-timeline-panel')).toBeVisible();
    firstPageBody = await (await initialPromise).json();
    const transcript = page.locator('[data-tl-track="transcript"] [data-tl-interval]');
    await expect(transcript).toHaveCount(1000, { timeout: 20000 });

    // The continuation answers with the first page's own cursors: fetching
    // again would loop forever, so the page must stop and show a retryable
    // error with the partial rows intact.
    let continuationRequests = 0;
    await page.route('**/api/v1/assets/asset-timeline-long/timeline*cursor=*', async (route: Route) => {
      continuationRequests += 1;
      route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(firstPageBody) });
    });
    await page.locator('#asset-timeline-partial button', { hasText: zh.loadMore }).click();
    await expect(page.locator('#asset-timeline-partial')).toContainText(zh.timelinePartialStalled, { timeout: 20000 });
    await expect(page.locator('#asset-timeline-partial button')).toContainText('重试');
    await expect(transcript).toHaveCount(1000);
    await page.waitForTimeout(2500);
    expect(continuationRequests).toBe(1);
  });
});

test.describe('browse and search view ownership (partition 18 review)', () => {
  test('an empty browse result is an empty state, not an error', async ({ page }) => {
    await page.goto('/?region=no-such-region');
    await expect(page.locator('#library .empty')).toBeVisible();
    await expect(page.locator('#library .empty.error')).toHaveCount(0);
  });

  test('Back to browse leaves the search and lists assets again', async ({ page }) => {
    await page.goto('/');
    await page.locator('#q').fill('camera');
    await page.locator('#q').press('Enter');
    await expect(page.locator('.shot-result').first()).toBeVisible();
    await page.locator('.shot-results-head .link-button').click();
    await expect(page.locator('[data-asset-timeline]').first()).toBeVisible();
    await expect(page.locator('.shot-result')).toHaveCount(0);
    expect(await page.locator('#q').inputValue()).toBe('');
  });

  test('Back to browse discards a pending image search', async ({ page }) => {
    await login(page);
    await page.goto('/');
    await page.locator('#q').fill('camera');
    await page.locator('#q').press('Enter');
    await expect(page.locator('.shot-result').first()).toBeVisible();
    await page.route('**/api/v1/search/shots', async (route: Route) => {
      const { promise, resolve } = Promise.withResolvers<void>();
      setTimeout(resolve, 1200);
      await promise;
      await route.fulfill({
        json: {
          results: [{ shot_id: 'shot-late', asset_id: 'asset-fixture', filename: 'late-image.mp4', start_ms: 0, end_ms: 5000, description: 'fixture', score: 0.9 }],
          has_more: false,
        },
      });
    });
    const search = page.waitForRequest((request) => request.url().endsWith('/api/v1/search/shots'));
    await page.setInputFiles('#image-search', { name: 'late.png', mimeType: 'image/png', buffer: nextProbePng() });
    await search;
    await page.locator('.shot-results-head .link-button').click();
    await expect(page.locator('[data-asset-timeline]').first()).toBeVisible();
    await page.waitForTimeout(2200);
    await expect(page.locator('[data-asset-timeline]').first()).toBeVisible();
    await expect(page.locator('.shot-result')).toHaveCount(0);
  });

  test('an older image search cannot replace a newer selection', async ({ page }) => {
    await login(page);
    await page.goto('/');
    let searches = 0;
    await page.route('**/api/v1/search/shots', async (route: Route) => {
      searches += 1;
      const first = searches === 1;
      // The first reference's search answers slowly with a shot that would be
      // visibly wrong if it replaced the second reference's result.
      if (first) {
        const { promise, resolve } = Promise.withResolvers<void>();
        setTimeout(resolve, 1500);
        await promise;
      }
      await route.fulfill({
        json: {
          results: [{
            shot_id: first ? 'shot-old' : 'shot-new', asset_id: 'asset-fixture',
            filename: first ? 'older-image.mp4' : 'newer-image.mp4',
            start_ms: 0, end_ms: 5000, description: 'fixture', score: 0.9,
          }],
          has_more: false,
        },
      });
    });
    const input = page.locator('#image-search');
    const firstSearch = page.waitForRequest((request) => request.url().endsWith('/api/v1/search/shots'));
    await input.setInputFiles({ name: 'a.png', mimeType: 'image/png', buffer: nextProbePng() });
    await firstSearch;
    // Select B while A's search is still delayed: only the generation guard
    // keeps A's late response from replacing B's result.
    await input.setInputFiles({ name: 'b.png', mimeType: 'image/png', buffer: nextProbePng() });
    await expect(page.locator('.shot-result-file')).toHaveText('newer-image.mp4', { timeout: 10000 });
    await page.waitForTimeout(2000);
    await expect(page.locator('.shot-result-file')).toHaveText('newer-image.mp4');
    await expect(page.locator('.shot-result-file')).toHaveCount(1);
    await expect(page.locator('#image-search-status')).toContainText(zh.imageSearchReady);
  });
});
