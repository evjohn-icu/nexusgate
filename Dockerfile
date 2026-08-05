# syntax=docker/dockerfile:1

FROM golang:1.25-bookworm AS build

ARG TARGETOS=linux
ARG TARGETARCH=amd64

WORKDIR /src

# Keep dependency downloads in their own layer for source-only rebuilds.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/timingdex ./cmd/timingdex

FROM debian:bookworm-slim AS runtime

ARG TIMINGDEX_UID=10001
ARG TIMINGDEX_GID=10001

# FFmpeg is used for probing/derived media. Debian ships the exiftool command
# in libimage-exiftool-perl; keeping both in the runtime image makes the image
# usable without host media-tool installations.
RUN apt-get update \
    && apt-get install --no-install-recommends --yes \
        ca-certificates \
        ffmpeg \
        libimage-exiftool-perl \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid "${TIMINGDEX_GID}" timingdex \
    && useradd --system --uid "${TIMINGDEX_UID}" --gid "${TIMINGDEX_GID}" \
        --home-dir /var/lib/timingdex --no-create-home timingdex \
    && install -d -o "${TIMINGDEX_UID}" -g "${TIMINGDEX_GID}" -m 0700 \
        /var/lib/timingdex /var/lib/timingdex-worker /media

COPY --from=build /out/timingdex /usr/local/bin/timingdex

ENV TIMINGDEX_DATA_DIR=/var/lib/timingdex

USER timingdex:timingdex
WORKDIR /var/lib/timingdex
STOPSIGNAL SIGTERM

ENTRYPOINT ["/usr/local/bin/timingdex"]
CMD ["serve"]

# --------------------------------------------------------------------------
# GPU variant: `docker build --target gpu` layers the VAAPI userspace for
# Intel and AMD iGPUs on top of the image above. Both vendors go through the
# same DRM render node (/dev/dri/renderD128) via libva -- Mesa's radeonsi
# serves AMD, intel-media-va-driver's iHD serves Intel -- so one image
# variant covers both; there is nothing vendor-specific to select at build
# time.
#
# NVIDIA gets nothing baked in here, on purpose. Debian's stock ffmpeg
# package already has h264_nvenc/hevc_nvenc compiled in; NVENC only fails in
# a container because the runtime libraries (libnvidia-encode.so.1,
# libcuda.so.1) and device nodes have to come from the host via the NVIDIA
# Container Toolkit, matched exactly to the host's loaded kernel module.
# Baking any NVIDIA .so into this image would be a version-skew trap the
# moment the host driver updates. NVIDIA support is therefore pure
# compose/template wiring -- see the device-passthrough comments in
# docker-compose.yml -- not a Dockerfile package.
FROM runtime AS gpu

USER root

# debian:bookworm-slim's default /etc/apt/sources.list.d/debian.sources
# (deb822 format) enables only the "main" component. Verified by pulling the
# image and reading the file directly:
#   docker run --rm debian:bookworm-slim cat /etc/apt/sources.list.d/debian.sources
# intel-media-va-driver-non-free lives in "non-free", which is not on by
# default, so it has to be turned on before apt can see the package at all.
#
# Packages, and why each is here:
#   intel-media-va-driver-non-free  Intel's iHD VAAPI driver (needs non-free, above).
#   intel-media-va-driver           The free fallback iHD build, in case non-free
#                                    packages are blocked at the mirror/policy level.
#   libvpl2                         The oneVPL runtime QSV needs. Without it,
#                                    h264_qsv still lists in `ffmpeg -encoders` and
#                                    every encode fails on the first frame -- the
#                                    same failure shape as no GPU at all.
#   mesa-va-drivers                 AMD's radeonsi VAAPI driver.
#   vainfo                          Makes `docker exec <container> vainfo` a real
#                                    diagnostic instead of a guess.
#
# Installed one package at a time and allowed to fail individually: these
# names, and which component carries them, differ across Debian releases and
# get renamed often enough that one missing name must not take the rest of
# the transaction down with it. deploy/prepare-node.sh hit exactly this bug
# before its own driver install loop was split the same way -- see the
# comment above its `for pkg in $DRIVER_PKGS` loop.
RUN sed -i 's/Components: main$/Components: main non-free/' /etc/apt/sources.list.d/debian.sources \
    && apt-get update \
    && for pkg in \
        intel-media-va-driver-non-free \
        intel-media-va-driver \
        libvpl2 \
        mesa-va-drivers \
        vainfo \
    ; do \
        apt-get install --no-install-recommends --yes "$pkg" \
            || echo "gpu image: skipped $pkg (not available on this mirror/policy)" >&2; \
    done \
    && rm -rf /var/lib/apt/lists/*

USER timingdex:timingdex

# --------------------------------------------------------------------------
# `docker build` with no --target builds the last stage in this file. Without
# this alias that would silently become `gpu` the moment the stage above was
# added, breaking every existing build/CI invocation that doesn't pass
# --target. This stage adds no instructions of its own, so it is
# byte-identical to `runtime` -- it only keeps the untargeted build pointed
# at the plain CPU image.
FROM runtime AS default
