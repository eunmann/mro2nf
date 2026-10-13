package cg

import (
	"iter"
	"strings"
)

// codeOffsets yields the offsets of the bytes of text, MRO source or
// invocation data, outside its strings and comments, as Martian's lexer
// reads them (syntax/tokenizer.go).
func codeOffsets(text string) iter.Seq[int] {
	return func(yield func(int) bool) {
		inString, comment := false, false

		for i := 0; i < len(text); i++ {
			c := text[i]

			switch {
			case comment:
				comment = c != '\n'
			case inString:
				if c == '\\' {
					i++
				} else {
					inString = c != '"'
				}
			case c == '"':
				inString = true
			case c == '#':
				comment = true
			default:
				if !yield(i) {
					return
				}
			}
		}
	}
}

// wholeToken reports whether text holds tok at offset i as a token of its
// own, not part of a longer number or name.
func wholeToken(text string, i int, tok string) bool {
	if !strings.HasPrefix(text[i:], tok) {
		return false
	}

	end := i + len(tok)

	return (i == 0 || !numberByte(text[i-1])) && (end == len(text) || !numberByte(text[end]))
}

// numberByte reports a byte that can continue a number or name.
func numberByte(c byte) bool {
	return c == '.' || c == '_' || c == '+' || c == '-' ||
		(c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
