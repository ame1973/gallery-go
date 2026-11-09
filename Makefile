.PHONY: build test check fmt dev api e2e

build:
	pnpm build
	go build -trimpath -o bin/gallerygo ./cmd/gallerygo

test:
	go test ./...
	pnpm test

check:
	python3 -I tests/integration/repository_hygiene.py
	go vet ./...
	# Keep production-strength bcrypt and avoid overlapping isolated package binaries.
	go test -p 1 -timeout=15m -race -cover ./...
	test -z "$$(gofmt -l cmd internal migrations contracts)"
	pnpm api:check
	pnpm typecheck
	pnpm lint
	pnpm format:check
	pnpm test
	pnpm build

fmt:
	gofmt -w cmd internal migrations contracts
	pnpm format

dev:
	pnpm dev

api:
	pnpm api:lint
	pnpm api:generate

e2e:
	pnpm build
	pnpm test:e2e
