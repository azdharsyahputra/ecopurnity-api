.PHONY: up down reset run build test openapi lint-openapi migrate migrate-down migrate-status migrate-clickhouse db-doc

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

migrate:
	set -a; . ./.env; set +a; go run ./cmd/migrate up

migrate-down:
	set -a; . ./.env; set +a; go run ./cmd/migrate down

migrate-status:
	set -a; . ./.env; set +a; go run ./cmd/migrate status

migrate-clickhouse:
	./scripts/migrate-clickhouse.sh

db-doc:
	./scripts/db-doc.sh
