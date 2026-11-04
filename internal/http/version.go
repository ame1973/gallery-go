package httpapi

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

func resourceETag(kind, id string, version int64) string {
	return fmt.Sprintf(`"%s:%s:%d"`, kind, id, version)
}

func resourcePrecondition(w http.ResponseWriter, r *http.Request, kind, id string) (int64, bool) {
	values := r.Header.Values("If-Match")
	if len(values) == 0 {
		problem(w, 428, "PRECONDITION_REQUIRED", "Send the resource's ETag in If-Match.")
		return 0, false
	}
	value := r.Header.Get("If-Match")
	version, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(value, `"`+kind+`:`+id+`:`), `"`), 10, 64)
	if len(values) != 1 || err != nil || version < 1 || value != resourceETag(kind, id, version) {
		problem(w, 400, "INVALID_PRECONDITION", "Send one strong resource ETag, without wildcards or lists.")
		return 0, false
	}
	return version, true
}
