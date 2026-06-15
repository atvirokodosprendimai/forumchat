.PHONY: tidy gen build run dev up down logs test fmt vet lint-mailbox

tidy:
	go mod tidy

gen:
	go tool templ generate

build: gen
	CGO_ENABLED=0 go build -o bin/forumchat ./cmd/app

run: gen
	go run ./cmd/app

dev: gen
	go run ./cmd/app

up:
	docker compose up -d --build

down:
	docker compose down

logs:
	docker compose logs -f app

fmt:
	go fmt ./...

vet:
	go vet ./...

test:
	go test ./...

# lint-mailbox enforces the read-only IMAP contract by forbidding any
# mutating call inside internal/mailbox/. If the grep matches anything
# the build fails — the only call site for IMAP is imap.go, which uses
# ReadOnly:true on Select and Peek:true on BodySection. See the spec
# anti-enumeration block.
lint-mailbox:
	@! grep -rnE 'Store\(|Expunge|\.Move\(|\.Copy\(|BodySection\{[^}]*Peek:[[:space:]]*false' internal/mailbox/ \
		|| (echo "lint-mailbox: forbidden mutating IMAP call above" && exit 1)
	@echo "lint-mailbox: ok"
