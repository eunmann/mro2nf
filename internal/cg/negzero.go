package cg

import "strings"

const negZero = "-0"

// keepNegativeZero restores the float literal -0.0 in text, MRO source as
// Martian's formatter prints it or invocation data as mrp encodes it, where
// it reads -0: an integer literal, which compiles to 0 (syntax
// format_exp.go FloatExp.format; core.BuildCallAst parses each argument's
// JSON as an MRO literal). mrp itself compiles the literal as written and
// passes -0 on. An integer zero never prints with a sign, so a -0 that is a
// whole token, outside a string or comment, is a float's.
func keepNegativeZero(text string) string {
	var b strings.Builder

	b.Grow(len(text))

	inString, comment := false, false

	for i := 0; i < len(text); i++ {
		c := text[i]

		switch {
		case comment:
			comment = c != '\n'
		case inString:
			if c == '\\' && i+1 < len(text) {
				b.WriteByte(c)
				i++
				c = text[i]
			} else {
				inString = c != '"'
			}
		case c == '"':
			inString = true
		case c == '#':
			comment = true
		case c == '-' && strings.HasPrefix(text[i:], negZero) && !numberByte(prev(text, i)) && !numberByte(next(text, i+len(negZero))):
			b.WriteString("-0.0")
			i++

			continue
		}

		b.WriteByte(c)
	}

	return b.String()
}

// numberByte reports a byte that can continue a number or name, so a -0
// beside it is part of a longer token.
func numberByte(c byte) bool {
	return c == '.' || c == '_' || c == '+' || c == '-' ||
		(c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func prev(text string, i int) byte {
	if i == 0 {
		return ' '
	}

	return text[i-1]
}

func next(text string, i int) byte {
	if i >= len(text) {
		return ' '
	}

	return text[i]
}
