.PHONY: tidy gen build run dev up down logs test fmt vet

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
