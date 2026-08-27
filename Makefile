.PHONY: run build test up down psql

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