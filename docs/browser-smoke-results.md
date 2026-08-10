# Browser Smoke Results: R3-30

This document records the consolidated browser round for the merged review/fix tree. The
suite is offline and uses the SQLite fixture; it does not call a real provider.

## Environment

- Node.js: 26.x (`v26.3.1`)
- Playwright: `1.60.0`
- Browser: cached Chromium `chromium-1223`; browsers were not reinstalled
- Fixture: offline SQLite fixture server
- Desktop viewport: `1280x900`
- Mobile viewport: `375x812`

## Viewport Matrix

| Suite | 1280x900 | 375x812 |
| --- | --- | --- |
| Desktop routes, library, progress, providers, collections, worker setup | 7/7 pass | not applicable |
| Worker setup wizard | 4/4 pass | not applicable |
| Mobile layout and interactions | not applicable | fixes in progress |
| Security/XSS and token memory | exercised at desktop | not applicable |
| Accessibility smoke | exercised at desktop | not applicable |

## Scenario Results

| Suite | Pass | Fail | Status |
| --- | ---: | ---: | --- |
| Desktop | 7 | 0 | accepted |
| Wizard | 4 | 0 | accepted |
| Mobile | 0 | 0 | fixes in progress |
| Security | 0 | 0 | fixes in progress |
| Accessibility | 0 | 0 | fixes in progress |
| **Accepted total** | **11** | **0** | **17 pending** |

The desktop and wizard counts are the consolidated peer results requested for this round.
The expected peer reports (`desktop-luna.md`, `wizard-luna.md`, and
`mobile-security-a11y-luna.md`) were polled for approximately ten minutes; they were not
available as durable artifacts at report time. Agent 23's fix commit was not present in
reachable history, so the remaining suites are recorded as **fixes in progress**, not as
accepted failures.

## Bugs and Ownership

The report worktree intentionally contains no product-code changes. Agent 23 owns the
remaining UI fixes; therefore there is no fix commit reference to report in this commit.
Observed residual areas requiring that follow-up are unlabeled advanced filters/provider
controls/worker mount controls and narrow-viewport horizontal overflow.

## Fixture and Spec-Only Failures

- Mobile and security specs hardcode port `4173`, while the shared configuration uses `8799`.
- The launcher points at a peer fixture worktree; a target-tree fixture must be rebuilt for
  results to represent this branch.
- Wizard fixture expectations assume a deterministic pairing token and a certificate
  fingerprint, while the current fixture generates a random token and runs with TLS disabled.
- Security storage assertions can execute before navigation establishes an origin, causing a
  browser `SecurityError` rather than a product failure.

## Skips and Residual Risks

- No real providers were called. No media-dependent scenario was accepted as evidence.
- CI spec-bundle provisioning remains a risk: all six spec files and the matching config must
  be present before execution.
- TLS-disabled fixture coverage cannot prove certificate-fingerprint rendering.
- Security-spec storage ordering must be corrected before token-memory results are final.
- Mobile, security, and accessibility results remain pending Agent 23's fix commit and
  follow-up browser evidence.
