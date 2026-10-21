"""Foreground smoke check against the existing local k3d Service."""
import json
import subprocess
import time

KUBE = ["kubectl", "--context", "k3d-mycluster", "-n", "go-sandbox"]
BASE = "http://go-sandbox.go-sandbox.svc.cluster.local:8080"


def request(path, payload=None):
    cmd = KUBE + ["exec", "deployment/go-sandbox", "-c", "api", "--",
                  "wget", "-qO-", "-T", "15"]
    if payload is not None:
        cmd += ["--header=Content-Type: application/json",
                "--post-data=" + json.dumps(payload)]
    return subprocess.check_output(cmd + [BASE + path], text=True, timeout=25)


assert request("/healthz") == "ok"
health = json.loads(request("/api/health"))
assert health["status"] == "ok" and health["runner"] == "docker", health
assert "go1.27.3" in health["go_version"], health
assert "<html" in request("/").lower()
assert "gosandbox_" in request("/metrics")
print("health, UI and metrics: PASS", health)

cases = [
    ('package main\nimport("fmt";"runtime";"os";"io")\n'
     'func main(){s,_:=io.ReadAll(os.Stdin);fmt.Println(runtime.Version(),string(s))}',
     "smoke-stdin", "success", "go1.27.3 smoke-stdin"),
    ("package main\nfunc main(){undefinedFunction()}", "", "compile_failed", ""),
]
for code, stdin, status, output in cases:
    run = json.loads(request("/api/run", {"code": code, "stdin": stdin}))
    deadline = time.monotonic() + 90
    while True:
        result = json.loads(request("/api/run/" + run["run_id"]))
        if result["status"] not in ("queued", "running", "compiling"):
            break
        if time.monotonic() > deadline:
            raise TimeoutError(result)
        time.sleep(1)
    assert result["status"] == status, result
    assert output in result.get("stdout", ""), result
    print("execution: PASS", result)
