.PHONY: help run run-pg test test-pg race bench cover up down
PG_TEST ?= postgres://booking:booking@localhost:5432/booking_test?sslmode=disable

help:            ## list targets
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | sed 's/:.*##/ —/'

run:             ## run with the in-memory store on :8080
	go run ./cmd/api

run-pg:          ## run against the Postgres from `make up`
	DATABASE_URL=postgres://booking:booking@localhost:5432/booking?sslmode=disable go run ./cmd/api

test:            ## unit + memory-store tests (Postgres tests are skipped)
	go test ./...

test-pg:         ## everything, including the Postgres contract tests
	TEST_DATABASE_URL=$(PG_TEST) go test -race -count=1 -p 1 ./...

race:            ## race detector
	go test -race -count=1 ./...

bench:           ## heap vs sorted-slice benchmark
	go test -run x -bench . -benchmem ./internal/booking

cover:           ## coverage report
	TEST_DATABASE_URL=$(PG_TEST) go test -p 1 -coverprofile=cover.out ./... && go tool cover -html=cover.out

up:              ## Postgres + API with Docker Compose
	docker compose up --build

down:            ## stop and remove containers (keeps data)
	docker compose down
