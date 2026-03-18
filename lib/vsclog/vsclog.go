package vsclog

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// LevelVerbose is for detailed operational logs: session lifecycle,
	// DB operations, blame tracking, readiness checks, block details.
	LevelVerbose = slog.Level(-8)

	// LevelTrace is for high-frequency per-message logs: P2P messages,
	// RPC calls, individual transaction processing, retry attempts.
	LevelTrace = slog.Level(-12)
)

// Logger wraps *slog.Logger to add Verbose and Trace convenience methods.
type Logger struct {
	*slog.Logger
}

// Verbose logs at LevelVerbose.
func (l *Logger) Verbose(msg string, args ...any) {
	l.Log(context.Background(), LevelVerbose, msg, args...)
}

// Trace logs at LevelTrace.
func (l *Logger) Trace(msg string, args ...any) {
	l.Log(context.Background(), LevelTrace, msg, args...)
}

// With returns a new Logger with the given attributes.
func (l *Logger) With(args ...any) *Logger {
	return &Logger{l.Logger.With(args...)}
}

// --- global state ---

var (
	mu            sync.RWMutex
	defaultLevel  = slog.LevelError
	moduleLevels  = map[string]slog.Level{}
	moduleLoggers = map[string]*Logger{}
	baseHandler   slog.Handler
)

func init() {
	rebuildHandler()
}

// Module returns a named logger. Repeated calls with the same name return
// the same *Logger instance. The logger carries a "module" attribute and
// respects the per-module level set via SetModuleLevel or parsed from the
// --log-level flag.
func Module(name string) *Logger {
	mu.RLock()
	if l, ok := moduleLoggers[name]; ok {
		mu.RUnlock()
		return l
	}
	mu.RUnlock()

	mu.Lock()
	defer mu.Unlock()
	// double-check after write lock
	if l, ok := moduleLoggers[name]; ok {
		return l
	}
	l := &Logger{slog.New(&moduleHandler{
		base:   baseHandler,
		module: name,
	}).With("module", name)}
	moduleLoggers[name] = l
	return l
}

// SetDefaultLevel sets the level for modules without an explicit override.
func SetDefaultLevel(level slog.Level) {
	mu.Lock()
	defaultLevel = level
	mu.Unlock()
	rebuildAll()
}

// SetModuleLevel sets the level for a specific module.
func SetModuleLevel(module string, level slog.Level) {
	mu.Lock()
	moduleLevels[module] = level
	mu.Unlock()
	rebuildAll()
}

// ParseAndApply parses a log level string and applies it. The format is:
//
//	"info"                          — set default to info
//	"error,tss=verbose,bp=info"    — default error, tss verbose, bp info
//
// Supported level names: error, warn, info, debug, verbose, trace.
func ParseAndApply(spec string) {
	parts := strings.Split(spec, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if k, v, ok := strings.Cut(part, "="); ok {
			level, err := ParseLevel(v)
			if err != nil {
				fmt.Fprintf(os.Stderr, "unknown log level %q for module %q, skipping\n", v, k)
				continue
			}
			SetModuleLevel(strings.TrimSpace(k), level)
		} else {
			level, err := ParseLevel(part)
			if err != nil {
				fmt.Fprintf(os.Stderr, "unknown log level %q, defaulting to error\n", part)
				level = slog.LevelError
			}
			mu.Lock()
			defaultLevel = level
			mu.Unlock()
		}
	}
	rebuildAll()
}

// ParseLevel converts a level name to a slog.Level.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "trace":
		return LevelTrace, nil
	case "verbose":
		return LevelVerbose, nil
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelError, fmt.Errorf("unknown level: %s", s)
	}
}

// --- internal ---

// moduleHandler is an slog.Handler that filters based on per-module levels.
type moduleHandler struct {
	base   slog.Handler
	module string
}

func (h *moduleHandler) Enabled(_ context.Context, level slog.Level) bool {
	mu.RLock()
	minLevel := defaultLevel
	if ml, ok := moduleLevels[h.module]; ok {
		minLevel = ml
	}
	mu.RUnlock()
	return level >= minLevel
}

func (h *moduleHandler) Handle(ctx context.Context, r slog.Record) error {
	return h.base.Handle(ctx, r)
}

func (h *moduleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &moduleHandler{base: h.base.WithAttrs(attrs), module: h.module}
}

func (h *moduleHandler) WithGroup(name string) slog.Handler {
	return &moduleHandler{base: h.base.WithGroup(name), module: h.module}
}

// stripTimeKeyWriter strips the "time=" key prefix emitted by slog's
// TextHandler so the timestamp appears bare at the start of each line.
type stripTimeKeyWriter struct{}

func (stripTimeKeyWriter) Write(p []byte) (int, error) {
	p = bytes.Replace(p, []byte("time="), nil, 1)
	p = bytes.Replace(p, []byte("level="), nil, 1)
	_, err := os.Stderr.Write(p)
	return len(p), err
}

func rebuildHandler() {
	baseHandler = slog.NewTextHandler(stripTimeKeyWriter{}, &slog.HandlerOptions{
		// Set to lowest possible so the moduleHandler.Enabled controls filtering.
		Level: LevelTrace,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				if t, ok := a.Value.Any().(time.Time); ok {
					a.Value = slog.StringValue(t.UTC().Format("2006-01-02T15:04:05.000Z"))
				}
			}
			if a.Key == slog.LevelKey {
				l := a.Value.Any().(slog.Level)
				switch {
				case l <= LevelTrace:
					a.Value = slog.StringValue("[TRACE]")
				case l <= LevelVerbose:
					a.Value = slog.StringValue("[VERBOSE]")
				default:
					a.Value = slog.StringValue("[" + a.Value.String() + "]")
				}
			}
			return a
		},
	})
}

func rebuildAll() {
	mu.Lock()
	defer mu.Unlock()
	rebuildHandler()
	for name, l := range moduleLoggers {
		*l = Logger{slog.New(&moduleHandler{
			base:   baseHandler,
			module: name,
		}).With("module", name)}
	}
}
