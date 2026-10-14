package cg

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/martian-lang/martian/martian/syntax"
	"github.com/martian-lang/martian/martian/util"
)

// ErrLiteralRange reports a number literal outside the range Martian's
// parser holds: an int outside int64 or a float outside float64, which mrp
// refuses.
var ErrLiteralRange = errors.New("number literal out of range: an int must fit int64 and a float float64")

// recoverLiteral runs parse, a parse by Martian's parser, and returns the
// number literal it panics on as out of range, or parse's error. Martian's
// lexer admits a 19-digit int beyond int64 and a float beyond float64, and
// its parser panics on either by design (syntax/parsenum.go parseInt,
// parseFloat), so recovering is the only way to refuse one as an error. Any
// other panic is re-raised.
func recoverLiteral(parse func() error) (string, error) {
	var lit string

	err := func() error {
		defer func() {
			r := recover()
			if r == nil {
				return
			}

			l, ok := rangePanic(r)
			if !ok {
				panic(r)
			}

			lit = l
		}()

		return parse()
	}()

	return lit, err
}

// parseMRO parses the .mro source src read from path, without its includes,
// and returns the number literal out of range it holds, if any.
func parseMRO(src []byte, path string) (*syntax.Ast, string, error) {
	var file *syntax.Ast

	lit, err := recoverLiteral(func() error {
		var p syntax.Parser

		f, err := p.UncheckedParse(src, path)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}

		file = f

		return nil
	})

	return file, lit, err
}

// rangePanic is the literal of r, a panic of Martian's number parsing over
// a literal out of range; parseInt's drops an int's sign.
func rangePanic(r any) (string, bool) {
	switch v := r.(type) {
	case string:
		return strings.CutPrefix(v, "integer overflow parsing ")
	case *strconv.NumError:
		return v.Num, errors.Is(v.Err, strconv.ErrRange)
	default:
		return "", false
	}
}

// CheckValue parses raw, a launch value as JSON, as core.BuildCallAst parses
// each argument of an invocation (syntax.Parser.ParseValExp), refusing a
// number literal out of range as mrp's parser does.
func CheckValue(raw []byte) error {
	lit, err := recoverLiteral(func() error {
		var p syntax.Parser

		if _, err := p.ParseValExp(raw); err != nil {
			return fmt.Errorf("parse: %w", err)
		}

		return nil
	})
	if lit == "" {
		return err
	}

	if _, tok, ok := literalOffset(string(raw), lit); ok {
		lit = tok
	}

	return fmt.Errorf("%w: %s", ErrLiteralRange, lit)
}

// literalError is the error of the out-of-range literal lit, a parse of the
// .mro file at path panicked on: its file, line and column.
func literalError(path string, mroPaths []string, lit string) error {
	at, tok, ok := findLiteral(path, mroPaths, lit, map[string]bool{})
	if !ok {
		return fmt.Errorf("%s: %w: %s", path, ErrLiteralRange, lit)
	}

	return fmt.Errorf("%s: %w: %s", at, ErrLiteralRange, tok)
}

// findLiteral is the position and text of lit's token in the .mro file at
// path or, depth first, in a file it includes, each resolved as Martian
// resolves an include (syntax/parser.go getIncludes): on the search path,
// then beside the including file.
func findLiteral(path string, mroPaths []string, lit string, seen map[string]bool) (string, string, bool) {
	abs, err := filepath.Abs(path)
	if err != nil || seen[abs] {
		return "", "", false
	}

	seen[abs] = true

	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", false
	}

	src := string(raw)
	if off, tok, ok := literalOffset(src, lit); ok {
		line := 1 + strings.Count(src[:off], "\n")
		col := off - strings.LastIndexByte(src[:off], '\n')

		return fmt.Sprintf("%s:%d:%d", path, line, col), tok, true
	}

	file, l, err := parseMRO(raw, path)
	if l != "" || err != nil {
		return "", "", false
	}

	dirs := append(slices.Clone(mroPaths), filepath.Dir(abs))
	for _, inc := range file.Includes {
		if ipath, err := util.FindUniquePath(inc.Value, dirs); err == nil {
			if at, tok, ok := findLiteral(ipath, mroPaths, lit, seen); ok {
				return at, tok, true
			}
		}
	}

	return "", "", false
}

// literalOffset is the offset and text of the first token in src that is
// lit or, when it also overflows, lit negated, as parseInt's panic drops an
// int's sign; the parse that panicked on lit read every token before it.
func literalOffset(src, lit string) (int, string, bool) {
	toks := []string{lit}

	negated := "-" + lit
	if _, err := strconv.ParseInt(negated, 10, 64); errors.Is(err, strconv.ErrRange) {
		toks = append(toks, negated)
	}

	for i := range codeOffsets(src) {
		for _, tok := range toks {
			if wholeToken(src, i, tok) {
				return i, tok, true
			}
		}
	}

	return 0, "", false
}
