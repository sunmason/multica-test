// Package logctx carries service log correlation and the injected logger.
package logctx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
)

type traceKey struct{}
type loggerKey struct{}

// NewID returns an opaque identifier for a run or an independent lifecycle.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("logctx: entropy unavailable: %v", err))
	}
	return hex.EncodeToString(b[:])
}

func WithTraceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, traceKey{}, id)
}

func TraceID(ctx context.Context) string {
	id, _ := ctx.Value(traceKey{}).(string)
	return id
}

func WithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, logger)
}

// Logger preserves injection and adds the uniform trace_id field from ctx.
// Use its *Context methods so custom handlers also receive the context.
func Logger(ctx context.Context) *slog.Logger {
	logger, _ := ctx.Value(loggerKey{}).(*slog.Logger)
	if logger == nil {
		logger = slog.Default()
	}
	if id := TraceID(ctx); id != "" {
		return logger.With("trace_id", id)
	}
	return logger
}
