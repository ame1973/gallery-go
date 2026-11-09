package auth

import (
	"path/filepath"
	"strings"
	"testing"

	"gallerygo/internal/store"
	"gallerygo/migrations"
)

func TestAlbumViewsAccountDeletionPlansUseAccountIndexes(t *testing.T) {
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "plans.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := store.Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`CREATE TEMP TABLE deleting_user_album_ids(album_id TEXT PRIMARY KEY) WITHOUT ROWID`); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, query string
		args        []any
		tables      []string
	}{
		{"routing", userDeleteRoutingSQL, []any{"actor", "actor"}, []string{"album_views", "ratings"}},
		{"ratings", userDeleteStageRatingsSQL, []any{"actor"}, []string{"ratings"}},
		{"views", userDeleteStageViewsSQL, []any{"actor"}, []string{"album_views"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			rows, err := tx.QueryContext(t.Context(), `EXPLAIN QUERY PLAN `+test.query, test.args...)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var plan []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				plan = append(plan, detail)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			for _, table := range test.tables {
				found := false
				for _, line := range plan {
					if strings.Contains(line, "SCAN "+table) || strings.Contains(line, "TEMP B-TREE") {
						t.Fatal("account deletion planned a global table scan or sort", test.name, plan)
					}
					if strings.Contains(line, "SEARCH "+table+" ") && strings.Contains(line, "INDEX") && strings.Contains(line, "user_id=?") {
						found = true
					}
				}
				if !found {
					t.Fatal("account deletion lost an account-indexed lookup", table, plan)
				}
			}
		})
	}
}
