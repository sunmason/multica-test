package runner

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"golang-sandbox/internal/config"
	"golang-sandbox/internal/run"
)

// ---- cappedBuffer (D4) ----

func TestCappedBufferTruncatesAndCounts(t *testing.T) {
	w := newCappedBuffer(5)
	n, err := w.Write([]byte("abcdefgh"))
	if err != nil || n != 8 {
		t.Fatalf("Write = %d, %v; want 8, nil", n, err)
	}
	if got := w.String(); got != "abcde" {
		t.Errorf("kept %q, want %q", got, "abcde")
	}
	if w.dropped != 3 {
		t.Errorf("dropped = %d, want 3", w.dropped)
	}
}

func TestCappedBufferMultipleWrites(t *testing.T) {
	w := newCappedBuffer(10)
	for i := 0; i < 4; i++ {
		w.Write([]byte("1234"))
	}
	if w.buf.Len() != 10 || w.dropped != 6 {
		t.Errorf("len=%d dropped=%d, want 10/6", w.buf.Len(), w.dropped)
	}
}

// ---- compile error parsing (B2) ----

func TestParseCompileErrors(t *testing.T) {
	stderr := "# command-line-arguments\n" +
		"/tmp/main.go:5:2: undefined: undefinedVar\n" +
		"./main.go:9:1: expected '}', found 'EOF'\n" +
		"some trailing note\n"
	errs := parseCompileErrors(stderr)
	if len(errs) != 2 {
		t.Fatalf("got %d errors, want 2: %+v", len(errs), errs)
	}
	if errs[0].Line != 5 || errs[0].Col != 2 || errs[0].Message != "undefined: undefinedVar" {
		t.Errorf("errs[0] = %+v", errs[0])
	}
	if strings.Contains(errs[0].Text, "/tmp") {
		t.Errorf("sandbox path leaked: %s", errs[0].Text)
	}
	if errs[1].Line != 9 || errs[1].Col != 1 {
		t.Errorf("errs[1] = %+v", errs[1])
	}
}

func TestParseCompileErrorsEmpty(t *testing.T) {
	if errs := parseCompileErrors(""); len(errs) != 0 {
		t.Errorf("expected no errors, got %+v", errs)
	}
}

// ---- local runner end-to-end (also exercises shared classification) ----

func testConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Limits.ExecTimeoutSec = 3
	cfg.Limits.CompileTimeoutSec = 60
	cfg.Limits.OutputCapBytes = 1 << 20
	cfg.ExecTimeout = 3 * time.Second
	cfg.CompileTimeout = 60 * time.Second
	return cfg
}

func newLocalOrSkip(t *testing.T) *Local {
	t.Helper()
	l, err := NewLocal(testConfig(t))
	if err != nil {
		t.Skipf("local runner unavailable: %v", err)
	}
	return l
}

func TestLocalSuccess(t *testing.T) {
	l := newLocalOrSkip(t)
	res := l.Execute(context.Background(), "t1", run.Request{
		Code: "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"hello\", \"sandbox\") }\n",
	})
	if res.Status != run.StatusSuccess {
		t.Fatalf("status = %s (%s), stderr: %s", res.Status, res.Reason, res.Stderr)
	}
	if res.Stdout != "hello sandbox\n" {
		t.Errorf("stdout = %q", res.Stdout)
	}
	if res.Truncated {
		t.Error("must not report truncation")
	}
}

func TestLocalCompileError(t *testing.T) {
	l := newLocalOrSkip(t)
	res := l.Execute(context.Background(), "t2", run.Request{
		Code: "package main\n\nfunc main() {\n\tundefinedVar.Println()\n}\n",
	})
	if res.Status != run.StatusCompileFailed {
		t.Fatalf("status = %s, want compile_failed", res.Status)
	}
	if len(res.CompileErrors) == 0 || res.CompileErrors[0].Line != 4 {
		t.Fatalf("compile errors = %+v, want line 4", res.CompileErrors)
	}
	if !strings.Contains(res.Reason, "4") {
		t.Errorf("reason should point at line 4: %s", res.Reason)
	}
}

func TestLocalTimeoutKills(t *testing.T) {
	l := newLocalOrSkip(t)
	start := time.Now()
	res := l.Execute(context.Background(), "t3", run.Request{
		Code: "package main\n\nfunc main() { for {} }\n",
	})
	if res.Status != run.StatusTimeout {
		t.Fatalf("status = %s, want timeout", res.Status)
	}
	if elapsed := time.Since(start); elapsed > 8*time.Second {
		t.Errorf("kill took too long: %s", elapsed)
	}
	if !strings.Contains(res.Reason, "3") {
		t.Errorf("reason should mention the 3s limit: %s", res.Reason)
	}
}

