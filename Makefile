export DATABASE_URL ?= postgres://autocheck:autocheck@localhost:5432/autocheck?sslmode=disable
GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0

.PHONY: migrate run-api run-worker test lint vulncheck

migrate:
	go run ./cmd/migrate up

run-api:
	go run ./cmd/api

run-worker:
	go run ./cmd/worker

test:
	go test -race ./...

lint:
	$(GOLANGCI_LINT) run ./...

vulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...
