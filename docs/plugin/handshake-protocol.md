# 插件握手协议 OPS-PLUGIN-1

状态：草案 v1
关联文档：[插件 Manifest 规范](./manifest-spec.md)

## 1. 总览

本协议覆盖插件的两种运行形态，**零 SDK 依赖**——任何语言只需：读环境变量、读写 stdio、监听本地端口。

| 形态 | 启动命令 | 通信方式 | 生命周期 |
|---|---|---|---|
| Task | `<bin> run <task>` | stdin/stdout JSON | 跑完即退 |
| Daemon | `<bin> serve` | stdout 握手行 + 本地 HTTP/gRPC | 常驻，底座管理生命周期 |

## 2. 环境注入

底座启动插件进程时注入以下环境变量：

| 变量 | 必有 | 说明 |
|---|---|---|
| `OPS_PLUGIN` | 是 | 固定 `1` |
| `OPS_PLUGIN_MAGIC` | 是 | 固定 `OPS-PLUGIN-MAGIC-1`；插件必须校验，防止被误当 CLI 手工启动 |
| `OPS_PLUGIN_NAME` | 是 | 插件名（manifest `name`） |
| `OPS_PLUGIN_INSTANCE` | 是 | 实例 ID，底座生成；升级期间新旧实例并存 |
| `OPS_PLUGIN_DATA_DIR` | 是 | 插件数据目录绝对路径 |
| `OPS_PLUGIN_CONFIG` | 是 | 配置 JSON 字符串（可为 `{}`），见 Manifest §5 |
| `OPS_PLUGIN_LOG_LEVEL` | 否 | `debug` / `info` / `warn` / `error`，默认 `info` |
| `OPS_PLUGIN_CORE_ADDR` | 仅 daemon | 底座 API 地址（v1 仅 daemon 注入） |
| `OPS_PLUGIN_CORE_TOKEN` | 仅 daemon | 底座 API 令牌 |

## 3. Task 协议（一次性任务）

### 3.1 调用

```
<bin> run <task-name>
```

- stdin：任务输入 JSON（无输入则为 `{}`，流关闭即 EOF）；
- stdout：**恰好一个** JSON 信封（UTF-8），随后进程退出；
- stderr：日志，自由格式；
- 退出码见 §3.3。

### 3.2 输出信封

成功：

```json
{ "ok": true, "data": { } }
```

失败：

```json
{ "ok": false, "error": { "code": "EXEC_FAILED", "message": "connection refused", "retryable": false } }
```

- `code`：大写下划线枚举；保留字：`TIMEOUT`、`INPUT_INVALID`、`PERMISSION_DENIED`、`EXEC_FAILED`；
- `retryable`：底座是否可自动重试。

### 3.3 退出码

| 码 | 语义 | 底座行为 |
|---|---|---|
| 0 | 成功（以 stdout 信封为准） | 记录结果 |
| 1 | 未分类崩溃 | 记录失败 |
| 2 | 输入错误（INPUT_INVALID） | 不重试 |
| 3 | 执行失败 | 按 `retryable` 决定 |
| 4 | 权限拒绝 | 不重试，产生告警 |
| 124 | 超时（底座强杀按此语义记录） | 按 `retryable` 决定 |

底座按 `task.timeout`（默认 60s）计时，到点强杀进程；若强杀前已写出失败信封，以信封为准。

### 3.4 v1 限制

Task 形态**不注入** `OPS_PLUGIN_CORE_*`，即任务内不得回调底座 API；需要常驻能力或回调底座的场景应实现 `serve` 模式。v2 预留放开。

## 4. Daemon 握手

### 4.1 启动与握手行

```
<bin> serve
```

插件完成监听后，在 **stdout 第一行**写出握手行：

```
OPS-PLUGIN-1|<protocol>|<network>|<addr>|<token>|<pid>
```

| 段 | 说明 |
|---|---|
| protocol | 子协议，与 manifest `service.protocol` 一致：`http.v1` / `grpc.v1` |
| network | `tcp` / `unix` / `pipe`（Windows named pipe） |
| addr | 监听地址：tcp 为 `127.0.0.1:<port>`；unix 为 socket 路径；pipe 为管道名 |
| token | base64url 编码的随机数，熵 ≥32 字节；实例唯一，每次启动重新生成 |
| pid | 插件进程 PID |

规则：

- 握手行是**唯一**走 stdout 的协议数据；此后 stdout 不再承载协议内容；
- 超过 `service.readyTimeout` 未收到合法握手行 → 启动失败，杀进程；
- 握手行格式非法 → 同上。

