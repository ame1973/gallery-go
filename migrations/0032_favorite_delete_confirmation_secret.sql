-- A server-only key protects deletion previews from client-side guessing.
-- The application initializes it once with crypto/rand after authorizing a preview.
CREATE TABLE favorite_delete_confirmation_secret (
 id INTEGER PRIMARY KEY CHECK(id=1),
 secret BLOB NOT NULL CHECK(typeof(secret)='blob' AND length(secret)=32 AND secret!=zeroblob(32))
);
