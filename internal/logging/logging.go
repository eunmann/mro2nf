// Package logging provides the transpiler's log/slog logger.
//
// Output goes to stderr so that stdout stays clean for tool output.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"
	"unicode"
)

const (
	ansiReset   = "\x1b[0m"
	ansiBold    = "\x1b[1m"
	ansiRed     = "\x1b[31m"
	ansiBoldRed = "\x1b[1;31m"
	ansiGreen   = "\x1b[32m"
	ansiYellow  = "\x1b[33m"
	ansiCyan    = "\x1b[36m"
	ansiGrey    = "\x1b[90m"
)

// New returns a stderr logger at the LOG_LEVEL level (debug, info, warn or
// error; default info). Records are coloured only when stderr is a terminal
// and NO_COLOR is unset, so piped and CI output carries no escape codes.
func New() *slog.Logger {
	return newLogger(os.Stderr, os.Getenv)
}

func newLogger(f *os.File, getenv func(string) string) *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(getenv("LOG_LEVEL"))); err != nil {
		level = slog.LevelInfo
	}

	return slog.New(newHandler(f, level, useColor(f, getenv)))
}

func useColor(f *os.File, getenv func(string) string) bool {
	if getenv("NO_COLOR") != "" {
		return false
	}

	fi, err := f.Stat()

	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// handler writes one line per record: `3:04PM INF message key=value ...`.
type handler struct {
	mu    *sync.Mutex
	w     io.Writer
	level slog.Leveler
	color bool
	// attrs are the WithAttrs fields, already formatted.
	attrs []byte
	// group is the WithGroup key prefix, e.g. "a.b.".
	group string
}

func newHandler(w io.Writer, level slog.Leveler, color bool) *handler {
	return &handler{mu: &sync.Mutex{}, w: w, level: level, color: color}
}

func (h *handler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level.Level()
}

func (h *handler) Handle(_ context.Context, r slog.Record) error {
	var buf []byte

	if !r.Time.IsZero() {
		buf = h.paint(buf, ansiGrey, r.Time.Format(time.Kitchen))
		buf = append(buf, ' ')
	}

	tag, color := levelTag(r.Level)
	buf = h.paint(buf, color, tag)
	buf = append(buf, ' ')
	buf = h.paint(buf, ansiBold, r.Message)
	buf = append(buf, h.attrs...)

	r.Attrs(func(a slog.Attr) bool {
		buf = h.appendAttr(buf, h.group, a)

		return true
	})

	buf = append(buf, '\n')

	h.mu.Lock()
	defer h.mu.Unlock()

	if _, err := h.w.Write(buf); err != nil {
		return fmt.Errorf("write log record: %w", err)
	}

	return nil
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}

	c := *h
	c.attrs = append([]byte(nil), h.attrs...)

	for _, a := range attrs {
		c.attrs = h.appendAttr(c.attrs, h.group, a)
	}

	return &c
}

func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}

	c := *h
	c.group = h.group + name + "."

	return &c
}

func (h *handler) appendAttr(buf []byte, prefix string, a slog.Attr) []byte {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return buf
	}

	if a.Value.Kind() == slog.KindGroup {
		if a.Key != "" {
			prefix += a.Key + "."
		}

		for _, g := range a.Value.Group() {
			buf = h.appendAttr(buf, prefix, g)
		}

		return buf
	}

	buf = append(buf, ' ')
	buf = h.paint(buf, ansiCyan, prefix+a.Key+"=")

	return append(buf, quoteIfNeeded(a.Value.String())...)
}

func (h *handler) paint(buf []byte, color, s string) []byte {
	if !h.color {
		return append(buf, s...)
	}

	buf = append(buf, color...)
	buf = append(buf, s...)

	return append(buf, ansiReset...)
}

func levelTag(l slog.Level) (string, string) {
	switch {
	case l >= slog.LevelError:
		return "ERR", ansiBoldRed
	case l >= slog.LevelWarn:
		return "WRN", ansiRed
	case l >= slog.LevelInfo:
		return "INF", ansiGreen
	default:
		return "DBG", ansiYellow
	}
}

// quoteIfNeeded keeps a value that holds a space, quote, '=' or control
// character readable as one key=value token.
func quoteIfNeeded(s string) string {
	if s == "" {
		return `""`
	}

	for _, r := range s {
		if unicode.IsSpace(r) || r == '"' || r == '=' || !unicode.IsPrint(r) {
			return strconv.Quote(s)
		}
	}

	return s
}
