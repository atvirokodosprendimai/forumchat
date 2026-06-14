FROM golang:1.26-alpine AS build
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go tool templ generate
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/forumchat ./cmd/app \
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
