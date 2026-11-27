.PHONY: build test check quick-check fmt dev api e2e views-load favorite-delete-load search-conformance search-candidates-load

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

quick-check:
	@printf '%s\n' 'Quick check: 5k album cases, no race or coverage; run make check for the full gate.'
	python3 -I tests/integration/repository_hygiene.py
	go vet ./...
	CGO_ENABLED=0 go test -p 1 -count=1 -timeout=15m -skip '^(TestRandomAlbums100KEnforcesVisibilityWithinMetadataDeadline|TestAlbumSelection100KUsesExistingSelectiveIndexesBeforeBoundedCards|TestFavoriteSubtree100KUsesIndexedBoundedPages)$$' ./...
	test -z "$$(gofmt -l cmd internal migrations contracts tests/integration/home_random_scale_test.go tests/integration/favorite_folder_scale_test.go tests/integration/favorite_folder_selection_test.go tests/integration/favorite_subtree_scale_test.go)"
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

search-conformance:
	python3 -I tests/searchconformance/run.py

# Synthetic lower bound only; this does not exercise ACL, ranking or an API.
search-candidates-load:
	go test -p 1 -count=3 -timeout=15m -race -tags=search_candidate_probe ./internal/search -run '^TestLiteralCandidates100KAdversarialProbe$$' -v

# Host-dependent production-deadline load checks use only isolated test databases.
views-load:
	CGO_ENABLED=0 go test -p 1 -count=1 -timeout=5m -tags=album_views_load ./tests/integration ./cmd/gallerygo -run '^(TestAlbumViewsAccountDelete100KMeetsMetadataDeadline|TestAlbumViewsAndRatingsAccountDelete100KMeetsMetadataDeadline|TestAlbumRatingsOnlyAccountDelete100KMeetsMetadataDeadline|TestCompiledAlbumViewsAccountDelete100K)$$' -v

favorite-delete-load:
	CGO_ENABLED=0 go test -p 1 -count=1 -timeout=15m -tags=favorite_delete_load ./tests/integration ./cmd/gallerygo -run '^(TestFavoriteCollectionDelete(100KMeetsMetadataDeadline|SparseSubtreeAt100K)|TestCompiledFavoriteDelete100K)$$' -v
