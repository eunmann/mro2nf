package emit

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// ErrGroovyControlChar reports a value with a control or line-separator
// character, which has no place in a single-quoted config literal: a newline
// would end the statement mid-literal.
var ErrGroovyControlChar = errors.New("control character in a Groovy string literal")

// GroovySingleQuoted renders s as a single-quoted Groovy string literal for a
// generated config. Under Nextflow's v1 config parser a config file is a Groovy
// script, so a value that could close the literal would run as code at parse
// time; every config string built from external input goes through here.
func GroovySingleQuoted(s string) (string, error) {
	for _, r := range s {
		if unicode.IsControl(r) || unicode.In(r, unicode.Zl, unicode.Zp) {
			return "", fmt.Errorf("%w: %q", ErrGroovyControlChar, s)
		}
	}

	// Only \ and ' are special in a Groovy single-quoted string ($ does not
	// interpolate there); \ goes first so the quote's escape is not doubled.
	s = strings.ReplaceAll(s, `\`, `\\`)

	return "'" + strings.ReplaceAll(s, `'`, `\'`) + "'", nil
}
