# URL shortener backend

# --- Image / registry config ---------------------------------------------
REGISTRY   ?= registry.home.arpa
IMAGE_NAME ?= url-shortener
TAG        ?= latest
IMAGE      := $(REGISTRY)/$(IMAGE_NAME):$(TAG)
# Raspberry Pi cluster runs linux/arm64.
PLATFORM   ?= linux/arm64
MAIN_PKG   := ./cmd/server

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

# --- Development ----------------------------------------------------------
.PHONY: generate
generate: ## Regenerate API code (oapi-codegen) and SQL code (sqlc)
	cd api && go generate ./...
	go run github.com/sqlc-dev/sqlc/cmd/sqlc@latest generate

.PHONY: build
build: ## Build the server binary into ./bin
	go build -o bin/server $(MAIN_PKG)

.PHONY: run
run: ## Run the server locally
	go run $(MAIN_PKG)

.PHONY: test
test: ## Run tests with the race detector
	go test -race ./...

.PHONY: cover
cover: ## Run tests and open the coverage report
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run ./...

.PHONY: fmt
fmt: ## Format the code
	gofmt -w .

.PHONY: tidy
tidy: ## Tidy go.mod / go.sum
	go mod tidy

.PHONY: check
check: fmt tidy lint test ## Run fmt, tidy, lint and test

# --- Container image (docker) ---------------------------------------------
.PHONY: image
image: ## Build the container image for $(PLATFORM)
	docker build --platform=$(PLATFORM) -t $(IMAGE) .

.PHONY: push
push: image ## Build and push the image to $(REGISTRY)
	docker push $(IMAGE)

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin coverage.out
