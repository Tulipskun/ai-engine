package d1store

import (
	"encoding/json"
	"strings"
)

// encodeBlob renders a value as JSON text: the Worker stores whatever string it
// is given, so keeping the payload structured lets Hydrate tell a session blob
// apart from a raw config file.
func encodeBlob(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// decodeBlob accepts either our JSON object or a raw string written by an older
// build, so a D1 that already holds plain config still hydrates.
func decodeBlob(value string, out *SessionBlob) error {
	trimmed := strings.TrimSpace(value)
	if strings.HasPrefix(trimmed, "{") {
		return json.Unmarshal([]byte(trimmed), out)
	}
	var s string
	if err := json.Unmarshal([]byte(trimmed), &s); err == nil {
		out.Version = 0
		out.Data = s
		return nil
	}
	*out = SessionBlob{Version: 0, Data: value}
	return nil
}
