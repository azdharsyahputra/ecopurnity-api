.PHONY: up down reset run build test openapi lint-openapi

up:
	docker compose up -d --wait

down:
	docker compose down

reset:
	docker compose down -v

run:
	set -a; . ./.env; set +a; go run ./cmd/api

build:
	go build -o bin/api ./cmd/api

test:
	go test ./...

openapi:
	npm run openapi:bundle

lint-openapi:
	npm run openapi:lint
