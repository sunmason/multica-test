# golang-sandbox 实现说明（LUOT-6）

实现基线：`docs/requirements.md` @ `da9100f`（V2，需求方已确认项 + 两项待确认默认值）。
本文描述架构决策、需求映射、运维方式与已知边界。

## 一、总体架构

```
浏览器 (web/ 静态资源, embed 进二进制)
   │  POST /api/run {code, stdin} ── 202 {run_id} 或 4xx/5xx {status:"rejected", ...}
   │  GET  /api/run/{id}           ── 轮询状态/位次/结果
   │  DELETE /api/run/{id}         ── 取消排队/运行
   │  POST /api/format             ── gofmt（进程内解析，不执行代码）
   │  GET  /api/health /api/meta   ── 健康与元信息（E1/E3）
   │  GET  /metrics               ── Prometheus 文本（E2）
   ▼
internal/server  ── HTTP 薄层（校验、限流语义映射、错误文案）
internal/sched   ── 调度核心：全局 3 槽位 + 有界队列 10 + 等待超时 + 取消 + EWMA 预计等待
internal/limit   ── 每来源速率（10/min 令牌桶）与同时进行数（1）
internal/runner  ── Runner 接口；docker（生产）/ local（开发回退）两实现
internal/config  ── 全量可配（defaults ← JSON 文件 ← GSB_* 环境变量）
```

零第三方依赖：docker CLI 以子进程方式驱动；配置用 JSON；指标手写暴露。

## 二、沙箱执行（docker runner，生产路径）

每次提交两个临时容器 + 一个一次性 named volume，全程 `--network none`：

| 阶段 | 容器 | 关键限制 |
|---|---|---|
| 编译 | `gsb-compile-<run_id>` | 1 核 / 512MB（平台开销，非确认限额范畴）/ pids 64 / 只读 rootfs / tmpfs 16MB / cap-drop ALL / no-new-privileges；源码经 stdin 注入（无宿主机 bind mount）；共享 `gsb-gocache` 卷加速 stdlib 编译 |
| 运行 | `gsb-run-<run_id>` | **1 核 / 256MB（确认硬限额）/ pids 64 / 只读 rootfs / tmpfs 16MB(noexec,nosuid) / uid 65534 / cap-drop ALL / no-new-privileges**；二进制经 `gsb-out-<run_id>` 卷只读挂载 |

- 源码与产物均不落宿主机文件系统；容器与一次性卷在运行结束（含异常路径）强制删除，另有两级兜底：启动时清扫 + 每分钟孤儿清扫（`docker ps -a --filter name=gsb-`，超过执行时限+120s 即强删）。
- **状态判定**：先查 `inspect` 的 `OOMKilled` → 「内存超限」；我们主动 `docker kill`（到时限）→ 「运行超时」；编译阶段退出非零且含诊断 → 「编译失败」（结构化行/列）；其余非零 → 「运行失败」；0 → 「成功」。资源类终止与代码自身错误严格分状态呈现（需求第六节）。
- **输出上限**：stdout/stderr 各 1MB，边收边计数，超出部分丢弃并统计 `dropped_bytes`，防输出炸弹打爆后端内存。
- 首次启动后台 warmup（预编译一次 hello world），避免首个用户请求承担 stdlib 冷编译（可达数十秒）。

## 三、并发与排队（第五节策略，全部可配可回滚）

1. 提交先过**每来源**闸门：10 次/分钟令牌桶 + 同时进行 1 个（并发 3 下防单用户垄断的关键配套）。
2. 全局槽位：带缓冲 channel（容量 3），**硬上限**由实现保证，不依赖业务代码自觉。
3. 槽满入队（上限 10），FIFO；轮询接口实时返回**位次**与**预计等待**（EWMA × 位次）。
4. 队列满 → 立即拒绝（`queue_full`）；等待超 30s → 自动取消（`queue_timeout`），代码留在前端编辑器。
5. 每条路径（完成/超时/取消/拒绝/停机）恰好释放一次槽位与来源配额；结果记录 10 分钟 TTL 后清除（无持久化状态，内存有界）。

## 四、待确认项的默认实现（仅改配置即可切换）

| 待确认项 | 当前默认 | 切换方式 |
|---|---|---|
| 执行时限 | 10s | `limits.exec_timeout_sec`（或 `GSB_EXEC_TIMEOUT_SEC`） |
| 网络策略 | 禁止出网（含内网） | docker `--network none`，暂无白名单能力；如需放行需另评估（见 README 风险节） |

