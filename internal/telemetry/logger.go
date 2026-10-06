package telemetry

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	defaultLogger *slog.Logger
	mu            sync.RWMutex
)

// flushClose drains a buffered writer before closing the file behind it, so
// the run's log tail is never lost to a buffer that was never flushed. Order
// matters: flush first, then close.
type flushClose struct {
	*bufio.Writer
	file *os.File
}

func (f flushClose) Close() error {
	err := f.Writer.Flush()
	if cerr := f.file.Close(); err == nil {
		err = cerr
	}
	return err
}

func init() {
	// Fallback text logger to stderr before explicit initialization.
	defaultLogger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level:       slog.LevelInfo,
		AddSource:   true,
		ReplaceAttr: humanize,
	}))
}

// MultiHandler multiplexes log records across multiple slog.Handler instances.
type MultiHandler struct {
	handlers []slog.Handler
}

// NewMultiHandler creates a handler broadcasting to all provided handlers.
func NewMultiHandler(handlers ...slog.Handler) *MultiHandler {
	return &MultiHandler{handlers: handlers}
}

// Enabled returns true if any handler accepts the given level.
func (m *MultiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

// Handle sends the record to all configured handlers.
func (m *MultiHandler) Handle(ctx context.Context, r slog.Record) error {
	var firstErr error
	for _, h := range m.handlers {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r.Clone()); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// WithAttrs returns a new MultiHandler with attributes added to all child handlers.
func (m *MultiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newHandlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		newHandlers[i] = h.WithAttrs(attrs)
	}
	return &MultiHandler{handlers: newHandlers}
}

// WithGroup returns a new MultiHandler with group added to all child handlers.
func (m *MultiHandler) WithGroup(name string) slog.Handler {
	newHandlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		newHandlers[i] = h.WithGroup(name)
	}
	return &MultiHandler{handlers: newHandlers}
}

// Config specifies options for telemetry initialization.
type Config struct {
	ConsoleWriter io.Writer
	LogDir        string
	RunID         string
	Verbose       bool
}

// shortTime renders log timestamps as HH:MM:SS on the human-readable
// surfaces (console, .log file): the run's date and id already live in the
// log file name (run-<DDMMYYYY_HHMMSS>.log), so repeating the date per line
// is noise. The JSONL machine copy keeps full precision.
func shortTime(groups []string, a slog.Attr) slog.Attr {
	if a.Key == slog.TimeKey && len(groups) == 0 {
		if t, ok := a.Value.Any().(time.Time); ok {
			a.Value = slog.StringValue(t.Format("15:04:05"))
		}
	}
	return a
}

// callerAttr turns the built-in source attribute into a traditional caller
// location, "pkg.Func file.go:123", under the caller= key. The key is not
// "source" because call sites already use source= for the input file path.
func callerAttr(groups []string, a slog.Attr) slog.Attr {
	if a.Key != slog.SourceKey || len(groups) != 0 {
		return a
	}
	src, ok := a.Value.Any().(*slog.Source)
	if !ok {
		return a
	}
	a.Key = "caller"
	a.Value = slog.StringValue(fmt.Sprintf("%s %s:%d", shortFunc(src.Function), shortFile(src.File), src.Line))
	return a
}

// humanize applies the human-surface tweaks: the short day-first stamp plus
// the caller location.
func humanize(groups []string, a slog.Attr) slog.Attr {
	return callerAttr(groups, shortTime(groups, a))
}

// shortFunc trims a fully-qualified function name to its package-qualified
// form, e.g. "github.com/x/y/cmd/run.Main" -> "run.Main".
func shortFunc(fn string) string {
	if i := strings.LastIndexByte(fn, '/'); i >= 0 {
		return fn[i+1:]
	}
	return fn
}

// shortFile keeps the last two path elements of a source file, e.g.
// "/home/x/proj/cmd/run.go" -> "cmd/run.go".
func shortFile(file string) string {
	parts := strings.Split(filepath.ToSlash(file), "/")
	if len(parts) >= 2 {
		return strings.Join(parts[len(parts)-2:], "/")
	}
	return file
}

// Init configures the global logger and optional file logging.
// Every run writes three surfaces: console (live human view), a JSONL file
// (machine copy for the audit/tuning loop), and a .log text file (human
// twin). Returns a cleanup function that closes log files.
func Init(cfg Config) (func(), error) {
	mu.Lock()
	defer mu.Unlock()

	var closers []io.Closer
	var handlers []slog.Handler

	consoleLevel := slog.LevelInfo
	if cfg.Verbose {
		consoleLevel = slog.LevelDebug
	}

	consoleOut := cfg.ConsoleWriter
	if consoleOut == nil {
		consoleOut = os.Stderr
	}

	consoleHandler := slog.NewTextHandler(consoleOut, &slog.HandlerOptions{
		Level:       consoleLevel,
		AddSource:   true,
		ReplaceAttr: humanize,
	})
	handlers = append(handlers, consoleHandler)

	if cfg.LogDir != "" && cfg.RunID != "" {
		if err := os.MkdirAll(cfg.LogDir, 0o755); err != nil {
			return func() {}, err
		}

		jsonFile, err := os.OpenFile(filepath.Join(cfg.LogDir, "run-"+cfg.RunID+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return func() {}, err
		}
		// Buffer the machine copy. slog's handlers hold no buffer of their
		// own and serialize per record, so an unbuffered *os.File made
		// every log line a write(2) — a convert run emits hundreds per
		// service. The flushClose closer drains the buffer before closing.
		jsonBuf := bufio.NewWriterSize(jsonFile, 64<<10)
		closers = append(closers, flushClose{Writer: jsonBuf, file: jsonFile})

		jsonHandler := slog.NewJSONHandler(jsonBuf, &slog.HandlerOptions{
			Level:       slog.LevelDebug, // machine copy always captures full debug detail
			AddSource:   true,
			ReplaceAttr: callerAttr, // keep full-precision time on the machine copy
		})
		// The run id is in every log file name; the human surfaces drop it
		// per line, but the machine copy keeps it in-record so concatenated
		// JSONL stays attributable.
		handlers = append(handlers, jsonHandler.WithAttrs([]slog.Attr{slog.String("run_id", cfg.RunID)}))

		textFile, err := os.OpenFile(filepath.Join(cfg.LogDir, "run-"+cfg.RunID+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return func() {}, err
		}
		textBuf := bufio.NewWriterSize(textFile, 64<<10)
		closers = append(closers, flushClose{Writer: textBuf, file: textFile})

		textHandler := slog.NewTextHandler(textBuf, &slog.HandlerOptions{
			Level:       slog.LevelDebug,
			AddSource:   true,
			ReplaceAttr: humanize,
		})
		handlers = append(handlers, textHandler)
	}

	composite := NewMultiHandler(handlers...)
	defaultLogger = slog.New(composite)

	cleanup := func() {
		for _, c := range closers {
			_ = c.Close()
		}
	}

	return cleanup, nil
}

// Log returns the default logger. Run scoping rides the log file names
// (and the in-record run_id on the JSONL machine copy), so no per-line
// run_id is attached here anymore.
func Log(ctx context.Context) *slog.Logger {
	mu.RLock()
	defer mu.RUnlock()
	return defaultLogger
}