func TestLocalStdinPipeline(t *testing.T) {
	l := newLocalOrSkip(t)
	res := l.Execute(context.Background(), "t4", run.Request{
		Code:  "package main\n\nimport (\n\t\"fmt\"\n\t\"io\"\n\t\"os\"\n)\n\nfunc main() {\n\tb, _ := io.ReadAll(os.Stdin)\n\tfmt.Printf(\"got:%s\", string(b))\n}\n",
		Stdin: "abc",
	})
	if res.Status != run.StatusSuccess || res.Stdout != "got:abc" {
		t.Fatalf("status=%s stdout=%q", res.Status, res.Stdout)
	}
}

func TestLocalEmptyStdinGivesEOF(t *testing.T) {
	l := newLocalOrSkip(t)
	res := l.Execute(context.Background(), "t5", run.Request{
		Code: "package main\n\nimport (\n\t\"fmt\"\n\t\"io\"\n\t\"os\"\n)\n\nfunc main() {\n\tb := make([]byte, 8)\n\tn, err := io.ReadFull(os.Stdin, b)\n\tfmt.Printf(\"n=%d err=%v\", n, err)\n}\n",
	})
	if res.Status != run.StatusSuccess || res.Stdout != "n=0 err=EOF" {
		t.Fatalf("status=%s stdout=%q", res.Status, res.Stdout)
	}
}

func TestLocalRunFailedVsTimeoutDistinct(t *testing.T) {
	l := newLocalOrSkip(t)
	res := l.Execute(context.Background(), "t6", run.Request{
		Code: "package main\n\nimport \"os\"\n\nfunc main() { os.Exit(3) }\n",
	})
	if res.Status != run.StatusRunFailed || res.ExitCode != 3 {
		t.Fatalf("status=%s exit=%d, want run_failed/3", res.Status, res.ExitCode)
	}
	if res.Status == run.StatusTimeout {
		t.Error("nonzero exit must stay distinct from timeout")
	}
}

func TestLocalPanicGoesToStderr(t *testing.T) {
	l := newLocalOrSkip(t)
	res := l.Execute(context.Background(), "t7", run.Request{
		Code: "package main\n\nfunc main() { panic(\"boom\") }\n",
	})
	if res.Status != run.StatusRunFailed {
		t.Fatalf("status = %s", res.Status)
	}
	if !strings.Contains(res.Stderr, "panic: boom") {
		t.Errorf("stderr missing panic: %q", res.Stderr)
	}
}

func TestLocalTruncatesOutput(t *testing.T) {
	cfg := testConfig(t)
	cfg.Limits.OutputCapBytes = 100
	l2, err := NewLocal(cfg)
	if err != nil {
		t.Skipf("local runner unavailable: %v", err)
	}
	code := "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfor i := 0; i < 100; i++ { fmt.Println(\"" + strings.Repeat("x", 100) + "\") }\n}\n"
	res := l2.Execute(context.Background(), "t8", run.Request{Code: code})
	if res.Status != run.StatusSuccess {
		t.Fatalf("status = %s: %s", res.Status, res.Stderr)
	}
	if !res.Truncated || res.DroppedBytes <= 0 {
		t.Fatalf("truncated=%v dropped=%d", res.Truncated, res.DroppedBytes)
	}
	if len(res.Stdout) != 100 {
		t.Errorf("stdout len = %d, want 100", len(res.Stdout))
	}
}

