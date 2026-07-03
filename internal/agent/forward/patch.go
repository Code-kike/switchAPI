package forward

// patch.go — byte-exact rewrite of the top-level "model" field in a JSON
// request body, using only encoding/json token scanning (no re-marshal, no
// key reordering, no whitespace changes). Validated by research/06 against
// nested same-name keys, unicode escapes, and chunked uploads.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// patchModel replaces the value of the TOP-LEVEL "model" field per the
// redirects map, leaving every other byte of the document untouched.
//
// Returns (possibly new) body, original model, effective model, whether a
// splice happened. Nested "model" keys are never touched: non-target values
// are consumed whole via json.RawMessage. When no patch applies, the original
// slice is returned unchanged (zero copy).
func patchModel(body []byte, redirects map[string]string) (out []byte, from, to string, patched bool, err error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()

	tok, err := dec.Token()
	if err != nil {
		return body, "", "", false, fmt.Errorf("request body is not JSON: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return body, "", "", false, errors.New("request body is not a JSON object")
	}

	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return body, "", "", false, err
		}
		key, _ := keyTok.(string)

		if key != "model" {
			var skip json.RawMessage // consumes the whole value, however nested
			if err := dec.Decode(&skip); err != nil {
				return body, "", "", false, err
			}
			continue
		}

		// InputOffset after the key token == byte offset just past the key
		// string; between here and the value there can only be ':' and
		// whitespace. After Decode(&val) the offset is just past the value.
		start := dec.InputOffset()
		var val string
		if err := dec.Decode(&val); err != nil {
			return body, "", "", false, fmt.Errorf(`"model" is not a string: %w`, err)
		}
		end := dec.InputOffset()

		newModel, ok := redirects[val]
		if !ok || newModel == val {
			return body, val, val, false, nil
		}

		window := body[start:end]
		q := bytes.IndexByte(window, '"') // opening quote of the value literal
		if q < 0 {
			return body, val, val, false, errors.New("cannot locate model value bytes")
		}
		valStart := start + int64(q)

		enc, err := json.Marshal(newModel)
		if err != nil {
			return body, val, val, false, err
		}
		out = make([]byte, 0, len(body)-int(end-valStart)+len(enc))
		out = append(out, body[:valStart]...)
		out = append(out, enc...)
		out = append(out, body[end:]...)
		return out, val, newModel, true, nil
	}
	return body, "", "", false, nil // no top-level "model" field
}
