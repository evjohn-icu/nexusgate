# Browser Smoke Scenarios

The Playwright smoke suite runs against the offline SQLite fixture server. It never calls a real provider. The deterministic specs, Playwright config, and exact npm lockfile live in `testdata/browser-smoke/`; reports and traces are generated as CI artifacts only.

## Viewport Matrix

| Scenario | 1280x900 | 375x812 |
| --- | --- | --- |
| Shell and branding on `/`, `/setup`, `/progress`, `/providers`, `/collections`, `/worker-setup`, `/workers` | yes | yes |
| Secure HttpOnly admin session login and empty browser storage | yes | yes |
| CSRF rejection for a session mutation without the header | yes | yes |
| Hostile text stays escaped and pages do not overflow horizontally | yes | yes |
| Library search succeeds without credentials in request URLs | yes | yes |
| Seeded collection labels render as text, not executable markup | yes | yes |
| Provider channel load keeps secret references and tokens hidden | yes | yes |
| Progress failure details stay redacted from public page text | yes | yes |
| Worker setup reports TLS without exposing the admin token | yes | yes |

## Token Memory Assertions

Every page assertion verifies that the admin token is absent from both
`localStorage` and `sessionStorage`. Login establishes a Secure, HttpOnly,
SameSite=Strict session cookie and a separate CSRF cookie; the session value is
not the admin token. The suite also checks rendered text and request URLs for
the token, verifies that a mutation without the CSRF header is rejected, and
checks that provider secret references are not rendered. CLI, Agent, and
Worker Bearer authentication remains separate.

Run the fixture over HTTPS on `127.0.0.1:4173` with the `playwrightfixture` build tag. The fixture creates a self-signed certificate through `internal/hubtls`; local runs may use Playwright's `ignoreHTTPSErrors` setting from the checked-in config. From the repository root:

```bash
bash scripts/check-browser-fixture.sh
go build -tags playwrightfixture -o /tmp/nexusgate-playwright-fixture \
  ./cmd/nexusgate-playwright-fixture
NEXUSGATE_PLAYWRIGHT_ADDR=127.0.0.1:4173 \
  /tmp/nexusgate-playwright-fixture
```

In another shell, install the locked browser dependencies and run `npx --no-install playwright test` from `testdata/browser-smoke/`.

## CI

The `browser-smoke` CI job runs on pushes to `main`, pull requests, and manual dispatch. It
installs Chromium with its system dependencies from the exact Playwright lockfile, verifies
both tagged fixture entry points compile, starts the offline HTTPS server, and runs the
checked-in specs at desktop and mobile viewports. The job is blocking: a browser regression
must fail the workflow.

Failure artifacts are retained for three days. They are untrusted test output: never include
admin tokens, provider keys, or other credentials in screenshots, traces, reports, or fixture
logs. Login tests disable video recording and assert that credentials do not appear in browser
storage, rendered text, URLs, or request URLs.
