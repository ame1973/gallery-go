.PHONY: build test check fmt dev api e2e views-load favorite-delete-load

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

# Host-dependent production-deadline load checks use only isolated test databases.
views-load:
	CGO_ENABLED=0 go test -p 1 -count=1 -timeout=5m -tags=album_views_load ./tests/integration ./cmd/gallerygo -run '^(TestAlbumViewsAccountDelete100KMeetsMetadataDeadline|TestAlbumViewsAndRatingsAccountDelete100KMeetsMetadataDeadline|TestAlbumRatingsOnlyAccountDelete100KMeetsMetadataDeadline|TestCompiledAlbumViewsAccountDelete100K)$$' -v

favorite-delete-load:
	CGO_ENABLED=0 go test -p 1 -count=1 -timeout=15m -tags=favorite_delete_load ./tests/integration ./cmd/gallerygo -run '^(TestFavoriteCollectionDelete(100KMeetsMetadataDeadline|SparseSubtreeAt100K)|TestCompiledFavoriteDelete100K)$$' -v
