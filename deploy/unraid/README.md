# Unraid deployment

Unraid does not run `docker-compose.yml` natively — Compose Manager is an
optional Community Applications plugin, and this deployment does not assume
it is installed. Instead there are two standalone Community Applications
templates here, mirroring the `hub` and `worker` services in
[`docker-compose.yml`](../../docker-compose.yml):

- `timingdex-hub.xml` — the database, HTTPS API and browser UI.
- `timingdex-worker.xml` — a paired node that only runs FFmpeg derive work.

Both reference the GPU image built by `docker build --target gpu` (see
[`Dockerfile`](../../Dockerfile)) and default `ExtraParams` to
`--device=/dev/dri:/dev/dri` for Intel/AMD VAAPI. Read
[`docs/v0.19-gpu-docker-unraid.md`](../../docs/v0.19-gpu-docker-unraid.md)
for the full picture (image tags, the NVIDIA path, the non-free/backports
caveat); this file is the Unraid-specific walkthrough.

## 1. Build (or obtain) the GPU image

This project does not publish images to a registry yet, so the exact tag the
templates reference (`timingdex:v0.31.0-alpha`) has to exist somewhere Unraid's
Docker can see. The simplest path is building it directly on the Unraid box,
which needs no registry at all — Unraid's Docker daemon will use a locally
tagged image instead of trying to pull it:

```sh
# From the Unraid terminal, with the repo checked out somewhere under /mnt/user:
cd /mnt/user/.../timingdex
docker build --target gpu -t timingdex:v0.31.0-alpha .
```

If you'd rather build elsewhere, push the same tag to a registry your Unraid
box can reach (Docker Hub, ghcr.io, or a LAN registry) and adjust
`<Repository>` in both templates accordingly before installing them.

## 2. Install the Hub template

