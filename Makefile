.PHONY: help build run test vet sqlc seed tidy docker-up

GOBIN := $(shell go env GOPATH)/bin

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

build: ## Compile the API binary to ./bin/api
	go build -o bin/api ./cmd/api

run: ## Run the API (reads env from the shell / .env)
	go run ./cmd/api

test: ## Run the test suite (needs a reachable TEST_DATABASE_URL)
	go test ./...

vet: ## Static analysis
	go vet ./...

sqlc: ## Regenerate the type-safe query layer from db/queries
	$(GOBIN)/sqlc generate

seed: ## Load sample development data
	go run ./cmd/seed

tidy: ## Tidy go.mod / go.sum
	go mod tidy

docker-up: ## Build and run the full stack (API + Postgres)
	docker compose up --build
