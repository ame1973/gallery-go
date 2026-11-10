package httpapi

import (
	"encoding/hex"
	"strings"
	"testing"

	"gallerygo/internal/access"
	"gallerygo/internal/store"
)

func TestFavoriteCollectionDeletionPreviewKeepsServerKeyAndErrorsPrivate(t *testing.T) {
	f := protectedMedia(t)
	rootResponse := f.reader.request("POST", "/api/v2/favorite-collections", map[string]string{"name": "Private key root"}, "", store.NewID())
	root := decodeResponse[access.FavoriteCollection](t, rootResponse, 201)
	path := "/api/v2/favorite-collections/" + root.ID + "/deletion-preview?action=remove_favorites"
	expectCode(t, f.admin.request("GET", path, nil, "", ""), 404, "NOT_FOUND")
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM favorite_delete_confirmation_secret`).Scan(&count); err != nil || count != 0 {
		t.Fatal("foreign preview initialized the private key", count, err)
	}
	if _, err := f.db.Exec(`CREATE TRIGGER fail_private_key BEFORE INSERT ON favorite_delete_confirmation_secret BEGIN SELECT RAISE(ABORT,'/private/SQL/confirmation-key'); END`); err != nil {
		t.Fatal(err)
	}
	failed := f.reader.request("GET", path, nil, "", "")
	expectCode(t, failed, 500, "INTERNAL_ERROR")
	if strings.Contains(problemContent(t, failed), "/private/SQL/confirmation-key") {
		t.Fatal("key initialization error disclosed private diagnostics")
	}
	if _, err := f.db.Exec(`DROP TRIGGER fail_private_key`); err != nil {
		t.Fatal(err)
	}
	response := f.reader.request("GET", path, nil, "", "")
	preview := decodeResponse[access.FavoriteCollectionDeletionPreview](t, response, 200)
	var secret []byte
	if err := f.db.QueryRow(`SELECT secret FROM favorite_delete_confirmation_secret WHERE id=1`).Scan(&secret); err != nil || len(secret) != 32 {
		t.Fatal("authorized preview did not create a private key", err)
	}
	if strings.Contains(response.Body.String(), hex.EncodeToString(secret)) || preview.ConfirmationToken == hex.EncodeToString(secret) || response.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("preview disclosed its server key or lost privacy")
	}
}
