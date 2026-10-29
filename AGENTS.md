# AGENTS.md —— AI 编码上下文规范

> 本文件是这个仓库的**入口权威**。任何 AI agent 或新成员在改动代码前，必须先读完本文件，再按 §2 的顺序读取关联文档。
> 冲突时的优先级：**AGENTS.md > docs/design/ > specs/ > 代码现状**。

## 1. 项目一句话

`vlogs-backup` 是一个 Go 单二进制工具，在 **VictoriaLogs 不停机**的前提下，把按 UTC 天分区的历史日志备份到 S3 兼容对象存储，并支持**按指定日期在线回灌**。

## 2. 必读文档（按顺序）

| 顺序 | 文件 | 读到什么程度 |
| --- | --- | --- |
| 1 | `AGENTS.md`（本文件） | 全文，含硬约束与禁止项 |
| 2 | `docs/design/victorialogs-s3-backup-design.md` | 至少精读 §5 设计决策、§6 数据模型、§7 流程与回滚矩阵、§9 VL 契约 |
| 3 | `specs/vlogs-backup/spec.md` | 当前要做的需求条目与验收标准 |
| 4 | `specs/vlogs-backup/tasks.md` | 本次任务在整体拆分中的位置与依赖 |

**开工前自检**：能否用一句话说清「这次改动落在一个分区备份/恢复生命周期的哪一步、失败时如何回滚」。说不清就先回去读文档，不要动手。

## 3. 不可协商的硬约束（Constitution）

以下 13 条是红线。任何一条与「实现方便」冲突时，一律以约束为准；确实需要突破，必须先停下来向人确认，并在设计文档中记录决策，不得自行放行。

| # | 约束 | 违反后果 |
| --- | --- | --- |
| C1 | **不重启 VL 进程**完成备份与单天恢复；恢复只走 detach → 替换 → attach | 违背产品前提，功能失去意义 |
| C2 | 备份数据**必须**来自 VL 分区快照，禁止直接读 `partitions/<date>` 拷贝 | 会备份到半写入 part，产生不可用副本 |
| C3 | `manifest.json` **必须最后写入**，且是唯一的提交点；无 manifest 的前缀不得被 `restore` 使用 | 产生「看起来完整、实际残缺」的备份 |
| C4 | 恢复**必须**先下载到同文件系统临时目录，校验通过后再 `rename` 原子替换；禁止边下边写目标目录 | 崩溃会留下半成品分区 |
| C5 | 任何失败分支**必须**把分区退回 `present` 状态（或明确停在「分区缺失 + 人工指引」），禁止 attach 未校验目录 | 可能造成数据不可用 |
| C6 | 快照**必须**在作业结束时清理；启动时**必须**执行孤立快照清理 | 磁盘被硬链接占满且保留策略无法释放 |
| C7 | S3 键**必须**含 `v1/<cluster>/<node>/<YYYYMMDD>/`；禁止省略 cluster/node | 集群下恢复出的「一天」不完整 |
| C8 | 分区日期一律按 **UTC** 解释；`--date` 与目录名、manifest 三方必须一致 | 数据落到错误的天 |
| C9 | 凭据**禁止**出现在日志、摘要、错误信息、manifest、命令行回显中 | 安全事故 |
| C10 | `restore` / `delete` / `gc` 在非 TTY 环境**必须**要求 `--yes`；恢复或删除 UTC 今天分区必须额外授权 | 误操作导致实时日志缺口或备份丢失 |
| C11 | 退出码语义固定（见设计文档 §11.2），不得复用或新增含义 | 破坏 cron/告警契约 |
| C12 | 重试只在访问层（`internal/vl`、`internal/s3store`）实现；编排层不得自行加 sleep 重试 | 重试风暴与超时不可控 |
| C13 | `delete` 操作的是**本地** `partitions/<date>`：必须先输出确认提示并等待用户输入、必须先 `detach`、必须 `rename` 出 `partitions/` 后再递归删除；`delete` 不得改动 S3 任何对象 | 直接删在线目录会留下残缺分区被重启 attach，造成数据损坏 |

## 4. 架构边界（不可跨越）

```text
cmd/vlogs-backup      → 只做参数解析 / 确认交互 / 退出码映射 / 输出渲染
internal/backup       → 备份状态机；依赖 vl、s3store、manifest、lock
internal/restore      → 恢复状态机与回滚；依赖 vl、s3store、manifest、lock
internal/purge        → 本地分区删除：detach → rename 出 partitions/ → 递归删除 → 复查；负责确认提示内容
internal/prune        → S3 备份单元清理（gc 专用）；先删 manifest 再删数据
internal/verify       → 备份单元在线清单与完整性校验（list/verify）；依赖 s3store、manifest
internal/manifest     → 纯结构体与校验；禁止 import 任何 IO 包
internal/errs         → 退出码契约与错误分类；纯函数，禁止 import 其他 internal 包
internal/vl           → VL HTTP 客户端；只认端点，不认业务语义
internal/s3store      → S3 客户端与键布局；只认键，不认业务语义
internal/config       → 配置合并与校验
internal/setup        → 交互式初始化向导：生成 config.toml + credentials；凭据经 Prompter 注入，不落日志
internal/lock         → 本地文件锁
internal/report       → 摘要渲染
```

