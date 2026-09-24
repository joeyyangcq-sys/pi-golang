package adapter

import (
	"context"
	"log/slog"
	"os"
)

// Logger matches the Logger port declared in internal/usecase/run.go.
// Declared again here so the Adapter layer has a stable seam to target
// without importing the Usecase package (direction of dependencies must
// never point inward).
type Logger interface {
	Debug(ctx context.Context, msg string, args ...any)
	Info(ctx context.Context, msg string, args ...any)
	Warn(ctx context.Context, msg string, args ...any)
	Error(ctx context.Context, msg string, args ...any)
}

// NopLogger is a no-op Logger useful in tests.
var NopLogger Logger = nop{}

type nop struct{}

func (nop) Debug(context.Context, string, ...any) {}
func (nop) Info(context.Context, string, ...any)  {}
func (nop) Warn(context.Context, string, ...any)  {}
func (nop) Error(context.Context, string, ...any) {}

// DefaultSlog returns a Logger backed by log/slog writing JSON to stderr
// at the given level (debug | info | warn | error). Defaults to info.
func DefaultSlog(level string) Logger {
	var lvl slog.Level
	switch level {
	case "debug", "DEBUG":
		lvl = slog.LevelDebug
	case "warn", "WARN", "warning":
		lvl = slog.LevelWarn
	case "error", "ERROR":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	return slogLogger{slog.New(h)}
}

type slogLogger struct{ l *slog.Logger }

func (s slogLogger) Debug(ctx context.Context, m string, a ...any) { s.l.DebugContext(ctx, m, a...) }
func (s slogLogger) Info(ctx context.Context, m string, a ...any)  { s.l.InfoContext(ctx, m, a...) }
func (s slogLogger) Warn(ctx context.Context, m string, a ...any)  { s.l.WarnContext(ctx, m, a...) }
func (s slogLogger) Error(ctx context.Context, m string, a ...any) { s.l.ErrorContext(ctx, m, a...) }
