package emit_test

import (
	"errors"
	"testing"

	"github.com/eunmann/mro2nf/internal/emit"
)

// TestGroovySingleQuoted guards #329: a value spliced into a single-quoted
// Groovy literal of a generated config must not be able to end the literal
// early (a quote, or a backslash that escapes the closing quote) or end the
// statement line (a newline), since the v1 config parser runs the rest as code.
func TestGroovySingleQuoted(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain", "8 GB", `'8 GB'`},
		{"empty", "", `''`},
		{"regex_text_is_untouched", "(A(_MAP)?|STAGE_[0-9]+_.+__A)", `'(A(_MAP)?|STAGE_[0-9]+_.+__A)'`},
		{"quote_cannot_close_literal", "a'b", `'a\'b'`},
		{"backslash_cannot_escape_closing_quote", `a\`, `'a\\'`},
		{"backslash_before_quote", `a\'b`, `'a\\\'b'`},
		{"dollar_is_literal_in_single_quotes", "a$b", `'a$b'`},
		{"non_ascii_letter", "é", `'é'`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := emit.GroovySingleQuoted(tc.in)
			if err != nil {
				t.Fatalf("GroovySingleQuoted(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("GroovySingleQuoted(%q) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

// TestGroovySingleQuotedRejectsControl: a newline would end the config line
// mid-literal, and no other control character belongs in a selector, image name
// or memory value, so every one is refused rather than escaped.
func TestGroovySingleQuotedRejectsControl(t *testing.T) {
	for _, in := range []string{"a\nb", "a\rb", "a\tb", "a\x00b", "a\x7fb", "a\u0085b", "a b"} {
		if got, err := emit.GroovySingleQuoted(in); !errors.Is(err, emit.ErrGroovyControlChar) {
			t.Errorf("GroovySingleQuoted(%q) = %q, %v; want ErrGroovyControlChar", in, got, err)
		}
	}
}