规则：

- **禁止反向依赖**：访问层不得 import 编排层；`manifest` 不得 import 其他 internal 包。
- **禁止在 `cmd/` 写业务逻辑**：`cmd` 只做参数映射与错误到退出码的翻译。
- **禁止跨层直接 new 客户端**：所有 HTTP/S3 调用必须经由接口注入，便于故障注入测试。
- 需要新包时，先在设计文档 §8.2 补一行职责说明，再建目录。

## 5. 代码规范

### 5.1 通用

- Go 1.x，标准库优先；新增第三方依赖需说明理由，且不得引入 CGO。
- 所有 IO 函数第一个参数为 `ctx context.Context`；禁止出现无 ctx 的阻塞调用。
- 错误必须用 `fmt.Errorf("...: %w", err)` 包装，保留链路；禁止 `errors.New` 丢弃原始错误。
- 错误信息用英文小写、不含首字母大写与结尾标点；面向用户的提示与文档用中文。
- 禁止 `panic`，除 `main` 中不可恢复的启动期配置错误（且必须给出可操作提示）。
- 禁止 `fmt.Println` 打日志；统一走结构化 logger，字段见设计文档 §11.1。
- 路径拼接一律 `filepath.Join`；涉及 S3 键一律用 `/` 且经 `KeyBuilder`，禁止手工字符串拼接。
- 时间一律 `time.Time` + 显式 `time.UTC`；禁止用本地时区做分区名换算。

### 5.2 并发

- 使用 `errgroup.WithContext` 收敛错误；禁止裸 `go func()` 无回收路径。
- worker 数一律来自配置，禁止硬编码；默认 8。
- 共享状态最小化；确需共享时用 channel 或互斥，禁止依赖 `sync.Map` 兜底。
- 任何循环内的 goroutine 必须绑定闭包变量，避免 Go 1.22 之前语义陷阱。

### 5.3 高危代码（`internal/restore`、`internal/purge`）

这两个包是唯一可能造成数据损失的区域，额外要求：

- 每个可失败步骤必须有对应回滚函数，且回滚函数本身必须可重入。
- 回滚逻辑与主流程**分文件**（`restore.go` / `rollback.go`），便于评审时逐条对照设计文档 §7.3 的矩阵。
- 每个失败点必须有对应测试（故障注入），未覆盖不得合并。
- 涉及 `rename`、删除、`detach`/`attach` 的调用，必须先写日志（含绝对路径）再执行。

### 5.4 测试

- 表驱动为主；测试文件与被测文件同包。
- 禁止真实访问外网：VL 用 `httptest`，S3 用 RustFS 容器（生产同款）或内存实现。
- 覆盖率不作为唯一指标，但 `internal/restore` 的回滚分支、`internal/purge` 的失败分支必须 100% 覆盖。

## 6. 验证门禁（提交前必须全部通过）

```bash
gofmt -l .                    # 必须无输出
go vet ./...
golangci-lint run
go test -race ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/vlogs-backup ./cmd/vlogs-backup
```

无法在本地执行某一步时，必须在交付说明中写明「未执行 + 原因」，不得默认跳过。

## 7. 变更流程

| 场景 | 必须动作 |
| --- | --- |
| 新增/修改用户可见行为 | 先改 `specs/vlogs-backup/spec.md`，再改代码 |
| 修改架构分层、包职责、接口签名 | 先改设计文档 §4/§8，并在 PR 说明中引用 |
| 改变 S3 布局、manifest 字段、退出码 | **必须**升级 `format_version` 或明确兼容策略，并更新设计文档 §6/§11 |
| 引入未决问题（§8）相关能力 | 先向人确认，不得自行选择方案 |
| 仅修 bug 且不改变契约 | 可直接改，但需在 PR 说明中标注「不涉及契约变更」 |

## 8. 已决问题记录（原「未决问题」，2026-10-28 全部裁决）

以下 Q1~Q6 已由需求方裁决，实现必须严格遵循；**新问题仍必须停下来问人**，裁决后在此表登记。

