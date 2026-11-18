export DATABASE_URL ?= postgres://autocheck:autocheck@localhost:5432/autocheck?sslmode=disable
GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0

.PHONY: migrate run-api run-worker test test-sandbox-security lint vulncheck proto-gen proto-lint

migrate:
	go run ./cmd/migrate up

run-api:
	go run ./cmd/api

run-worker:
	go run ./cmd/worker

test:
	go test -race ./...

test-sandbox-security:
	docker info >/dev/null
	go test -race -count=1 -v ./internal/sandbox ./internal/judge -run '^TestDocker(Security.*|SandboxSecurity|OtherWorkspaceUnreadable|WorkspaceQuotaRegression|RootOwnedSetuidDenied)$$'

lint:
	$(GOLANGCI_LINT) run ./...

vulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

proto-gen:   ## сгенерировать Go-код, gateway и openapi из api/ (нужен buf)
	buf dep update
	buf generate

proto-lint:
	buf lint
	buf format --diff --exit-code