### 4.2 鉴权

底座对插件服务的每次请求携带：

```
Authorization: Bearer <token>
```

token 仅在握手行出现一次，底座保存于内存，不落盘、不写日志。

### 4.3 http.v1 必须实现

| 方法与路径 | 说明 |
|---|---|
| `GET /healthz` | 健康检查，返回 200 即健康，body 任意 |
| `GET /capabilities` | 可选；返回运行时能力补充声明 |
| `POST /shutdown` | 优雅停止：完成进行中请求后退出，退出码 0 |

### 4.4 grpc.v1

实现标准 gRPC Health Checking Protocol（`grpc.health.v1.Health` 的 `Check` 与 `Watch`）；优雅停止语义由进程信号/自行退出实现。

### 4.5 健康检查

- 底座按 `service.liveness.intervalSeconds`（默认 15s）周期探测；
- 连续 `failureThreshold`（默认 3）次失败 → 判定不健康，杀进程并进入崩溃恢复。

## 5. 生命周期与崩溃恢复

### 5.1 状态机

```
STARTING → READY → SERVING → STOPPING → EXITED
              │        │
              └─ 崩溃 ─┴→ BACKOFF → (重启) STARTING

STARTING/BACKOFF 阶段失败超限 → FAILED
```

- READY：收到合法握手行；
- SERVING：健康检查通过；
- STOPPING：底座发起 `POST /shutdown` 或执行强杀；
- EXITED：进程退出且属预期（主动停止、升级切换）。

### 5.2 崩溃退避

- 非主动退出（exit ≠ 0 或被杀）→ 自动拉起，退避序列 `1s → 2s → 4s → 8s → 16s`（封顶）；
- 滚动窗口 1 小时内崩溃 ≥5 次 → 插件置 **FAILED**，停止拉起并产生告警事件；
- 主动退出（exit = 0）不拉起，仅记录并告警。

### 5.3 热升级

1. 启动新版本实例（新 `OPS_PLUGIN_INSTANCE`，二进制先落暂存名）；
2. 新实例 READY 且健康检查通过；
3. 切流：UI 路由/服务发现指向新实例；
4. 对旧实例 `POST /shutdown`，等待 EXITED（至多 `stopTimeout` 后强杀）；
5. 任一步失败：停新实例，旧实例若仍在则继续服务，插件状态回滚。

Windows：运行中的 exe 不可覆盖，须停旧后将 `<bin>.exe.new` rename 为 `<bin>.exe`。

## 6. 安全要求

1. 插件服务**只监听回环地址**（127.0.0.1 / unix socket / named pipe）；
2. `serve` 模式必须校验 `OPS_PLUGIN` 与 `OPS_PLUGIN_MAGIC`，缺失或不符 → 立即以退出码 1 拒绝执行；
3. token 与 secret 只经环境变量/握手行传递，禁止写入日志、manifest、错误信息；
4. 插件 UI 一律经底座反向代理访问（统一鉴权与审计），插件端口不得直接暴露给用户；
5. 审计：记录插件进程启动/退出/崩溃/升级事件（含 exit code 与 INSTANCE）。

## 7. 时序示例（http.v1 daemon）

```
底座                                     插件进程
 │                                         │
 │ spawn(serve, env=OPS_PLUGIN_*)          │
 │────────────────────────────────────────▶│
 │                                         │ bind 127.0.0.1:39127
 │  OPS-PLUGIN-1|http.v1|tcp|127.0.0.1:    │
 │      39127|<token>|<pid>                │
 │◀────────────────────────────────────────│  (stdout 第一行)
 │ GET /healthz  Authorization: Bearer ... │
 │────────────────────────────────────────▶│
 │ 200 OK                                  │  → SERVING
 │◀────────────────────────────────────────│
 │  (用户请求经底座反向代理转发至插件)          │
 │◀───────────────────────────────────────▶│
 │ POST /shutdown                          │
 │────────────────────────────────────────▶│
 │ connection closed, exit 0               │  → EXITED
 │◀────────────────────────────────────────│
```

Task 形态则简单得多：`spawn(run probe, stdin=JSON)` → 进程输出单个信封 → 退出码收尾。

## 8. 版本兼容

- 协议版本 `OPS-PLUGIN-1`：底座与插件在 MAGIC 校验时一并核对；
- v1 内只允许追加可选字段/可选路径，禁止修改既有字段语义；
- 下一版本 `OPS-PLUGIN-2` 预留：Task 回调底座、双向流、连接复用等；
- 底座须拒绝 MAGIC 不匹配的进程（等价于未知插件）。
