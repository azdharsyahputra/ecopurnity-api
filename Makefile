.PHONY: seed-admin mail-preview gen check-gen up down reset run build test openapi lint-openapi migrate migrate-down migrate-status migrate-clickhouse db-doc

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

# Regenerate internal/api from the OpenAPI sources (bundle -> 3.0 codegen copy -> oapi-codegen -> 501 stubs).
gen:
	go generate ./internal/api

# Fails when the committed generated code is stale (run in CI).
check-gen: gen
	git diff --exit-code -- internal/api api/openapi.yaml

# Sends a sample of every email template through SMTP (Mailpit locally: http://localhost:8025).
mail-preview:
	set -a; . ./.env; set +a; go run ./cmd/mailpreview

# Grants the admin capability to an existing account: make seed-admin EMAIL=sari@example.id
seed-admin:
	@test -n "$(EMAIL)" || (echo "usage: make seed-admin EMAIL=..." && exit 1)
	set -a; . ./.env; set +a; go run ./cmd/seed-admin "$(EMAIL)"
