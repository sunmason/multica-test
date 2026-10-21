package logctx

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

type valueKey struct{}
type contextHandler struct {
	slog.Handler
	seen *bool
}

func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{Handler: h.Handler.WithAttrs(attrs), seen: h.seen}
}
func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	*h.seen = ctx.Value(valueKey{}) == "kept"
	return h.Handler.Handle(ctx, r)
}
func TestLoggerPreservesInjectionTraceAndHandlerContext(t *testing.T) {
	var out bytes.Buffer
	seen := false
	logger := slog.New(contextHandler{Handler: slog.NewJSONHandler(&out, nil), seen: &seen}).With("component", "injected")
	parent, cancel := context.WithCancel(context.WithValue(context.Background(), valueKey{}, "kept"))
	ctx := WithTraceID(WithLogger(parent, logger), "run-id")
	cancel()
	ctx = context.WithoutCancel(ctx)
	Logger(ctx).InfoContext(ctx, "finished")
	var entry map[string]any
	if err := json.Unmarshal(out.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if !seen || entry["trace_id"] != "run-id" || entry["component"] != "injected" || ctx.Err() != nil {
		t.Fatalf("context/log fields lost: seen=%v entry=%v err=%v", seen, entry, ctx.Err())
	}
}
