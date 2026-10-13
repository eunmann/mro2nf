package cg

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// echoStage declares ECHO, whose call literals the cases vary.
const echoStage = `stage ECHO(
    in  string s,
    in  int    n,
    in  float  f,
    out int    m,
    src py     "stages/echo",
)
`

// echoCall is the call of ECHO holding n and f, at line 6, column 9 and
// line 7, column 9 of the call, after a comment and a string holding the
// int beyond int64 that are no literal.
func echoCall(callable, n, f string) string {
	return fmt.Sprintf(`
# 9223372036854775808 is no literal in a comment
call %s(
    s = "9223372036854775808",
    n = %s,
    f = %s,
)
`, callable, n, f)
}

// writeMROs writes each named .mro file into a new dir and returns it.
func writeMROs(t *testing.T, files map[string]string) string {
	t.Helper()

	dir := t.TempDir()
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

// TestReadSourceRefusesOutOfRangeLiterals (#567): an int literal outside
// int64 or a float one outside float64, which mrp's parser panics on
// (syntax/parsenum.go), fails the transpile with an error naming the file,
// line and column of the literal, in the entry or a file it includes; int64's
// ends and float64's largest value compile.
func TestReadSourceRefusesOutOfRangeLiterals(t *testing.T) {
	pipeline := `
pipeline P(out int m,)
{
    call ECHO(
        s = "x",
        n = 1,
        f = 1e400,
    )

    return (m = ECHO.m,)
}
`
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"int above", map[string]string{"pipeline.mro": echoStage + echoCall("ECHO", "9223372036854775808", "1")}, "pipeline.mro:12:9: "},
		{"int below", map[string]string{"pipeline.mro": echoStage + echoCall("ECHO", "-9223372036854775809", "1")}, "pipeline.mro:12:9: "},
		{"float", map[string]string{"pipeline.mro": echoStage + echoCall("ECHO", "1", "-1.5e400")}, "pipeline.mro:13:9: "},
		{"included", map[string]string{"echo.mro": echoStage + pipeline, "pipeline.mro": "@include \"echo.mro\"\n\ncall P()\n"}, "echo.mro:14:13: "},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeMROs(t, tc.files)

			_, err := ReadSource(filepath.Join(dir, "pipeline.mro"), []string{dir})
			if !errors.Is(err, ErrLiteralRange) {
				t.Fatalf("ReadSource: %v, want %v", err, ErrLiteralRange)
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("ReadSource: %v, want the literal's position %q", err, tc.want)
			}
		})
	}

	dir := writeMROs(t, map[string]string{"pipeline.mro": echoStage + echoCall("ECHO", "-9223372036854775808", "1.7976931348623157e308")})
	if _, err := ReadSource(filepath.Join(dir, "pipeline.mro"), []string{dir}); err != nil {
		t.Errorf("ReadSource of int64's min and float64's max: %v", err)
	}
}
