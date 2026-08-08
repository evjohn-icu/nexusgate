# Security Policy

Re:Footage (`timingdex`) holds third-party provider API keys in an encrypted
local store, serves an administrator-token-protected HTTP API, and pairs remote
Worker nodes over TLS with certificate fingerprint pinning. Those three things
are where the interesting bugs are, so this document is specific about them
rather than generic.

## Reporting a vulnerability

Report privately through **GitHub private security advisories** on this
repository:

<https://github.com/evjohn-icu/timingdex/security/advisories/new>

(Repository page → **Security** → **Advisories** → **Report a vulnerability**.)

Please do not open a public issue, a pull request, or a discussion thread for
anything that leaks a credential or bypasses one of the boundaries below.

A useful report contains:

- the version or commit you tested (`git rev-parse HEAD` — alpha tags exist,
  the latest is the best reference, but main moves fast);
- how the Hub was reached: loopback, LAN, Docker published port, reverse proxy,
  Tailscale-style overlay;
- whether `hub_security.trusted_read_networks` or
  `hub_security.allow_worker_provider_credentials` were changed from their
  defaults;
- the concrete artefact, if a secret escaped — the API response body, the log
  line, the `jobs.last_error_message` row, the DOM node. **Redact the key
  itself**; the surrounding shape is what matters.

## Security-release process (alpha)

The project is an alpha / technical preview. Tags exist but there is no
supported-version table, no backport branch, and no advertised response or fix
SLA — promising one before a stable release would be fiction. What happens
today is: reports are read, valid ones are fixed on `main`, and the fix is
described in `CHANGELOG.md` in terms of the boundary it restores. When the
project reaches a stable release, this section will be replaced with a real
policy.

Reporters are credited in the advisory unless they ask not to be.

## Deployment model this policy assumes

Timingdex is **local-first**: one trusted machine on a trusted LAN, running the
Hub, optionally with paired Workers on other machines on the same network. It is
not designed or hardened to be a multi-tenant service or to be published to the
open internet, and it is not a privilege boundary between users of the Hub host.

That assumption is what makes the scope lists below meaningful.

## In scope

Anything that breaks one of these, as a remote or unprivileged party:

**Provider key confinement.** A provider API key reaching *anywhere* other than
the Hub-only encrypted store is a vulnerability, no matter how narrow the path:

- an API response body or header;
- a log line, on any log level;
- `jobs.last_error_message` or any other SQLite column — including via an
  upstream error body echoed back through `common.ReadError`;
- a browser page, DOM node, `localStorage`/`sessionStorage`, or a URL;
- Worker configuration on disk, or a Worker-visible response outside the
  explicit direct-mode opt-in;
- a `String()` / `MarshalJSON()` / `%v` rendering of a config or channel struct;
- the generated Worker install script, or any other exported artefact.

**Authentication and authorization bypass.**

- Reaching a mutating or administrative route without the administrator token,
  or defeating the constant-time comparison (timing oracle, prefix match,
  header-parsing quirk).
- Using the **agent token** outside the two routes it is accepted on (create and
  revise a *draft* Repurpose plan). Approving a plan, running the pipeline,
  reading provider configuration or reading raw media paths with the agent token
  is a vulnerability, not a feature request.
- Reaching library read routes from a network outside the trusted-read set —
  including via `X-Forwarded-For` or any other forwarded header, which the Hub
  deliberately ignores, or via Docker's NAT sitting in front of the read guard
  in a way the guard cannot see.
- Escaping the read guard's default set (loopback, RFC1918, link-local, IPv6
  unique-local, CGNAT) through address parsing or normalisation tricks.

**Worker trust.**

- Reusing a one-time pairing token, or minting/forging a node token.
- Defeating TLS certificate fingerprint pinning, or getting a Worker to accept a
  different Hub identity.
- Getting a lease-bound provider credential without holding the matching job
  lease, for an undeclared operation, after lease expiry, or while
  `allow_worker_provider_credentials` is at its default (deny).
- Turning the Hub JSON proxy into a media relay: getting media, multipart, or
  bodies over 2 MiB through it, or using it to reach an endpoint the leasing job
  did not declare.
- A `LeaseAudit` record that contains key material.

**The model-output boundary.** Model output must never write directly to
canonical tables. A provider response — including a hostile or prompt-injected
one — that reaches `asset_analysis`, the canonical tag catalog, shots, or the
FTS5 index without passing validation and the transactional commit is in scope.
So is an agent-authored plan that gets approved without a human action, or one
that invents asset IDs, shot IDs or timestamps that survive into a saved
revision.

**Path and media handling.**

- Any write to, or beside, an original source file. Originals are read-only; a
  sidecar, a metadata write-back, or an in-place re-encode is a serious bug.
- Path traversal through a library root, an asset/shot ID, a cache path, a
  Worker artifact upload, or a served proxy/thumbnail, reaching a file outside
  the configured root or the data directory.
- Escaping the argument boundary into an FFmpeg/ffprobe/ExifTool invocation, or
  into the optional forced-alignment external command, from filename or metadata
  content.

**Local state permissions.** `admin-token` and `agent-token` not being `0600`,
`provider-secrets/` not being `0700` with `0600` members, or the data directory
being created world-readable.

**Capture-location precision leaking.** Source coordinates appearing outside the
administrator-only per-asset route — in the ordinary asset/shot payloads, in
search results, in a proxy or thumbnail response, or in exported EXIF.

