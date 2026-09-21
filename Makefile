.PHONY: run build test test-integration vet up down psql migrate-up migrate-down migrate-version migrate-force seed reset

run:
	go run ./cmd/api

build:
	go build -o bin/api ./cmd/api

test:
	go test -race -v ./...

up:
	docker compose up -d

down:
	docker compose down -v

psql:
	docker compose exec postgres psql -U loka -d loka

DB_URL = postgres://loka:loka@localhost:5432/loka?sslmode=disable

migrate-up:
	migrate -path migrations -database "$(DB_URL)" up

migrate-down:
	migrate -path migrations -database "$(DB_URL)" down 1

migrate-version:
	migrate -path migrations -database "$(DB_URL)" version

migrate-force:
	migrate -path migrations -database "$(DB_URL)" force $(V)

reset:
	docker compose down -v
	docker compose up -d
	@echo "Waiting for Postgres to accept TCP connections..."
	@for i in $$(seq 1 30); do \
		docker compose exec -T postgres pg_isready -h 127.0.0.1 -U loka >/dev/null 2>&1 && exit 0; \
		sleep 1; \
	done; \
	echo "Postgres not ready after 30s" >&2; exit 1
	migrate -path migrations -database "postgres://loka:loka@localhost:5432/loka?sslmode=disable" up
	docker compose exec -T postgres psql -U loka -d loka < scripts/seed.sql

seed:
	docker compose exec -T postgres psql -U loka -d loka < scripts/seed.sql

test-integration:
	go test -race -tags=integration -count=1 ./...

vet:
	go vet ./...
	go vet -tags=integration ./...