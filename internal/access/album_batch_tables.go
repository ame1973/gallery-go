package access

// Only these two compiled families may supply SQL identifiers. Request data,
// stored payloads and public resource keys never select a table or SQL fragment.
// Each family keeps typed immutable targets and independent foreign-key history.
type albumBatchTables struct {
	intent, targets, runs, results, audit string
}

func tablesForAlbumBatch(kind string) (albumBatchTables, bool) {
	switch kind {
	case "batch_tags":
		return albumBatchTables{"album_tag_batches", "album_tag_batch_targets", "album_tag_batch_runs", "album_tag_batch_results", "tag_batch"}, true
	case "batch_covers":
		return albumBatchTables{"album_cover_batches", "album_cover_batch_targets", "album_cover_batch_runs", "album_cover_batch_results", "cover_batch"}, true
	default:
		return albumBatchTables{}, false
	}
}
