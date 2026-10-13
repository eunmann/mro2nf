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

	last := 0

	for i := range codeOffsets(text) {
		if wholeToken(text, i, negZero) {
			b.WriteString(text[last:i])
			b.WriteString("-0.0")
			last = i + len(negZero)
		}
	}

	b.WriteString(text[last:])

	return b.String()
}
