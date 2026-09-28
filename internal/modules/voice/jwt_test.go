package voice

import (
	"encoding/base64"
	"strings"
)

// splitJWT decodes the three base64url segments of a compact JWT (test helper).
func splitJWT(tok string) [][]byte {
	parts := strings.Split(tok, ".")
	out := make([][]byte, 0, len(parts))
	for _, p := range parts {
		b, err := base64.RawURLEncoding.DecodeString(p)
		if err != nil {
			return nil
		}
		out = append(out, b)
	}
	return out
}