| # | 问题 | 裁决 |
| --- | --- | --- |
| Q1 | 分区管理接口鉴权 | 生产 VL 前置 **vmauth**；工具支持 `none` / `basic` / `bearer` 三种模式，配置驱动切换；不硬编码鉴权方式与 Header，全部从配置读取（设计文档 §9、§10.1） |
| Q2 | 单日分区容量与文件数 | 典型约十几 GB、分场景波动；**不写死阈值，全部做成可配置参数** |
| Q3 | 跨账户/跨区域 S3 双写 | **不做**，不实现该能力 |
| Q4 | 备份生命周期 & VL `-retentionPeriod` | 冷热分层是**使用者的调度业务方案，不是二进制内置的固定流水线**：热层 0~30d 本地（窗口由调度侧控制）、冷层 30~181d 仅 S3（调度侧编排 `backup → delete` 落位）、>181d 交 S3 生命周期淘汰。工具只提供独立原子能力。**VL `-retentionPeriod` 必须 ≥181d、禁止 30d**（否则 attach 恢复的分区被 VL 原生 GC 删除）；本地 30 天清理由调度侧调本工具 `delete` 实现，禁止依赖 VL retention；时间判断统一 UTC（设计文档 §7.7） |
| Q5 | 客户端加密（SSE-C / 自有密钥） | **不做**，不实现 |
| Q6 | 编译 & 运行适配 | 目标 OS：Linux、Windows；架构：amd64、arm64；需四平台交叉编译 |

Q1 实现约定：实现 `AuthInjector` 接口，按 `vl_auth_mode` 提供 `NoAuth` / `BasicAuth` / `BearerAuth` 三种实现；**不要**猜测或写死具体 header 名，一律按标准 `Authorization` 头从配置构造。

## 9. 明确禁止

- 禁止用 `vmbackup` / `vmrestore`（那是 VictoriaMetrics 时序库的工具，不适用于 VictoriaLogs）。
- 禁止用 `rsync` / `aws-cli` / shell 脚本充当实现，工具必须自带能力。
- 禁止为「未来可能需要」预留抽象、配置项或兼容分支；三行重复优于一个提前抽象。
- 禁止在恢复路径上「先删旧目录再下载新数据」。
- 禁止在 `gc` 中绕过 `prune.DeleteUnit()` 手写删除逻辑，或颠倒「先 manifest 后数据」的顺序。
- 禁止 `delete` 跳过确认提示、禁止在未 `detach` 时改动分区目录、禁止直接 `os.RemoveAll(partitions/<date>)`（必须先 `rename` 出 `partitions/`）。
- 禁止让 `delete` 操作 S3 对象，或让它承担 `gc` 的职责。
- 禁止把快照长期保留当作常规机制。
- 禁止在未读设计文档 §7.3（回滚矩阵）的情况下修改 `internal/restore`；禁止在未读 §7.5 的情况下修改 `internal/purge`。
- 禁止修改本文档的硬约束条目而不经人确认。

## 10. 交付说明模板

每次任务结束，回复中至少包含：

1. **改了什么**：按包列出，附文件链接。
2. **对应哪条需求**：引用 `specs/vlogs-backup/spec.md` 的条目编号。
3. **触碰了哪些硬约束**：若无，写「无」；若有，说明如何满足。
4. **验证结果**：§6 门禁逐条列出通过/未执行。
5. **遗留风险**：回滚路径覆盖情况、未决问题依赖。

## 11. DO / DON'T 避雷清单（需求方 2026-10-28 追加）

Agent 在任何任务中都必须遵守以下清单，与 §3 硬约束同等效力。

✅ DO

- 快照必须确认 S3 上传成功（manifest 已提交、命令退出 0）后，调度侧才执行 detach / delete partition；工具侧通过 `backup` 退出码与 `delete` 的备份护栏（R-DEL-07）保证该顺序可被安全编排；
- 恢复前前置校验分区时间，超出 retention 窗口（`vl_retention_days`，默认 181d）直接报错拒绝 attach（退出码 6）；
- 鉴权头动态构造，配置驱动（`vl_auth_mode` = none/basic/bearer，标准 `Authorization` 头）；
- 分区容量、相关阈值全部可配置，不写死。

❌ DON'T

- 不要把 VL 启动参数 `-retentionPeriod` 设置成 30d（必须 ≥181d，否则冷层分区 attach 后被 VL 原生 GC 直接删除）；
- 不要依靠 VL retention 做本地 30 天分区清理（本地窗口清理由调度侧调用本工具 `delete` 实现）；
- 不要在快照上传未完成时 detach / 删除分区；
- 不要硬编码鉴权 Header、容量阈值；
- 不要把冷热分层做成二进制内置的固定流水线——它是使用者的调度业务方案，本工具只提供独立、可单独调用的原子能力。