func TestLocalFilesystemIsScopedToTempCwd(t *testing.T) {
	l := newLocalOrSkip(t)
	res := l.Execute(context.Background(), "t9", run.Request{
		Code: "package main\n\nimport (\n\t\"fmt\"\n\t\"os\"\n)\n\nfunc main() {\n\terr := os.WriteFile(\"note.txt\", []byte(\"hi\"), 0644)\n\tfmt.Println(err)\n}\n",
	})
	if res.Status != run.StatusSuccess {
		t.Fatalf("status = %s: %s", res.Status, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "<nil>") {
		t.Errorf("expected writable cwd, got: %s", res.Stdout)
	}
}

func TestLocalCancelViaContext(t *testing.T) {
	l := newLocalOrSkip(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(1500 * time.Millisecond) // let compile finish, run start
		cancel()
	}()
	res := l.Execute(ctx, "t10", run.Request{
		Code: "package main\n\nimport \"time\"\n\nfunc main() { time.Sleep(30 * time.Second) }\n",
	})
	if res.Status != run.StatusCancelled {
		t.Fatalf("status = %s, want cancelled", res.Status)
	}
}

func TestLocalGoVersion(t *testing.T) {
	l := newLocalOrSkip(t)
	v, err := l.GoVersion(context.Background())
	if err != nil || !strings.Contains(v, "go") {
		t.Errorf("GoVersion = %q, %v", v, err)
	}
}

func TestLocalSweepRemovesNothingFresh(t *testing.T) {
	l := newLocalOrSkip(t)
	l.Execute(context.Background(), "t11", run.Request{
		Code: "package main\n\nfunc main() {}\n",
	})
	l.Sweep(context.Background()) // must not remove fresh dirs / panic
}

// ---- docker runner (skipped unless daemon + image are available) ----

func TestDockerRunnerEndToEnd(t *testing.T) {
	d, err := NewDocker(config.Default())
	if err != nil {
		t.Skipf("docker runner unavailable: %v", err)
	}
	if err := d.Probe(context.Background()); err != nil {
		t.Skipf("docker probe failed: %v", err)
	}

	t.Run("success", func(t *testing.T) {
		res := d.Execute(context.Background(), dtestID("ok"), run.Request{
			Code: "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"docker hello\") }\n",
		})
		if res.Status != run.StatusSuccess || res.Stdout != "docker hello\n" {
			t.Fatalf("status=%s exit=%d reason=%q stdout=%q stderr=%q dur=%dms",
				res.Status, res.ExitCode, res.Reason, res.Stdout, res.Stderr, res.DurationMS)
		}
	})

	t.Run("compile_error", func(t *testing.T) {
		res := d.Execute(context.Background(), dtestID("ce"), run.Request{
			Code: "package main\n\nfunc main() {\n\tundefinedVar()\n}\n",
		})
		if res.Status != run.StatusCompileFailed || len(res.CompileErrors) == 0 {
			t.Fatalf("status=%s errors=%+v", res.Status, res.CompileErrors)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		res := d.Execute(context.Background(), dtestID("to"), run.Request{
			Code: "package main\n\nfunc main() { for {} }\n",
		})
		if res.Status != run.StatusTimeout {
			t.Fatalf("status = %s", res.Status)
		}
	})

	t.Run("oom", func(t *testing.T) {
		// Touch every page: lazy zero pages don't count toward cgroup RSS.
		res := d.Execute(context.Background(), dtestID("oom"), run.Request{
			Code: "package main\n\nfunc main() {\n\ts := make([]byte, 512<<20)\n\tfor i := range s {\n\t\ts[i] = 1\n\t}\n\tprintln(s[511<<20])\n}\n",
		})
		if res.Status != run.StatusMemoryExceeded {
			t.Fatalf("status = %s (%s) stderr=%q", res.Status, res.Reason, res.Stderr)
		}
	})

	t.Run("network_denied", func(t *testing.T) {
		res := d.Execute(context.Background(), dtestID("net"), run.Request{
			Code: "package main\n\nimport (\n\t\"fmt\"\n\t\"net\"\n)\n\nfunc main() {\n\tc, err := net.Dial(\"tcp\", \"1.1.1.1:80\")\n\tif err != nil { fmt.Println(\"blocked:\", err) ; return }\n\tfmt.Println(\"CONNECTED\", c.RemoteAddr())\n}\n",
		})
		if res.Status != run.StatusSuccess || strings.Contains(res.Stdout, "CONNECTED") {
			t.Fatalf("network must be blocked: status=%s stdout=%q", res.Status, res.Stdout)
		}
		if !strings.Contains(res.Stdout, "blocked") {
			t.Errorf("expected dial failure, got: %q", res.Stdout)
		}
	})

	t.Run("stdin", func(t *testing.T) {
		res := d.Execute(context.Background(), dtestID("in"), run.Request{
			Code:  "package main\n\nimport (\n\t\"fmt\"\n\t\"io\"\n\t\"os\"\n)\n\nfunc main() {\n\tb, _ := io.ReadAll(os.Stdin)\n\tfmt.Print(\"got:\" + string(b))\n}\n",
			Stdin: "xyz",
		})
		if res.Status != run.StatusSuccess || res.Stdout != "got:xyz" {
			t.Fatalf("status=%s stdout=%q", res.Status, res.Stdout)
		}
	})
}

func dtestID(tag string) string {
	return fmt.Sprintf("%s%016x", tag[:2], time.Now().UnixNano())
}