## 五、local runner（开发回退，明确降级）

Docker 不可用时自动回退（`runner: "auto"`）：宿主机进程执行，唯一可写目录为运行后即删的临时目录；进程组击杀 + 墙钟超时 + 输出上限仍有效。**明确不具备**：网络隔离、内存限额（darwin 无 cgroup；`RLIMIT_AS` 会破坏 Go 运行时地址空间预留）、Linux 以外的进程数上限。此降级在 `/api/health`（`runner:"local"`）与前端横幅中如实标注。

## 六、前端（web/，零依赖、无构建、无 CDN）

- **UI 视觉以 LUOT-5 交付为唯一基线**：「浅色工作台 × 深色画布」，CSS token 一比一采用设计规范 §3.1–3.3（`#F8FAFC` 页面 + `#0D1526`/`#0A111E` 画布、品牌青交互色、状态色族、22px 代码行高）。首版实现曾自创深色主题，已按 LUOT-6 验收意见整体回归基线，全状态截图随交付评论归档。
- 布局对齐规范：顶栏（logo + Go 版本 chip + 容量点阵 ●●○，点阵由 `/api/health` 实时数据驱动）· 编辑器卡（`main.go` 页签、卡头 格式化/stdin、卡底运行条含 Ln/Col、字节数、重置示例、⌘/Ctrl+↵ 提示）· 输出卡（卡头状态徽章 + 耗时 + trace id + 复制/清空）· 页脚（平台不留存 · 状态页 · 范围声明）。
- 状态徽章按规范色族映射：蓝=排队 · 青=运行中 · 绿=成功 · 玫红=编译失败 · 正红=运行失败 · 琥珀=超时/排队超时 · 橙=拒绝 · 紫=内存超限；终态在输出画布内渲染同色族横幅（图标 + 标题 + 原因 + 建议 + 动作），语法高亮用规范指定色（关键字 `#7DD3FC`、字符串 `#86EFAC` 等）。
- 自绘 Go 语法高亮（tokenizer + overlay 双层编辑器）：行号、Tab 缩进、括号配对/自动缩进、⌘/Ctrl+Enter 运行；首次打开加载 Hello World。
- 九种状态全量具名渲染 + 截断琥珀块 + 排队横幅（位次/预计等待/已等待进度条/取消排队，规范 §4.3）+ 运行中/排队中按钮模式矩阵（规范 §5.3）。
- 编译错误以横幅内整行按钮呈现（`main.go:行:列`），点击 → 编辑器滚动定位、对应行玫红底持续标注 + 闪烁高亮（B2）。
- 草稿 localStorage 自动保存/恢复（A4，含「保存于 HH:MM」；纯客户端，与服务端无持久化一致）；格式化成功/失败走 Toast（A2，失败保留原码）；64KB 前端拦截不发请求（A1）。
- 程序输出一律 `textContent` 渲染（无 innerHTML 拼接用户数据），CSP `script-src 'self'` + `style-src 'self'`（无内联样式属性），杜绝输出内容 XSS。
- stdin 折叠抽屉（B5，一次性预填 + EOF 说明）；页脚展示限额与「平台不留存」声明（E5）。

## 七、观测与运维（E1/E2）

