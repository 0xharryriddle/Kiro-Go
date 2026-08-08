# builder 阶段始终运行在构建机原生平台（amd64），用 Go 交叉编译目标平台二进制
FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /app
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o kiro-go .

FROM alpine:latest
RUN apk --no-cache add ca-certificates

# Run as a non-root user. UID/GID are 1000 ON PURPOSE and are not arbitrary:
# docker-compose.yml bind-mounts the host's ./data into /app/data, and a bind
# mount keeps the HOST's ownership — a build-time chown cannot change it. The
# app writes config.json (plus .bak rotations), data/imports, data/traces and
# the audit/request logs, so if the container UID does not match the host owner
# of ./data the process starts and then fails every persistence write.
#
# Measured on this repo before choosing the value: ./data and every file under it
# is uid=1000 gid=1000, and no root-owned files exist there (the previous
# root-running container left none). Override at build time if your host differs:
#   docker build --build-arg APP_UID=$(id -u) --build-arg APP_GID=$(id -g) .
ARG APP_UID=1000
ARG APP_GID=1000
RUN addgroup -g "${APP_GID}" -S kiro \
    && adduser -u "${APP_UID}" -G kiro -S -h /app kiro

WORKDIR /app
# --chown on COPY, not a later `RUN chown`: a separate chown layer duplicates
# every copied byte in the image.
COPY --from=builder --chown=${APP_UID}:${APP_GID} /app/kiro-go .
COPY --from=builder --chown=${APP_UID}:${APP_GID} /app/version.json .
COPY --from=builder --chown=${APP_UID}:${APP_GID} /app/web ./web
# Owned at build time so the NAMED-VOLUME / plain `docker run` case works too:
# Docker seeds a fresh named volume from the image, ownership included. The
# bind-mount case is handled by the UID match above instead.
RUN mkdir -p /app/data && chown -R "${APP_UID}:${APP_GID}" /app/data

EXPOSE 8080
# Enterprise SSO (Microsoft 365) loopback callback port — see docker-compose.yml.
EXPOSE 3128
VOLUME /app/data

# Image-level healthcheck so `docker run` (no compose) still gets a health signal;
# docker-compose.yml defines its own, which overrides this one. wget is BusyBox
# wget from the alpine base — verified present in this image, no extra package.
#
# The port is hardcoded to the container-side 8080 that EXPOSE and compose's
# `PORT=8080` both pin. If you override PORT, this check needs the same value.
# `/healthz` is unauthenticated by design and returns {"status","time"}.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null 2>&1 || exit 1

USER kiro

CMD ["./kiro-go"]
