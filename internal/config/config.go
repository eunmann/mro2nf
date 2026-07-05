// Package config loads a project's .mro2nf.yml: per-project defaults for the
// transpiler flags. It parses a deliberately tiny YAML subset — flat `key: value`
// lines, `#` comments, optional quotes — so no YAML dependency is needed; the
// keys mirror the CLI flags and any explicit flag still overrides the file.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// FileName is the config file the CLI looks for next to the pipeline .mro.
const FileName = ".mro2nf.yml"

var (
	errMalformed  = errors.New("expected `key: value`")
	errUnknownKey = errors.New("unknown key")
	errBadBool    = errors.New("want true or false")
)

// Config is the set of flag defaults a .mro2nf.yml may set. A nil field means the
// file did not set that key, so the CLI leaves the flag's own default in place.
//
// Native changes launch-time behavior, not just the emitted orchestration:
// -native bakes entry args at transpile time, so the project rejects
// launch-time entry-arg overrides — a project that defaults `native: true`
// opts every transpile into that contract. NativeRunner swaps the Python
// stage-execution hop to the embedded direct-call runner (baked into the
// image on container backends, so toggling it there means a rebuild).
//
// Overriding a config `true` back off for one run needs the equals form:
// `-native=false` (Go's flag package reads a space-separated `-native false`
// as bare -native plus a positional arg).
type Config struct {
	Target       *string
	Container    *string
	Mre          *string
	Shell        *string
	Mrjob        *string
	Monitor      *bool
	FuseChains   *bool
	FoldDisables *bool
	Native       *bool
	NativeRunner *bool
}

// Load reads and parses the config at path. A missing file is not an error — it
// returns a zero Config (no defaults set), so the caller can attempt a load
// unconditionally. For a path the user named explicitly, use LoadRequired so a
// typo is not silently ignored.
func Load(path string) (*Config, error) {
	cfg, err := LoadRequired(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Config{}, nil
	}

	return cfg, err
}

// LoadRequired is Load, except a missing file is an error. Use it for paths the
// user named explicitly, where tolerating absence would swallow a typo.
func LoadRequired(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	cfg := &Config{}
	sc := bufio.NewScanner(f)

	for line := 1; sc.Scan(); line++ {
		if err := parseLine(cfg, sc.Text()); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
	}

	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	return cfg, nil
}

// parseLine applies one `key: value` line to cfg. Blank lines and comments are
// ignored; an unknown key or malformed value is an error so typos are loud.
func parseLine(cfg *Config, raw string) error {
	line := strings.TrimSpace(stripComment(raw))
	if line == "" {
		return nil
	}

	key, val, ok := strings.Cut(line, ":")
	if !ok {
		return fmt.Errorf("%w, got %q", errMalformed, raw)
	}

	key = strings.TrimSpace(key)
	val = unquote(strings.TrimSpace(val))

	return assign(cfg, key, val)
}

// assign sets the field for key, parsing bools where the flag is a bool.
func assign(cfg *Config, key, val string) error {
	switch key {
	case "target":
		cfg.Target = &val
	case "container":
		cfg.Container = &val
	case "mre":
		cfg.Mre = &val
	case "shell":
		cfg.Shell = &val
	case "mrjob":
		cfg.Mrjob = &val
	case "monitor":
		return assignBool(&cfg.Monitor, key, val)
	case "fuse-chains":
		return assignBool(&cfg.FuseChains, key, val)
	case "fold-disables":
		return assignBool(&cfg.FoldDisables, key, val)
	case "native":
		return assignBool(&cfg.Native, key, val)
	case "native-runner":
		return assignBool(&cfg.NativeRunner, key, val)
	default:
		return fmt.Errorf("%w %q", errUnknownKey, key)
	}

	return nil
}

func assignBool(dst **bool, key, val string) error {
	b, err := strconv.ParseBool(val)
	if err != nil {
		return fmt.Errorf("key %q: %w, got %q", key, errBadBool, val)
	}

	*dst = &b

	return nil
}

// stripComment removes a trailing `#` comment that is not inside quotes.
func stripComment(s string) string {
	inQuote := byte(0)

	for i := range len(s) {
		switch c := s[i]; {
		case inQuote != 0:
			if c == inQuote {
				inQuote = 0
			}
		case c == '"' || c == '\'':
			inQuote = c
		case c == '#':
			return s[:i]
		}
	}

	return s
}

// unquote strips a single matching pair of surrounding quotes.
func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}

	return s
}
