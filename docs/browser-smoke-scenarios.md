# Browser Smoke Scenarios

The Playwright smoke suite runs against the offline SQLite fixture server. It never calls a real provider and keeps scripts/traces/screenshots outside the repository under `/tmp/timingdex-playwright`.

## Viewport Matrix

| Scenario | 1280x900 | 375x812 |
| --- | --- | --- |
| Shell and branding on `/`, `/setup`, `/progress`, `/providers`, `/collections`, `/worker-setup`, `/workers` | yes | yes |
| Empty browser storage and password token input | yes | yes |
| No page errors and hostile text stays escaped | yes | yes |
| Search POST includes `include_evidence`, drawer open/close | yes | yes |
| Collection add, reorder, remove | yes | yes |
| Provider channel load and key-write redaction | yes | yes |
| Progress counts and escaped failure text without token | yes | yes |
| Worker setup steps and pairing-token disappearance after reload | yes | yes |

Run the temporary server with `/tmp/timingdex-playwright/run-fixture.sh`, then run the scenarios with Playwright at both viewports. The scenario source is intentionally not committed.