- 结构化 JSON 日志：每次运行含 `run_id`（即 trace id）、来源、状态、耗时、排队时长、截断字节数、退出码、runner。
- 服务日志统一通过 `internal/logctx` 从 `context.Context` 读取 `trace_id`，使用 `InfoContext` / `WarnContext` / `ErrorContext`，保留注入的 logger。运行日志原有 `run_id` 字段保留，且 `trace_id == run_id`；提交成功的 HTTP access log、排队后执行、轮询与取消均关联该运行 ID。校验失败/拒绝、格式化、健康检查等请求各有服务端生成的独立 ID；不读取外部 trace header，不改变 HTTP JSON 或前端。
- `run stopped` 记录取消/排队超时等业务终态；`run finished` 表示执行器已经返回（包含其清理）。其中 `status` 始终为 Record 保存的最终业务终态，`runner_status` 单独保留执行器返回的状态。若取消先保存终态而执行器稍后返回 success，两条日志的 `status` 均为 cancelled，完成事件额外记录 `runner_status: success`，耗时/退出码等执行信息仍保留；不改变轮询或终态决策。
- `Scheduler.Submit/Get/Cancel/Stats/Start/Shutdown` 的首参数为 ctx，HTTP 层传入请求 ctx；Runner 原有 ctx 接口继续贯通至编译、执行及 Docker 管理命令。Record/限流器/指标等纯数据操作、纯函数和标准库约定的 HTTP handler 接口不机械加参。
- 提交被接收后，调度器用 `context.WithoutCancel` 保留调用方值、移除请求取消和 deadline，再建立每个任务独立的 cancel。排队任务保存自己的 ctx，出队时使用该 ctx；HTTP 返回 202、连接断开或请求 deadline 均不取消已接收任务。取消、排队超时、编译/执行超时、Shutdown 继续独立生效；终态移除任务 ctx/cancel 并停止排队定时器，轮询记录不保存 ctx。任务注册、出队、超时和停机以同一锁协调，避免丢失取消或停机后新增执行。
- main 的启动/清扫/warmup/停机共享进程生命周期 ctx 和独立 ID；Shutdown 等待 warmup 与执行清理（受调用方 ctx 及原 20s 上限约束）。Go 版本刷新使用该生命周期 ctx。Docker 清理及 OOM 检查脱离任务取消但保留 ctx 值，维持原清理行为；用户程序 stdout/stderr 不作为服务日志改写。
- `/metrics`：`gosandbox_runs_total{status}`、`gosandbox_rejections_total{reason}`、运行耗时/排队时长直方图、队列深度/运行数 gauge。
- 告警接入：对 `runs_total{timeout|memory_exceeded}` 增速、`queue_depth` 持续 > 5、`rejections_total` 增速配置阈值告警即可（由监控系统实施）。

## 八、验证记录（本机 macOS + Docker 29.8，arm64）

- `go vet` / `go build` / 全量 `go test ./...` 通过（7 个测试包，覆盖限额器、状态机、runner 全异常路径、调度排队/取消/超时、HTTP 全接口；docker 端到端含编译错误/超时/OOM/禁网/stdin 用例）。
- 真机冒烟（docker runner，warm cache）：hello world 端到端 785ms（P95 ≤ 5s 达标）；死循环 10.4s 被「运行超时」终止；512MB 逐页写入触发 cgroup OOM →「内存超限」；`net.Dial` 在 `--network none` 下立即失败；4 并发提交 → 3 运行 + 1 排队（位次/预计等待正确）；取消/超时后槽位释放、队列自动顶替。
- 停机/清扫残留检查：容器 0、一次性卷 0、临时目录 0。
- UI 复验（LUOT-6 验收退回后）：headless Chrome 驱动真实服务（docker runner）逐状态截图，idle / queued / success / compile_error / timeout / oom 六态与 LUOT-5 基线截图逐一对照通过，截图随交付评论归档。

## 九、已知边界与风险

1. **网络白名单不存在**：当前为全禁。需求方若要求出网，需要引入代理/白名单方案重新评审（不是改一个配置的问题）。
2. **gocache 卷是共享工具缓存**：只含编译产物缓存，不含任何用户代码语义上的持久化；清空该卷即回冷启动（`docker volume rm gsb-gocache`）。
3. **编译容器以 root 运行**（受限于 cap-drop ALL / 只读 rootfs / 无网络）；运行用户代码的容器恒为 uid 65534。如需 user namespace 重映射需改 Docker daemon 配置，属部署方决策。
4. **每来源识别以 IP 为准**：直连场景取 RemoteAddr；反代场景必须设 `trust_proxy: true` 且仅在可信代理后开启，否则 X-Forwarded-For 可伪造。
5. local runner 仅用于开发/测试，生产必须 `runner: "docker"`。
6. **stdout/stderr 分段展示**：runner 以两个独立缓冲收集输出，无逐行到达时序数据，故输出流按「stdout 段 → stderr 段」展示（每行带 `out`/`err` 源标）；规范 §5.4 的「按到达时序混合」需后端提供带时序的混合流才能完全实现，当前为最接近的诚实呈现。
7. **状态页入口为占位**：页脚「状态页」链接已按 UI 基线展示，目标页面属 E1/P1 范围未实现（悬停有说明）；顶栏容量点阵为 P1 项，已提前用 `/api/health` 实时数据实现。
