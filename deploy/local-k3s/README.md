# 本地 k3d 发布

适用于现有 `k3d-mycluster` 集群的 `go-sandbox` namespace，ARM64、Docker sidecar 模式。
Dockerfile 固定 Go 1.27.3 与 Docker CLI 基础镜像摘要；仅复制构建所需代码。
Linux 的 local runner 另外依赖 util-linux 的 `prlimit`；缺失时执行失败，不会无资源限制运行。
生产路径仍要求 `GSB_RUNNER=docker`，无需 prlimit。

每次发布使用新标签，不覆盖旧镜像。示例：

```sh
docker build --platform linux/arm64 -f deploy/local-k3s/Dockerfile -t go-sandbox:luot8-go127-20261021-2 .
k3d image import go-sandbox:luot8-go127-20261021-2 -c mycluster
kubectl --context k3d-mycluster -n go-sandbox get deployment go-sandbox -o yaml > deployment-before.yaml
kubectl --context k3d-mycluster -n go-sandbox rollout history deployment/go-sandbox
```

确认节点有额外 3.1 CPU / 2.1 GiB 请求容量后，以一次 patch 切换镜像和滚动策略：

```sh
kubectl --context k3d-mycluster -n go-sandbox patch deployment go-sandbox --type=strategic -p '{"spec":{"minReadySeconds":10,"strategy":{"type":"RollingUpdate","rollingUpdate":{"maxSurge":1,"maxUnavailable":0}},"template":{"spec":{"containers":[{"name":"api","image":"go-sandbox:luot8-go127-20261021-2"}]}}}}'
kubectl --context k3d-mycluster -n go-sandbox rollout status deployment/go-sandbox --timeout=300s
kubectl --context k3d-mycluster -n go-sandbox exec deployment/go-sandbox -c api -- wget -qO- http://127.0.0.1:8080/api/health
```

执行 `python3 deploy/local-k3s/smoke.py`，通过 Service 验证健康、页面、指标、Go 版本、stdin 与编译错误。
应确认 `runner=docker`、Go 1.27.3，提交代码输出 `runtime.Version()` 成功，再检查 Service 的 `/healthz`、页面与 `/metrics`。
应用无持久化；更新后内存中的运行记录不保留。滚动期间每个 Pod 各有并发限额。

本次发布前 revision 为 4，旧镜像为 `go-sandbox:luot3-go127-local-2`。回滚命令：

```sh
kubectl --context k3d-mycluster -n go-sandbox rollout undo deployment/go-sandbox --to-revision=4
kubectl --context k3d-mycluster -n go-sandbox rollout status deployment/go-sandbox --timeout=300s
```

回滚后重复健康与代码执行验证。此命令为恢复预案，未实际演练。
发布仅更新应用，不变更 k3s 控制平面版本；所有命令显式指定 context，避免旧的默认 context。

冷缓存编译临时文件使用每次运行的 `/out` 卷（`GOTMPDIR=/out`），避开 16MB `/tmp` 限额；运行结束随卷清理，运行阶段仍保留原有限额。
