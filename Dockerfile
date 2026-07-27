# syntax=docker/dockerfile:1

FROM golang:1.23-bookworm AS build

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
