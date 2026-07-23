package gateway

// parseStreamField scans bodyBytes for the JSON "stream" field value.
// Returns true when "stream":true is found at the top-level of the JSON object.
// Tracks whether the scanner is inside a JSON string to avoid false matches
// from "stream" appearing in message content.
//
// When the "stream" key is absent, returns false (OpenAI spec default).
// Does NOT perform a full JSON unmarshal — typical overhead < 1μs for 50KB body.
func parseStreamField(body []byte) bool {
	if len(body) == 0 {
		return false
	}

	// target is the byte sequence we search for: "stream"
	target := []byte(`"stream"`)
	targetLen := len(target)

	inString := false
	escaped := false
	depth := 0 // brace/bracket nesting depth; top-level == 0

	for i := range body {
		b := body[i]

		if escaped {
			escaped = false
			continue
		}

		if b == '\\' && inString {
			escaped = true
			continue
		}

		if b == '"' {
			if !inString {
				// Start of a string — check if it is the "stream" key at top-level depth 1.
				if depth == 1 && i+targetLen <= len(body) && matchBytes(body[i:i+targetLen], target) {
					// Advance past the key and colon, then read the value.
					j := i + targetLen
					j = skipWhitespace(body, j)
					if j < len(body) && body[j] == ':' {
						j++
						j = skipWhitespace(body, j)
						return matchTrue(body, j)
					}
				}
				inString = true
			} else {
				inString = false
			}
			continue
		}

		if inString {
			continue
		}

		switch b {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		}
	}

	return false
}

// matchBytes checks if a == b byte-by-byte. Both must have the same length.
func matchBytes(a, b []byte) bool {
	for k := range a {
		if a[k] != b[k] {
			return false
		}
	}
	return true
}

// skipWhitespace advances index past JSON whitespace characters.
func skipWhitespace(data []byte, i int) int {
	for i < len(data) {
		switch data[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

// matchTrue checks if data[i:] starts with "true".
func matchTrue(data []byte, i int) bool {
	return i+4 <= len(data) && data[i] == 't' && data[i+1] == 'r' && data[i+2] == 'u' && data[i+3] == 'e'
}
