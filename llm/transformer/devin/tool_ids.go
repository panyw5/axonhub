package devin

import (
	"encoding/base64"
	"strings"
	"unicode/utf8"
)

const toolIDPrefix = "axonhub_tid_v1_"

func isPortableToolCallID(id string) bool {
	if id == "" {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}

// encodeToolID reserves a namespace so native IDs cannot collide with
// encoded IDs. Empty deltas remain empty until upstream supplies the call ID.
func encodeToolID(id string) string {
	if id == "" || (isPortableToolCallID(id) && !strings.HasPrefix(id, toolIDPrefix)) {
		return id
	}
	return toolIDPrefix + base64.RawURLEncoding.EncodeToString([]byte(id))
}

// decodeToolID removes exactly one canonical encoding layer. Ordinary
// legal IDs, malformed encodings and arbitrary base64-looking IDs stay intact.
func decodeToolID(id string) string {
	if !strings.HasPrefix(id, toolIDPrefix) {
		return id
	}
	decoded, errDecode := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(id, toolIDPrefix))
	if errDecode != nil || !utf8.Valid(decoded) || len(decoded) == 0 {
		return id
	}
	original := string(decoded)
	if encodeToolID(original) != id {
		return id
	}
	return original
}
