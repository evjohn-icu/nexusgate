# syntax=docker/dockerfile:1

FROM golang:1.25-bookworm AS build

ARG TARGETOS=linux
ARG TARGETARCH=amd64

WORKDIR /src

# Keep dependency downloads in their own layer for source-only rebuilds.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN version="$(cat VERSION)" \
    && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags="-s -w -X github.com/evjohn-icu/nexusgate/internal/buildinfo.Version=${version} -X github.com/evjohn-icu/nexusgate/internal/domain.Version=${version}" \
      -o /out/nexusgate ./cmd/nexusgate

FROM debian:bookworm-slim AS runtime

ARG NEXUSGATE_UID=10001
ARG NEXUSGATE_GID=10001

# FFmpeg is used for probing/derived media. Debian ships the exiftool command
# in libimage-exiftool-perl; keeping both in the runtime image makes the image
# usable without host media-tool installations.
RUN apt-get update \
    && apt-get install --no-install-recommends --yes \
         ca-certificates \
         curl \
        ffmpeg \
        libimage-exiftool-perl \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid "${NEXUSGATE_GID}" nexusgate \
    && useradd --system --uid "${NEXUSGATE_UID}" --gid "${NEXUSGATE_GID}" \
        --home-dir /var/lib/nexusgate --no-create-home nexusgate \
    && install -d -o "${NEXUSGATE_UID}" -g "${NEXUSGATE_GID}" -m 0700 \
        /var/lib/nexusgate /var/lib/nexusgate-worker /media

COPY --from=build /out/nexusgate /usr/local/bin/nexusgate

ENV NEXUSGATE_DATA_DIR=/var/lib/nexusgate

USER nexusgate:nexusgate
WORKDIR /var/lib/nexusgate
STOPSIGNAL SIGTERM

ENTRYPOINT ["/usr/local/bin/nexusgate"]
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

USER nexusgate:nexusgate

# --------------------------------------------------------------------------
# CPU inference environment, opt-in. Only `docker build --target inference`
# (and the Compose override in deploy/docker-compose.inference.yml) reach
# these two stages; every default invocation above never downloads a Python
# interpreter or a wheel, so the lean image stays lean.
#
# The venv is built from the checked-in pin file -- inference/requirements/
# torch.lock, the resolved set that the "torch" environment in
# inference/models.lock.json validates against -- not from the floating
# requirements input. Installing exactly those pins is also what makes a
# broken environment fail the build: if any pin cannot be resolved or
# installed, pip exits nonzero here and no image gets published that would
# advertise an empty runtime. (The same rule covers architectures: a pin the
# index does not publish for TARGETARCH fails the build instead of silently
# substituting something else.)
#
# python:3.12-slim-bookworm is both the interpreter major the lock names
# ("environments.torch.python": "3.12") and the same Debian suite as the
# runtime stage, so the interpreter, its stdlib and libpython copied below
# are ABI-compatible with the libraries they link against.
FROM python:3.12-slim-bookworm AS inference-venv

# torch is pinned as 2.15.0+cpu, which exists only on the PyTorch CPU index;
# PyPI stays the primary index for every other pin in the lock. This is the
# same two-index layout inference/README.md documents for local venvs.
COPY inference/requirements/torch.lock /tmp/torch.lock
RUN python -m venv /opt/nexusgate-venv \
    && /opt/nexusgate-venv/bin/pip install \
         --disable-pip-version-check --no-cache-dir \
         --extra-index-url https://download.pytorch.org/whl/cpu \
         -r /tmp/torch.lock

# --------------------------------------------------------------------------
# The opt-in inference image: the lean runtime above plus the locked CPU
# Python environment and the checked-in inference module. Weights are never
# baked in; NEXUSGATE_INFERENCE_MODEL_DIR (Hub: <dataDir>/models, Worker:
# --inference-models) is populated at runtime by the hash-verified install
# path, and an artifact-free probe reports model_unavailable instead of
# downloading anything. Runtime enabling stays an operator decision: the
# Compose override sets NEXUSGATE_INFERENCE_* on the Hub and the matching
# --inference-* flags on the Worker command (deploy/docker-compose.inference.yml).
FROM runtime AS inference

USER root

# The venv's python entries are symlinks into the builder's
# /usr/local/bin/python3.12, and that interpreter is built --enable-shared,
# so the binary, the stdlib and the libpython shared object must exist in the
# runtime image at the same paths the symlinks point to.
COPY --from=inference-venv /opt/nexusgate-venv /opt/nexusgate-venv
COPY --from=inference-venv /usr/local/bin/python /usr/local/bin/python
COPY --from=inference-venv /usr/local/bin/python3 /usr/local/bin/python3
COPY --from=inference-venv /usr/local/bin/python3.12 /usr/local/bin/python3.12
COPY --from=inference-venv /usr/local/lib/python3.12 /usr/local/lib/python3.12
COPY --from=inference-venv /usr/local/lib/libpython3.12.so.1.0 /usr/local/lib/libpython3.12.so.1.0

# Shared libraries the copied interpreter's extension modules and the locked
# wheels load dynamically. The builder image keeps exactly these via its own
# ldd pass; a plain debian:bookworm-slim (the runtime stage) does not carry
# them. libgomp1 is torch's OpenMP runtime, libsndfile1 backs soundfile.
RUN apt-get update \
    && apt-get install --no-install-recommends --yes \
         libbz2-1.0 \
         libexpat1 \
         libffi8 \
         libgomp1 \
         liblzma5 \
         libsndfile1 \
         libsqlite3-0 \
         libssl3 \
         libstdc++6 \
         zlib1g \
    && rm -rf /var/lib/apt/lists/* \
    && ldconfig

# The checked-in module: dispatch/protocol/adapters plus models.lock.json and
# the pinned requirements. .dockerignore keeps venvs, caches and the model
# weights out of the context, so this copy is exactly the reviewed artifacts.
COPY inference/ /opt/nexusgate/inference/

# The venv's tools are handy for operators exec'ing into the container; it
# contains nothing that conflicts with the runtime image's own commands.
ENV PATH=/opt/nexusgate-venv/bin:/usr/local/bin:/usr/bin:/bin

USER nexusgate:nexusgate
RUN ["/opt/nexusgate-venv/bin/python", "-B", "-c", "import torch, transformers"]

# --------------------------------------------------------------------------
# `docker build` with no --target builds the last stage in this file. Without
# this alias that would silently become the newest stage above (gpu, then
# inference), breaking every existing build/CI invocation that doesn't pass
# --target. This stage adds no instructions of its own, so it is
# byte-identical to `runtime` -- it only keeps the untargeted build pointed
# at the plain CPU image.
FROM runtime AS default
