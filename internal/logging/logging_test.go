package logging

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func record(level slog.Level, msg string, attrs ...slog.Attr) slog.Record {
	r := slog.NewRecord(time.Date(2026, 10, 11, 13, 4, 5, 0, time.UTC), level, msg, 0)
	r.AddAttrs(attrs...)

	return r
}

func TestHandlerPlainLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    slog.Record
		want string
	}{
		{
			"info with fields in call order",
			record(slog.LevelInfo, "emitted Nextflow project",
				slog.String("source", "p.mro"), slog.Int("stages", 3), slog.String("out", "/tmp/x")),
			"1:04PM INF emitted Nextflow project source=p.mro stages=3 out=/tmp/x\n",
		},
		{"warn", record(slog.LevelWarn, "flag conflict"), "1:04PM WRN flag conflict\n"},
		{"error", record(slog.LevelError, "bad"), "1:04PM ERR bad\n"},
		{
			"values that would split a token are quoted",
			record(slog.LevelInfo, "m", slog.String("a", "x y"), slog.String("b", ""), slog.String("c", "k=v")),
			`1:04PM INF m a="x y" b="" c="k=v"` + "\n",
		},
		{
			"group attrs are prefixed and empty attrs dropped",
			record(slog.LevelInfo, "m", slog.Group("g", slog.Int("n", 1)), slog.Attr{}),
			"1:04PM INF m g.n=1\n",
		},
		{"zero time is omitted", slog.NewRecord(time.Time{}, slog.LevelInfo, "m", 0), "INF m\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := newHandler(&buf, slog.LevelInfo, false).Handle(context.Background(), tc.r); err != nil {
				t.Fatalf("handle: %v", err)
			}

			if diff := cmp.Diff(tc.want, buf.String()); diff != "" {
				t.Errorf("line (-want +got):\n%s", diff)
			}
		})
	}
}

func TestHandlerColor(t *testing.T) {
	var buf bytes.Buffer

	r := record(slog.LevelInfo, "m", slog.Int("n", 1))
	if err := newHandler(&buf, slog.LevelInfo, true).Handle(context.Background(), r); err != nil {
		t.Fatalf("handle: %v", err)
	}

	want := "\x1b[90m1:04PM\x1b[0m \x1b[32mINF\x1b[0m \x1b[1mm\x1b[0m \x1b[36mn=\x1b[0m1\n"
	if diff := cmp.Diff(want, buf.String()); diff != "" {
		t.Errorf("line (-want +got):\n%s", diff)
	}
}

func TestHandlerWithAttrsAndGroup(t *testing.T) {
	var buf bytes.Buffer

	log := slog.New(newHandler(&buf, slog.LevelInfo, false)).With("run", "r1").WithGroup("call").With("name", "A")
	log.Info("m", "n", 2)

	_, got, _ := strings.Cut(buf.String(), " ")
	if diff := cmp.Diff("INF m run=r1 call.name=A call.n=2\n", got); diff != "" {
		t.Errorf("line (-want +got):\n%s", diff)
	}
}

func TestNewLoggerLevel(t *testing.T) {
	for _, tc := range []struct {
		level     string
		wantDebug bool
		wantInfo  bool
	}{
		{"", false, true},
		{"debug", true, true},
		{"warn", false, false},
		{"bogus", false, true},
	} {
		t.Run(tc.level, func(t *testing.T) {
			f := tempFile(t)
			log := newLogger(f, env(map[string]string{"LOG_LEVEL": tc.level}))

			if got := log.Enabled(context.Background(), slog.LevelDebug); got != tc.wantDebug {
				t.Errorf("debug enabled = %v, want %v", got, tc.wantDebug)
			}

			if got := log.Enabled(context.Background(), slog.LevelInfo); got != tc.wantInfo {
				t.Errorf("info enabled = %v, want %v", got, tc.wantInfo)
			}
		})
	}
}

// TestNewLoggerNoEscapesWhenNotTerminal is the #281/#313 regression: captured
// or piped stderr must carry no ANSI escape codes.
func TestNewLoggerNoEscapesWhenNotTerminal(t *testing.T) {
	f := tempFile(t)
	newLogger(f, env(nil)).Warn("flag conflict", "flag", "-native")

	got, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("read log: %v", err)
	}

	if bytes.Contains(got, []byte("\x1b")) {
		t.Errorf("non-terminal output carries ANSI escapes: %q", got)
	}

	if !bytes.Contains(got, []byte("WRN flag conflict flag=-native\n")) {
		t.Errorf("log line = %q, want the WRN record", got)
	}
}

func TestUseColor(t *testing.T) {
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() { _ = devNull.Close() })

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })

	for _, tc := range []struct {
		name string
		f    *os.File
		env  map[string]string
		want bool
	}{
		{"character device", devNull, nil, true},
		{"character device with NO_COLOR", devNull, map[string]string{"NO_COLOR": "1"}, false},
		{"regular file", tempFile(t), nil, false},
		{"pipe", w, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := useColor(tc.f, env(tc.env)); got != tc.want {
				t.Errorf("useColor = %v, want %v", got, tc.want)
			}
		})
	}
}

func tempFile(t *testing.T) *os.File {
	t.Helper()

	f, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	return f
}

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}
