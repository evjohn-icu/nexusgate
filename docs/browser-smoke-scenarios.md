# Browser Smoke Scenarios

The Playwright smoke suite runs against the offline SQLite fixture server. It never calls a real provider and keeps scripts/traces/screenshots outside the repository under `/tmp/timingdex-playwright`.

## Viewport Matrix

| Scenario | 1280x900 | 375x812 |
| --- | --- | --- |
| Shell and branding on `/`, `/setup`, `/progress`, `/providers`, `/collections`, `/worker-setup`, `/workers` | yes | yes |
| HttpOnly admin session login/logout and empty browser storage | yes | yes |
| No page errors and hostile text stays escaped | yes | yes |
| Search POST includes `include_evidence`, drawer open/close | yes | yes |
| Collection add, reorder, remove | yes | yes |
| Provider channel load and key-write redaction | yes | yes |
| Progress counts and escaped failure text without token | yes | yes |
| Worker setup steps and pairing-token disappearance after reload | yes | yes |

## Token Memory Assertions

Every page assertion verifies that the admin token is absent from both `localStorage` and `sessionStorage`. The token input is used only to establish the HTTPS HttpOnly admin session and is cleared immediately after login. After a reload, the token input and any generated pairing token must be gone from page state; the suite does not accept a token restored from browser storage. The token, session value, and CSRF value must not appear in rendered HTML, DOM text, URLs, request URLs, or logs. Browser mutations use the HttpOnly session plus same-origin `Origin` and `X-CSRF-Token`; CLI, Agent, and Worker Bearer headers remain separate. Provider requests are inspected to ensure the key value is never sent or exposed by the browser.

Run the temporary server with `/tmp/timingdex-playwright/run-fixture.sh`, then run the scenarios with Playwright at both viewports. The scenario source is intentionally not committed.

## CI

The `browser-smoke` CI job runs on pushes to `main`, pull requests, and manual dispatch. It
installs Chromium with its system dependencies, verifies the tagged fixture compiles, starts
the offline server, and runs `/tmp/timingdex-playwright/specs/`. The `/tmp/timingdex-playwright`
spec bundle is an external CI input and must be provided before the job runs; it is not checked
into this repository. The job is intentionally
non-blocking for this release (`continue-on-error: true`) while the smoke signal is established.

Failure artifacts are retained for three days. They are untrusted test output: never include
admin tokens, provider keys, or other credentials in screenshots, traces, reports, or fixture
logs.
