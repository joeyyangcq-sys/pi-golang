package infrastructure

import (
	"pi-golang/internal/adapter"
)

// NewLogger is a thin shim over adapter.DefaultSlog. Exposed from the
// infrastructure package so that main() (which wires dependencies) does
// not need to import the adapter package directly for this one call.
func NewLogger(level string) adapter.Logger {
	return adapter.DefaultSlog(level)
}

// NopLogger re-exports the adapter no-op logger so tests need only one import path.
var NopLogger = adapter.NopLogger
