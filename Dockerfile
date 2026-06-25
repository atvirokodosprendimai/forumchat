FROM golang:1.26-alpine AS build
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum ./
# Cache mounts persist the module + compile caches across builds. On CI they are
# kept warm by buildkit-cache-dance (see .github/workflows/docker.yml); locally
# they survive between `docker build` runs. Modules live in the mount, not the
# layer, so every step that compiles must re-mount /go/pkg/mod to see them.
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go tool templ generate
# VERSION is injected by CI on tagged builds (see .github/workflows/docker.yml).
# Defaults to "dev" for a plain `docker build` so the footer never shows blank.
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X github.com/atvirokodosprendimai/forumchat/web/templ.Version=${VERSION}" \
      -o /out/forumchat ./cmd/app \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/forumchat-cli ./cmd/cli \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/forumchat /app/forumchat
COPY --from=build /out/forumchat-cli /app/forumchat-cli
COPY --from=build /src/web/static /app/web/static
# /data hosts the sqlite db, uploads, and persisted VAPID keys. Created with
# nonroot ownership (65532:65532 = distroless nonroot) so the running user can
# write to it without an entrypoint shim. Declared as a VOLUME so callers can
# `docker run -v $PWD/data:/data` and have everything survive a container swap.
COPY --from=build --chown=65532:65532 /out/data /data
VOLUME /data
# These overrides only apply inside the container; local `go run` keeps the
# `./data/...` defaults baked into config.go. Override at runtime with `-e`
# or compose `environment:` / `.env` if you mount somewhere other than /data.
ENV HTTP_ADDR=:8080 \
    DB_PATH=/data/forumchat.db \
    UPLOADS_DIR=/data/uploads \
    VAPID_KEYS_FILE=/data/vapid.json
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/app/forumchat"]
