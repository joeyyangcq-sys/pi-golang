package adapter

import (
	"context"
	"log/slog"
	"os"
)

// Logger 与 internal/usecase/run.go 里声明的 Logger 端口一致。
// 在此再声明一次，让 Adapter 层有一个稳定的接缝目标，而无需导入
// Usecase 包（依赖方向绝不向内指）。
type Logger interface {
	Debug(ctx context.Context, msg string, args ...any)
	Info(ctx context.Context, msg string, args ...any)
	Warn(ctx context.Context, msg string, args ...any)
	Error(ctx context.Context, msg string, args ...any)
}

// NopLogger 是测试中可用的 no-op Logger。
var NopLogger Logger = nop{}

type nop struct{}

func (nop) Debug(context.Context, string, ...any) {}
func (nop) Info(context.Context, string, ...any)  {}
func (nop) Warn(context.Context, string, ...any)  {}
func (nop) Error(context.Context, string, ...any) {}

// DefaultSlog 返回一个由 log/slog 支撑的 Logger，向 stderr 输出 JSON，
// 级别由 level 指定（debug | info | warn | error），默认 info。
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
