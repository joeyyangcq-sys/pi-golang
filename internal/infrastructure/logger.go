package infrastructure

import (
	"pi-golang/internal/adapter"
)

// NewLogger 是 adapter.DefaultSlog 的薄封装。从 infrastructure 包暴露，
// 让 main()（负责接线依赖）不必为这一个调用直接 import adapter。
func NewLogger(level string) adapter.Logger {
	return adapter.DefaultSlog(level)
}

// NopLogger 再导出 adapter 的 no-op logger，让测试只需一个 import 路径。
var NopLogger = adapter.NopLogger
