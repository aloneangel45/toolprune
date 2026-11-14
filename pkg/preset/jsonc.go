package preset

import "bytes"

// utf8BOM is the optional 3-byte UTF-8 Byte Order Mark emitted by some Windows editors.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// NormalizeJSONC converts JSON with Comments (JSONC) — as used by Zed settings.json,
// VS Code .vscode/mcp.json, and hand-edited IDE configs — into strict RFC 8259 JSON.
//
// Specifically, NormalizeJSONC:
//   - Strips a leading UTF-8 BOM if present.
//   - Strips "//" single-line comments and closed "/* ... */" block comments outside string literals,
//     preserving line breaks so JSON syntax error line numbers remain accurate.
//   - Strips a single trailing comma before '}' or ']' outside string literals.
//   - Preserves "//", "/*", "*/", escaped quotes, and ",}" sequences verbatim inside JSON string literals.
//   - Leaves unterminated "/*" block comments intact so json.Unmarshal rejects truncated/malformed input.
func NormalizeJSONC(data []byte) []byte {
	data = bytes.TrimPrefix(data, utf8BOM)
	if len(data) == 0 {
		return data
	}

	// Pass 1: Strip // and /* ... */ comments outside string literals.
	noComments := make([]byte, 0, len(data))
	inString := false
	escaped := false

	for i := 0; i < len(data); {
		c := data[i]
		if inString {
			noComments = append(noComments, c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			i++
			continue
		}

		if c == '"' {
			inString = true
			noComments = append(noComments, c)
			i++
			continue
		}

		// Line comment: // ... up to \r or \n
		if c == '/' && i+1 < len(data) && data[i+1] == '/' {
			i += 2
			for i < len(data) && data[i] != '\n' && data[i] != '\r' {
				i++
			}
			continue
		}

		// Block comment: /* ... */
		if c == '/' && i+1 < len(data) && data[i+1] == '*' {
			end := findBlockCommentEnd(data, i+2)
			if end != -1 {
				noComments = append(noComments, ' ')
				for k := i + 2; k < end; k++ {
					if data[k] == '\n' || data[k] == '\r' {
						noComments = append(noComments, data[k])
					}
				}
				i = end + 2
				continue
			}
			// Unterminated block comment: emit verbatim so json.Unmarshal fails cleanly.
		}

		noComments = append(noComments, c)
		i++
	}

	// Pass 2: Strip trailing commas immediately preceding '}' or ']' outside string literals.
	out := make([]byte, 0, len(noComments))
	inString = false
	escaped = false

	for i := 0; i < len(noComments); i++ {
		c := noComments[i]
		if inString {
			out = append(out, c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}

		if c == '"' {
			inString = true
			out = append(out, c)
			continue
		}

		if c == ',' {
			j := i + 1
			for j < len(noComments) && isJSONWhitespace(noComments[j]) {
				j++
			}
			if j < len(noComments) && (noComments[j] == '}' || noComments[j] == ']') {
				continue
			}
		}

		out = append(out, c)
	}

	return out
}

func findBlockCommentEnd(data []byte, start int) int {
	for i := start; i+1 < len(data); i++ {
		if data[i] == '*' && data[i+1] == '/' {
			return i
		}
	}
	return -1
}

func isJSONWhitespace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n'
}