In the Unraid UI: **Docker → Add Container → Template: (select) →** point it
at `timingdex-hub.xml` (via **Template repositories** if you host this repo's
`deploy/unraid/` directory, or by pasting the local file path / using
**Add Container → XML view** and pasting the file's contents directly).

Before starting it for the first time, fix the uid mismatch: Unraid creates
`/mnt/user/appdata/...` owned `root:root` or `nobody:users` (99:100), and
this image runs as a fixed unprivileged uid 10001 with no PUID/PGID
entrypoint wrapper (that's deliberate — see the Dockerfile's user-creation
comment; adding one would mean the image doing privileged setup steps at
startup).

```sh
mkdir -p /mnt/user/appdata/timingdex-hub
chown -R 10001:10001 /mnt/user/appdata/timingdex-hub
```

Point **素材库目录 Media library** at your footage and leave its access mode
on **Read Only slave** — see
[Mounting remote (SMB/NFS) shares](#mounting-remote-smbnfs-shares-with-live-remount)
below for why that mode is not optional.

**Admin auth must be set before first start.** The Hub runs in a container and
the template publishes port 8787 on the bridge network, so the container guard
`ValidateContainerAdminAuth` refuses to start with the default
`admin_auth=trusted_network` and no explicit `admin_auth_networks` — and the
templates ship no admin-auth setting, so a clean `timingdex-hub` appdata
directory will not boot (the container exits at startup). Add an environment
variable to the Hub template (**Docker → timingdex-hub → edit → add a
Variable**):

- `TIMINGDEX_HUB_ADMIN_AUTH` = `required` — every admin/mutating route then
  demands the `admin-token`; this is the recommended setting and the one the
  rest of this walkthrough assumes.

Alternatively keep `trusted_network` and supply explicit CIDRs via
`hub_security.admin_auth_networks` in a `config.json` inside the Hub data
directory — but understand Docker NAT first: behind a published port every
peer appears as the bridge gateway (an RFC1918 address), so the listed ranges
end up covering the whole bridge, trusting anything that can reach the port
from it. Don't work around the guard by disabling auth or by exposing the
port to the internet.

Start the container; it should now boot (check `docker logs timingdex-hub`
for the admin-auth refusal being gone). Read the generated `admin-token` from
the data directory and open `https://<unraid-ip>:8787/workers` to generate a
one-time pairing token and read the Hub's certificate fingerprint — both
templates' `Overview` fields walk through this in more detail.

If this Unraid box has no `/dev/dri` (no Intel/AMD iGPU — e.g. it's CPU-only,
or the GPU is an NVIDIA card going through a different path), delete
`--device=/dev/dri:/dev/dri` from **Extra Parameters** before starting the
container, or it will fail to start with a "no such device" error.

## Mounting remote (SMB/NFS) shares with live remount

Both templates ship the footage bind with access mode `ro,slave` — the
option Unraid's Access Mode control calls **Read Only slave**, which exists
in that dropdown for exactly this case. It is what makes the bind useful for
network shares:

1. **Mount the share with Unassigned Devices first.** If your footage lives
   on a separate NAS rather than an Unraid array/pool share, install the
   Unassigned Devices plugin and mount the SMB/NFS share through it. It
   lands under `/mnt/remotes/<server>_<share>` on the host.
2. **Point the media path at the `/mnt/remotes` parent, not one specific
   share.** Both templates already default to `/mnt/remotes` rather than
   `/mnt/remotes/<server>_<share>`. Timingdex still records whatever
   subpath you pass to `root add`/`root scan` inside the container, so
   nothing about the app's view of a given root changes — the only
   difference is that a share added *later* through Unassigned Devices
   appears inside the running container immediately, with no container
   recreate.
3. **Why propagation matters here specifically:** a bind mount defaults to
   private propagation, so a share mounted on the host *after* the container
   started is invisible inside it forever — the container's view of
   `/mnt/remotes` was snapshotted at start time. This also bites when nothing
   was added at all: Unassigned Devices remounts its remote shares when the
   array stops and starts, and a container holding a private bind keeps
   looking at the stale, now-empty directory. `slave` propagates host-side
   mount and unmount events into the container going forward, and only in
   that direction: the container still cannot create or affect mounts the
   host sees, and the bind stays read-only exactly as before.
4. The one-time `chown -R 10001:10001 /mnt/user/appdata/timingdex-hub` (and
   the matching one for `timingdex-worker`) described above is unaffected by
   any of this — it applies to the Hub/Worker data directories, not the
   read-only media bind, and still only needs to run once.

## 3. Enroll and install the Worker template

The Worker container's default command is `worker run --config
/var/lib/timingdex-worker/worker.json`, and that file only exists after
enrollment succeeds — starting the container before enrolling just exits
immediately with a missing-config error (the same reason the Compose worker
service sits behind a profile and isn't started by `docker compose up`).

Add the `timingdex-worker.xml` template but run the enrollment command
**before** starting it — a one-off `docker run`, not `docker exec` into the
persistent container (it isn't running yet, and `worker run` without a
config exits too fast to exec into):

```sh
mkdir -p /mnt/user/appdata/timingdex-worker
chown -R 10001:10001 /mnt/user/appdata/timingdex-worker

docker run --rm -it \
  -v /mnt/user/appdata/timingdex-worker:/var/lib/timingdex-worker \
  timingdex:v0.31.0-alpha \
  worker enroll --hub https://<hub-ip>:8787 \
    --fingerprint <hub-fingerprint-from-/workers> \
    --pairing <one-time-token-from-/workers> \
    --name unraid-worker \
    --mount <root-id>=/media/library \
    --config /var/lib/timingdex-worker/worker.json
```

The bind mount above must be the exact host path you set as the Worker
template's **Worker 数据目录 Data directory**, so the `worker.json` this
command writes lands where the persistent container will actually look for it.

`--config /var/lib/timingdex-worker/worker.json` is mandatory: the Worker's
default config path is `~/.timingdex/worker.json` (not its data directory),
and the template's PostArgs run `worker run --config
/var/lib/timingdex-worker/worker.json`. Enroll and run must agree on the same
path, and that path must live on the persistent data directory
(`/mnt/user/appdata/timingdex-worker`) so a container recreate does not lose
the pairing.

Once it succeeds, start the `timingdex-worker` container normally.

Same GPU note as the Hub: if this box has no `/dev/dri`, remove
`--device=/dev/dri:/dev/dri` from the Worker template's Extra Parameters too.

## Verifying acceleration actually worked

Listing an encoder is not the same as it running — a missing runtime
library, a permission problem, or a driver too old for the hardware all list
fine and fail on the first frame. Check with:

```sh
docker exec timingdex-hub timingdex doctor
docker exec timingdex-worker timingdex worker doctor
docker exec timingdex-hub vainfo      # only present in the gpu image variant
```

`timingdex worker doctor` is a local hardware/enrollment report (Hub URL,
platform, name, detected hardware) — it does not contact the Hub or verify
leases, so use it to confirm acceleration, and the Worker's own `worker run`
logs plus the Hub's `/workers` page for connectivity.

## Updating

Back up the Hub data directory before upgrading: stop the `timingdex-hub`
container and copy `/mnt/user/appdata/timingdex-hub` (the SQLite database
together with its `-wal`/`-shm` sidecars), and note the current
`schema_migrations` state so a rollback can restore the snapshot. Then rebuild
or repull the `timingdex:v0.31.0-alpha` tag and recreate both containers from
the CA UI (**Force Update** / **Apply**).

Recreating keeps template values, so the `TIMINGDEX_HUB_ADMIN_AUTH` Variable
you added in step 2 and the Worker's data-directory path survive — just confirm
they are still present after importing a newer template revision. The uid
stays 10001 across versions, so the one-time `chown` above does not need to be
repeated.
