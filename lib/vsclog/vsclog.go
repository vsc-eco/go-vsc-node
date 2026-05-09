package vsclog

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-isatty"
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
	colorEnabled  bool
)

const (
	ansiReset   = "\x1b[0m"
	ansiBold    = "\x1b[1m"
	ansiDim     = "\x1b[2m"
	ansiError   = "\x1b[1;31m" // bold red
	ansiWarn    = "\x1b[33m"   // yellow
	ansiInfo    = "\x1b[32m"   // green
	ansiDebug   = "\x1b[36m"   // cyan
	ansiVerbose = "\x1b[34m"   // blue
	ansiTrace   = "\x1b[90m"   // bright black / dim
)

var levelColors = []struct {
	tag   []byte
	color string
}{
	{[]byte("[ERROR]"), ansiError},
	{[]byte("[WARN]"), ansiWarn},
	{[]byte("[INFO]"), ansiInfo},
	{[]byte("[DEBUG]"), ansiDebug},
	{[]byte("[VERBOSE]"), ansiVerbose},
	{[]byte("[TRACE]"), ansiTrace},
}

func init() {
	colorEnabled = os.Getenv("NO_COLOR") == "" && isatty.IsTerminal(os.Stderr.Fd())
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

// stripTimeKeyWriter post-processes slog TextHandler output: strips the
// "time=", "level=", and "msg=" key envelopes, and applies ANSI tinting
// (dim timestamp, colored level tag, bold message, dim attribute keys)
// when stderr is a TTY.
type stripTimeKeyWriter struct{}

func (stripTimeKeyWriter) Write(p []byte) (int, error) {
	out := formatLine(p)
	_, err := os.Stderr.Write(out)
	return len(p), err
}

// formatLine parses one slog text-handler record and rewrites it.
// On any deviation from the expected `time=… level=[X] msg=… <attrs>`
// shape, it returns the input unchanged.
func formatLine(p []byte) []byte {
	body := p
	var trailingNL bool
	if n := len(body); n > 0 && body[n-1] == '\n' {
		body = body[:n-1]
		trailingNL = true
	}

	if !bytes.HasPrefix(body, []byte("time=")) {
		return p
	}
	body = body[len("time="):]

	spaceIdx := bytes.IndexByte(body, ' ')
	if spaceIdx < 0 {
		return p
	}
	timestamp := body[:spaceIdx]
	body = body[spaceIdx+1:]

	if !bytes.HasPrefix(body, []byte("level=")) {
		return p
	}
	body = body[len("level="):]

	if len(body) == 0 || body[0] != '[' {
		return p
	}
	closeBracket := bytes.IndexByte(body, ']')
	if closeBracket < 0 {
		return p
	}
	level := body[:closeBracket+1]
	body = body[closeBracket+1:]

	if len(body) > 0 && body[0] == ' ' {
		body = body[1:]
	}

	msg, attrs := extractMessage(body)

	out := bytes.NewBuffer(make([]byte, 0, len(p)+64))
	if colorEnabled {
		out.WriteString(ansiDim)
		out.Write(timestamp)
		out.WriteString(ansiReset)
		out.WriteByte(' ')
		if c := levelColor(level); c != "" {
			out.WriteString(c)
			out.Write(level)
			out.WriteString(ansiReset)
		} else {
			out.Write(level)
		}
		if len(msg) > 0 {
			out.WriteByte(' ')
			out.WriteString(ansiBold)
			out.Write(msg)
			out.WriteString(ansiReset)
		}
		if len(attrs) > 0 {
			if msg == nil && attrs[0] != ' ' {
				out.WriteByte(' ')
			}
			out.Write(walkAttrs(attrs))
		}
	} else {
		out.Write(timestamp)
		out.WriteByte(' ')
		out.Write(level)
		if len(msg) > 0 {
			out.WriteByte(' ')
			out.Write(msg)
		}
		if len(attrs) > 0 {
			if msg == nil && attrs[0] != ' ' {
				out.WriteByte(' ')
			}
			out.Write(attrs)
		}
	}

	if trailingNL {
		out.WriteByte('\n')
	}
	return out.Bytes()
}

// extractMessage parses a "msg=VALUE" prefix and returns the unquoted
// message bytes plus the trailing attrs bytes (with the leading space
// preserved). If body does not start with "msg=", returns (nil, body).
// An empty quoted msg ("") returns a non-nil zero-length slice so the
// caller can distinguish "msg= present but empty" from "no msg= field".
func extractMessage(body []byte) (msg, attrs []byte) {
	if !bytes.HasPrefix(body, []byte("msg=")) {
		return nil, body
	}
	body = body[len("msg="):]
	if len(body) == 0 {
		return body[:0], nil
	}
	if body[0] == '"' {
		end := 1
		for end < len(body) {
			if body[end] == '\\' && end+1 < len(body) {
				end += 2
				continue
			}
			if body[end] == '"' {
				end++
				break
			}
			end++
		}
		if unquoted, err := strconv.Unquote(string(body[:end])); err == nil {
			msg = []byte(unquoted)
		} else if end >= 2 {
			msg = body[1 : end-1]
		} else {
			msg = body[:0]
		}
		attrs = body[end:]
		return msg, attrs
	}
	end := bytes.IndexAny(body, " \n")
	if end < 0 {
		return body, nil
	}
	return body[:end], body[end:]
}

// walkAttrs scans the attrs portion of a slog record and dims each
// "key=" prefix. The caller has already verified colorEnabled. Recognises
// the four value shapes slog can emit: bare token, "quoted", [bracketed
// slice], and "[quoted bracketed slice]".
func walkAttrs(p []byte) []byte {
	out := bytes.NewBuffer(make([]byte, 0, len(p)+32))
	i := 0
	for i < len(p) {
		for i < len(p) && (p[i] == ' ' || p[i] == '\t') {
			out.WriteByte(p[i])
			i++
		}
		if i >= len(p) {
			break
		}
		eq := -1
		for j := i; j < len(p); j++ {
			c := p[j]
			if c == '=' {
				eq = j
				break
			}
			if c == ' ' || c == '\n' {
				break
			}
		}
		if eq < 0 {
			out.Write(p[i:])
			break
		}
		out.WriteString(ansiDim)
		out.Write(p[i : eq+1])
		out.WriteString(ansiReset)
		i = eq + 1
		if i >= len(p) {
			break
		}
		switch p[i] {
		case '"':
			j := i + 1
			for j < len(p) {
				if p[j] == '\\' && j+1 < len(p) {
					j += 2
					continue
				}
				if p[j] == '"' {
					j++
					break
				}
				j++
			}
			out.Write(p[i:j])
			i = j
		case '[':
			j := i + 1
			depth := 1
			for j < len(p) && depth > 0 {
				switch p[j] {
				case '[':
					depth++
				case ']':
					depth--
				}
				j++
			}
			out.Write(p[i:j])
			i = j
		default:
			j := i
			for j < len(p) && p[j] != ' ' && p[j] != '\n' {
				j++
			}
			out.Write(p[i:j])
			i = j
		}
	}
	return out.Bytes()
}

func levelColor(level []byte) string {
	for _, lc := range levelColors {
		if bytes.Equal(level, lc.tag) {
			return lc.color
		}
	}
	return ""
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
