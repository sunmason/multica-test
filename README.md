# golang-sandbox

在线 Go 代码运行平台：浏览器编辑单文件 Go 代码 → 服务端沙箱执行 → 返回输出与编译错误。
单文件、仅标准库、无账号、无持久化。需求基线见 `docs/requirements.md`，实现说明见 `docs/design.md`。

## 快速开始

前置：Go 1.26+、Docker（生产路径）。

```bash
make pull    # 拉取沙箱工具链镜像 golang:1.26-alpine
make build   # 编译到 bin/server
make run     # 监听 :8080（GSB_LISTEN 可改）
```

打开 http://localhost:8080 即用。无 Docker 的开发机自动回退 local runner（无网络隔离，页面会标注 ⚠）。

## 配置

默认值即需求基线（并发 3 / CPU 1 核 / 内存 256MB / 队列 10 / 排队超时 30s / 执行 10s / 禁网）。
覆盖方式两级：`-config config.example.json` 同构 JSON 文件，或 `GSB_*` 环境变量（如 `GSB_MAX_CONCURRENCY`、`GSB_EXEC_TIMEOUT_SEC`）。完整键位见 `config.example.json` 与 `docs/design.md`。

## 运维

- 健康探测 `GET /healthz`；状态与队列深度 `GET /api/health`；指标 `GET /metrics`（Prometheus 文本）。
- 限额回滚：改配置重启即生效；停机路径会取消在跑任务并清理容器。
- 残留清扫：启动与每分钟自动清扫 `gsb-*` 孤儿容器/一次性卷，无需人工介入。

## 测试

```bash
make vet test
```

docker 端到端用例（编译错误/超时/OOM/禁网/stdin）在本机有 Docker + 镜像时自动执行，否则跳过。
