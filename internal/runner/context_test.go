package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"golang-sandbox/internal/logctx"
)

func TestLocalWarmupUsesContextLogger(t *testing.T) {
	l := newLocalOrSkip(t)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil)).With("component", "injected")
	ctx := logctx.WithTraceID(logctx.WithLogger(context.Background(), logger), "startup")
	l.Warmup(ctx)
	var entry map[string]any
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["trace_id"] != "startup" || entry["component"] != "injected" || entry["msg"] != "local build cache warmed" {
		t.Fatalf("warmup lost injected logger or context trace: %v", entry)
	}
}
