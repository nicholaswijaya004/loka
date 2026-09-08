.PHONY: run build test up down psql migrate-up migrate-down migrate-version migrate-force seed reset

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
	until docker compose exec -T postgres pg_isready -U loka >/dev/null 2>&1; do sleep 1; done
	migrate -path migrations -database "$(DB_URL)" up
	docker compose exec -T postgres psql -U loka -d loka < scripts/seed.sql

seed:
	docker compose exec -T postgres psql -U loka -d loka < scripts/seed.sql