**Injection and web bugs in the browser UI**, since the UI holds a pasted
administrator token in page memory: stored XSS from filenames, tags, transcripts
or model output; CSRF against a mutating route; SQL injection anywhere in
`internal/repository/sqlite`.

## Out of scope

None of these are treated as vulnerabilities, because the deployment model is a
single trusted machine:

- **Anyone who already has a shell, or the ability to run code, on the Hub
  host.** They can read `admin-token`, read `agent-token`, read
  `provider-secrets/store.key`, and read the SQLite database. The store protects
  keys at rest and against the *application's own* output paths, not against
  local root or against the account that owns the data directory.
- Reading `$TIMINGDEX_DATA_DIR` from a backup, a snapshot, a synced folder, or a
  disk image the operator chose to create. Encrypting backups is the operator's
  job.
- **Publishing the Hub to the internet** and then reporting what that exposes.
  Removing the read guard, replacing `trusted_read_networks` with `0.0.0.0/0`,
  or publishing the container port to a public interface are documented as
  operator decisions with documented consequences.
- Trusting a reverse proxy's forwarded headers. The Hub ignores them on purpose,
  because they are attacker-controlled on a directly exposed listener; a
  reverse-proxied deployment has to do that filtering itself.
- The self-signed TLS certificate being untrusted by browsers and CLI tools.
  Worker trust comes from fingerprint pinning, not from a public CA.
- **Direct mode exposing a key to Worker memory.** With
  `hub_security.allow_worker_provider_credentials` explicitly enabled, the
  Worker's process memory holds a long-lived third-party key for the life of the
  lease. That is the stated cost of the opt-in; the five-minute lease is a
  delivery and audit window, not upstream key revocation. A compromised Worker
  that was *granted* a credential reading that credential is not a finding.
- Denial of service by a party who already holds the administrator token, or by
  filling the disk, or by queueing an unbounded scan. The pipeline leases one
  job at a time; throughput limits are a settings concern, not a security one.
- Cost or quota exhaustion at a provider caused by an operator's own
  configuration (retry budget, fallback chain, library size).
- Vulnerabilities in `ffmpeg`, `ffprobe`, `exiftool`, or a local VLM/aligner you
  configured — report those upstream. A bug in *how Timingdex invokes them* is
  in scope (see path handling above).
- Missing hardening headers, missing rate limits, or scanner output with no
  demonstrated impact on one of the boundaries above.
- Anything requiring a malicious build of the binary, or a modified
  `config.json` the attacker had to already be able to write.

## Invariants that are intentional

Read these before filing — several look like bugs and are not. They are the
design, and changing them is a design discussion, not a security fix.

- **Provider keys are stored in `secretstore` whenever possible.** Provider
  channel keys are encrypted in `provider-secrets/` at `0700` with `0600` files
  and never appear in SQLite, API responses, browser storage, Worker config,
  logs, error strings or serialisation output. **Legacy `providers.*` blocks in
  `config.json` hold keys in plaintext** alongside other configuration; these
  keys are read directly into in-memory `Credential` structs at issue time and
  are never persisted by the application — but they exist on disk in `config.json`
  until the operator migrates them to provider channels. If you find a path that
  leaks a key from either source, that *is* the report.
- **`provider-secrets/store.key` is deliberately not derived from the
  administrator token.** It is its own key file. Deriving it from the token made
  token rotation brick every CLI command, so the derivation was removed on
  purpose. `Open` still accepts the admin token solely to migrate a pre-
  `store.key` store on first run (backing the original up to
  `.pre-key-migration`). Proposing to re-derive the data key from the token is
  not a hardening improvement here.
- **`common.ReadError` truncates the upstream body it embeds.** That text reaches
  `jobs.last_error_message` and the `/progress` page, so the bound is what stops
  a relay echoing a request back and persisting a key. The truncation is
  load-bearing; removing it is the bug.
- **Workers get a provider credential only under an explicit opt-in that
  defaults to deny.** Without `hub_security.allow_worker_provider_credentials`,
  a Worker uses the Hub JSON proxy, which rejects media, audio, images,
  multipart and bodies over 2 MiB. Audit records must stay key-free.
- **Library read routes carry no token by design**, so the UI works without one.
  They are restricted by source network instead, and a valid token is admitted
  from any network.
- **Capture coordinates are exposed as a region label.** Source precision is
  administrator-only, per asset, via
  `GET /api/v1/admin/assets/{id}/capture-location`. The coarse label is
  intentional, not an incomplete redaction.
- **Original media is opened read-only** and nothing is ever written next to a
  source file; NAS mode copies into `cache/sources/` first. Derived artifacts
  live only under the data directory.
- **Agents draft, humans approve.** Plan approval, pipeline execution, provider
  keys and raw media paths are outside the agent token's allowlist, and that is
  enforced by the routes, not by prompt text.
- **New write endpoints default to gated.** If you find one that is not wrapped
  in the administrator check, that is a vulnerability — the default is the
  invariant.

## Hardening notes for operators

- Keep the Hub on a trusted LAN or a private overlay. If you must reach it
  remotely, use a VPN or Tailscale-style overlay rather than a forwarded port.
- Keep `$TIMINGDEX_DATA_DIR` on a filesystem that honours Unix permissions, and
  encrypt any backup of it — it contains both tokens and the secret store's key.
- Treat the generated Worker install script as a credential: it embeds a
  single-use pairing token. Do not commit it.
- Use the administrator token only for Hub management. Never put it in Worker
  configuration or browser storage.
- Revoke a node token when a Worker is retired; enrolment is not the only
  lifecycle step.
