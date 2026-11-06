package store

import (
	"io/fs"
	"testing"
	"testing/fstest"

	"gallerygo/migrations"
)

func beforeMediaSelection(t *testing.T) fstest.MapFS {
	t.Helper()
	previous := beforeCoverBatches(t)
	data, err := fs.ReadFile(migrations.Files, "0019_album_cover_batches.sql")
	if err != nil {
		t.Fatal(err)
	}
	previous["0019_album_cover_batches.sql"] = &fstest.MapFile{Data: data}
	return previous
}

func TestMediaSelectionMigrationPreservesCaptureNullsMetadataAndHistory(t *testing.T) {
	db := testDB(t)
	previous := beforeMediaSelection(t)
	if err := Migrate(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO libraries(id,name,created_at) VALUES('library','Preserved',0);
 INSERT INTO albums(id,library_id,source_name,display_name,created_at,updated_at) VALUES('album','library','Source','Override',0,0);
 INSERT INTO media(id,album_id,kind,file_name,capture_at,metadata_json,created_at,updated_at) VALUES
 ('unknown','album','image','Unknown.png',NULL,'{}',0,0),('known','album','video','Known.mp4',-315619200000,'{"codec":"h264"}',0,0);
 INSERT INTO jobs(id,kind,payload_json,status,next_run_at,created_at,updated_at) VALUES('history','scan','{"version":1}','cancelled',0,0,0)`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if err := VerifySchema(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var unknown, known, history, indexes, ledger int
	if err := db.QueryRow(`SELECT
 (SELECT COUNT(*) FROM media WHERE id='unknown' AND capture_at IS NULL AND metadata_json='{}'),
 (SELECT COUNT(*) FROM media WHERE id='known' AND capture_at=-315619200000 AND json_extract(metadata_json,'$.codec')='h264'),
 (SELECT COUNT(*) FROM jobs WHERE id='history' AND status='cancelled'),
 (SELECT COUNT(*) FROM sqlite_schema WHERE type='index' AND name IN('idx_media_album_kind','idx_media_album_capture','idx_media_album_kind_capture')),
 (SELECT COUNT(*) FROM schema_migrations WHERE version='0020_media_selection.sql')`).Scan(&unknown, &known, &history, &indexes, &ledger); err != nil || unknown != 1 || known != 1 || history != 1 || indexes != 3 || ledger != 1 {
		t.Fatal("media ordering indexes rewrote evidence/history or did not publish atomically", unknown, known, history, indexes, ledger, err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal("media selection migration is not replay-safe", err)
	}
}

func TestMediaSelectionMigrationLateIndexFailureRollsBackAllNewDDL(t *testing.T) {
	db := testDB(t)
	previous := beforeMediaSelection(t)
	if err := Migrate(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE INDEX idx_media_album_kind_capture ON media(id)`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err == nil {
		t.Fatal("late conflicting media index was ignored")
	}
	var partial, ledger int
	if err := db.QueryRow(`SELECT
 (SELECT COUNT(*) FROM sqlite_schema WHERE type='index' AND name IN('idx_media_album_kind','idx_media_album_capture')),
 (SELECT COUNT(*) FROM schema_migrations WHERE version='0020_media_selection.sql')`).Scan(&partial, &ledger); err != nil || partial != 0 || ledger != 0 {
		t.Fatal("failed index migration left partial DDL/ledger", partial, ledger, err)
	}
	if err := VerifySchema(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP INDEX idx_media_album_kind_capture`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal("media selection migration could not retry after rollback", err)
	}
}
