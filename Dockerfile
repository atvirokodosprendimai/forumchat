FROM golang:1.26-alpine AS build
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go tool templ generate
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/forumchat ./cmd/app \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/forumchat-cli ./cmd/cli

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/forumchat /app/forumchat
COPY --from=build /out/forumchat-cli /app/forumchat-cli
COPY --from=build /src/web/static /app/web/static
ENV HTTP_ADDR=:8080
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/app/forumchat"]
