# tasks.md —— vlogs-backup 任务分解

> 每个任务标注：**产出**、**验收依据**（对应 `spec.md` 的 R 编号）、**前置依赖**。
> 执行约定：任务未满足验收不得标记完成；遇到 `AGENTS.md` §8 的未决问题必须停下确认。

## 里程碑总览

| 里程碑 | 目标 | 出口标准 |
| --- | --- | --- |
| M0 | 契约确认 | Q1~Q6 已裁决（2026-10-28，见 AGENTS.md §8）；T0.1 curl 实测通过；S3 兼容测试存储可用（生产为 RustFS，测试首选 RustFS 容器；备选 MinIO 归档镜像或 SeaweedFS/Garage） |
| M1 | 骨架可编译 | `go build` 通过，`version` 可用，门禁全绿 |
| M2 | 备份可用 | 单天备份 E2E 通过，S3 产物可被 `list` 识别 |
| M3 | 恢复可用 | 单天在线恢复 E2E 通过，回滚矩阵全覆盖 |
| M4 | 运维完备 | `setup`/`verify`/`delete`/`gc` 可用，退出码与 JSON 契约齐备 |
| M5 | 发布 | 四平台二进制（Linux/Windows × amd64/arm64）+ 发布说明 + 运维手册 |
| M6 | 插件协议集成 | `describe` / `run <task>` 对齐 OPS-PLUGIN-1（规范见 `docs/plugin/`），CLI 行为零破坏 |

---

## M0 契约确认（不含代码）

| # | 任务 | 产出 | 验收 | 依赖 |
| --- | --- | --- | --- | --- |
| T0.1 | 用实测验证鉴权连通性（Q1 方案已定：vmauth + none/basic/bearer） | 设计文档 §18 更新 + 一个 curl 实测记录（三种模式各一次） | 实测能带鉴权调用 snapshot/create | — |
| T0.2 | 实测单日分区规模（Q2 已定方向：阈值全部可配置） | 现网一天的文件数与总字节数结论 | 写入设计文档 §14，校准默认值 | — |
| T0.3 | 确认 S3 桶生命周期规则（Q4 已裁决：>181d 交 S3 淘汰） | 生命周期规则配置记录（181d 淘汰，含 manifest 与数据） | 与设计文档 §7.7 约定一致 | — |
| T0.4 | 搭建本地验证环境 | 本地 VL + S3 兼容存储（docker compose；RustFS 容器优先——生产同款，备选 MinIO 归档镜像 / SeaweedFS / Garage） | 能造数、能查、能写对象 | — |

**M0 未完成前不得开始 M2 及之后任务**，否则会在错误的契约上写代码。

---

## M1 骨架（可编译、可测、无业务）

| # | 任务 | 产出 | 验收 | 依赖 |
| --- | --- | --- | --- | --- |
| T1.1 | 初始化模块与目录 | `go.mod`、`cmd/vlogs-backup/main.go`、`internal/` 空包 | 目录与 `AGENTS.md` §4 一致 | M0 |
| T1.2 | 配置层 | `internal/config`：flag/env/文件合并 + 校验 | 缺必填项返回退出码 2 | T1.1 |
| T1.3 | 错误与退出码 | `internal/errs`：错误分类 → 退出码映射 | 表驱动测试覆盖 8 个码，R-OPS-01 | T1.1 |
| T1.4 | 摘要渲染 | `internal/report`：表格 + 单行 JSON | R-OPS-02 | T1.1 |
| T1.5 | 版本信息 | `version` 子命令 + ldflags 注入 | R-OPS-05 | T1.1 |
| T1.6 | 门禁脚本 | `Makefile` 或 `justfile` 固化 §6 命令 | 本地一次跑通 | T1.1 |

---

## M2 备份链路

| # | 任务 | 产出 | 验收 | 依赖 |
| --- | --- | --- | --- | --- |
| T2.1 | manifest 契约 | `internal/backupmeta`（旧名 internal/manifest，2026-10 更名）：结构、校验、版本兼容 | R-MF-02、R-MF-03 | T1.3 |
| T2.2 | VL 客户端 | `internal/vl`：`AuthInjector`（NoAuth/BasicAuth/BearerAuth，按 `vl_auth_mode` 动态构造标准 `Authorization` 头，不硬编码）+ snapshot create/delete/list、detach、attach、flush、能力探测 | R-OPS-04、R-OPS-08；契约测试用 `httptest`（断言三种模式的请求头与缺凭据启动失败） | T0.1 |
| T2.3 | S3 客户端 | `internal/s3store`：Head/Put/Get/List/Delete + 分段上传 + KeyBuilder；依赖 `aws-sdk-go-v2`（config/credentials/service/s3，`UsePathStyle` 兼容自建端点，选型见设计文档 §8.4） | 键布局符合 C7；S3 兼容容器集成测试（RustFS 容器优先，备选 MinIO 归档镜像 / SeaweedFS / Garage） | T1.3 |
| T2.4 | 作业锁 | `internal/lock`：`cluster/node/date` 互斥 | 并发启动第二个作业返回退出码 6 | T1.1 |
| T2.5 | 备份主流程 | `internal/backup`：状态机 + 快照 + 并发上传 | R-BK-01、R-BK-03、R-BK-08 | T2.1 T2.2 T2.3 T2.4 |
| T2.6 | 幂等与续传 | HEAD 比对跳过 + 中断重跑 | R-BK-05 | T2.5 |
| T2.7 | 已存在处理 | `--if-exists=skip/replace`，先传后删 | R-BK-06 | T2.6 |
| T2.8 | 快照生命周期 | 作业内清理 + 启动孤立清理 | R-BK-07 | T2.5 |
| T2.9 | 批量与当天提示 | `--from/--to`、`--latest`、当天 flush + 提示 | R-BK-02、R-BK-04 | T2.5 |
| T2.10 | `list` 子命令 | 列举已提交备份 | 无 manifest 前缀不展示 | T2.1 T2.3 |

---

## M3 恢复链路（最高风险，逐项评审）

| # | 任务 | 产出 | 验收 | 依赖 |
| --- | --- | --- | --- | --- |
| T3.1 | 恢复前置校验 | manifest 拉取、身份比对、版本检查、磁盘预检、路径安全、**保留窗口校验**（分区日期早于 `now(UTC)-vl_retention_days` 直接拒绝，退出码 6，下载与 detach 之前） | R-MF-04、R-RS-02、R-RS-09、R-RS-10 | T2.1 T2.3 |
| T3.2 | 下载与校验 | 并发下载到临时目录 + 大小/哈希校验 | R-RS-03 | T3.1 |
| T3.3 | 原子替换 | detach → rename → 旧目录改名 → attach | R-RS-04、R-RS-06 | T3.2 |
| T3.4 | 回滚矩阵实现 | `rollback.go` 覆盖 §7.3 全部失败点 | R-RS-05（每点一测） | T3.3 |
| T3.5 | 冒烟查询 | attach 后最小查询 + 摘要字段 | R-RS-07 | T3.3 |
| T3.6 | 高危确认 | `--yes`、当天二次确认、`--force-today` | R-RS-08、C10 | T3.1 |
| T3.7 | E2E 验证 | 造数 → 备份 → 删分区 → 恢复 → 查数 | R-RS-01 全流程通过 | T3.4 T3.5 T3.6 |

**T3.4 是合并前必须单独评审的项**。评审清单：每个失败点是否有测试、回滚是否可重入、日志是否含绝对路径。

---

## M4 运维完备

| # | 任务 | 产出 | 验收 | 依赖 |
| --- | --- | --- | --- | --- |
| T4.1 | `verify` | 轻校验（大小）与 `--deep`（哈希） | 篡改对象能被识别为退出码 5 | T2.1 T2.3 |
| T4.2 | setup 向导实现 | `internal/setup`：`Wizard`、`Prompter`；逐项询问、URL 校验、凭据不回显 | R-SU-02、R-SU-03 | T1.2 |
| T4.3 | 连通性验证 | VL 能力探测 + S3 `HeadBucket`；重试/重输/中止分支；`--skip-check` | R-SU-04 | T4.2 T2.2 T2.3 |
| T4.4 | 配置文件落盘 | `config.toml`（0644）+ `credentials`（0600）、目录创建、覆盖确认 | R-SU-01、R-SU-05 | T4.2 |
| T4.5 | `setup` 子命令 | `--output-dir`、非 TTY 拒绝、生成结果 dry-run 自检 | R-SU-06、R-SU-07 | T4.3 T4.4 |
| T4.6 | 本地删除实现 | `internal/purge`：`Job`、`Confirmer`；detach → rename 出 `partitions/` → 递归删除 → 复查 | R-DEL-05、R-DEL-06、R-DEL-09 | T2.2 T1.1 |
| T4.7 | 确认提示渲染 | 6 字段提示文本 + 输入完整日期串校验 + `--json` 下走 stderr | R-DEL-02、R-DEL-03 | T4.6 |
| T4.8 | `delete` 子命令 | `--date`/`--from-to`、`--dry-run`、`--yes`、`--force-no-backup`、`--force-today`、逐天独立 | R-DEL-01、04、07、08、10 | T4.6 T4.7 |
| T4.9 | 删除故障注入 | rename/删除各失败点回滚测试 + `.deleting-*` 启动清理 | R-DEL-09、R-DEL-11 | T4.8 |
| T4.10 | `gc` 子命令 | `internal/prune`：dry-run 默认、无 manifest 未完成前缀 stale 判定清理；**不含按保留期删除完整单元**（>181d 淘汰交 S3 生命周期，R-GC-04）；人工显式移除完整单元时保持先 manifest 后数据顺序 | R-GC-01 ~ R-GC-04 | T2.10 |
| T4.11 | 凭据扫描 | 自动化断言：输出与产物中无凭据（含 setup 生成的文件） | R-OPS-03、R-SU-03 | M3 |
| T4.12 | 并发等价性 | 并发 1 与并发 8 产物一致 | R-OPS-06 | T2.5 |
| T4.13 | 告警样例 | cron 场景下的退出码 → 告警映射示例 | 可被运维直接抄用 | T4.10 |

**T4.6 / T4.7 / T4.9 涉及本地在线数据删除，属高危代码**。评审清单：确认提示字段是否齐全、交互是否要求输入完整日期串、是否先 detach 再 rename、`partitions/` 下是否任何时刻都只有完整目录、失败分支是否覆盖。

---

## M5 发布

| # | 任务 | 产出 | 验收 | 依赖 |
| --- | --- | --- | --- | --- |
| T5.1 | 交叉编译 | 四平台静态二进制（linux/amd64、linux/arm64、windows/amd64、windows/arm64，Windows 带 `.exe`）+ sha256 | 目标机直接可跑，`version` 自检通过（R-OPS-09） | M4 |
| T5.2 | 发布说明 | 版本、`format_version`、兼容性、破坏性变更 | 符合设计文档 §16 | T5.1 |
| T5.3 | 运维手册 | 备份/恢复/校验/清理的标准操作与回滚步骤 | 覆盖设计文档 §17 | T5.1 |
| T5.4 | 首次现网演练 | 非生产环境完整演练一次恢复 | 记录耗时与瓶颈，回写 §14 | T5.3 |

---

## M6 插件协议集成（2026-10-29，规范见 docs/plugin/）

| # | 任务 | 产出 | 验收 | 依赖 |
| --- | --- | --- | --- | --- |
| T6.1 | 命名隔离 | `internal/manifest` → `internal/backupmeta` 更名（避免与插件 manifest 重名），15 处 import 同步；S3 键文件名 `manifest.json` 不变 | R-PL-04：既有测试全绿 | — |
| T6.2 | 协议结构层 | `internal/plugin`：manifest v1 结构与 `Validate()`、task 信封 `Envelope`/`ErrorInfo`、`OPS_PLUGIN_*` 环境变量名、握手行解析；纯结构体，禁止 IO | R-PL-01 结构基础；`Validate` 覆盖 name/modes/binary/task 唯一性/ui⇒daemon | T6.1 |
| T6.3 | describe 子命令 | `cmd/vlogs-backup/describe.go`：动态构造完整 manifest JSON（6 task + config schema + permissions），secret 项仅标 `secret: true` 不含真实值 | R-PL-01（C9） | T6.2 |
| T6.4 | run 适配层 | `cmd/vlogs-backup/plugin_run.go`：stdin JSON → argv 翻译（kebab/snake_case 等价）、自动注入 yes/json、退出码映射（设计文档 §11.2 插件映射表）、信封包装 | R-PL-02、R-PL-03、R-PL-05 | T6.3 |
| T6.5 | 帮助页与 CLI 兼容 | `run -h` 专属帮助页；`-h` 总览列出 describe/run；既有子命令行为零破坏 | R-PL-06、R-PL-04 | T6.4 |
| T6.6 | 协议单测补齐（2026-10-29 完成） | `internal/plugin/manifest_test.go`（Validate 边界 / ParseHandshake / ListenAddr / Env 检测）+ `cmd` 侧 `plugin_run_test.go`（jsonToArgs 类型覆盖、readTaskInput、buildTaskEnvelope 全表、runPlugin 端到端）与 `describe_test.go`（**config 默认值与 config.Default() 一致性回归**、task 清单、required/secret 标注、C9 secret 无默认值） | 覆盖 R-PL-01 ~ R-PL-06 可自动化验收项 | T6.4 |

---

## 并行建议

```text
M0 ──► M1 ──┬──► M2 ──► M3 ──► M4 ──► M5
            └──► T1.x 可并行完成后，T2.1/T2.2/T2.3 三条线可并行开发
```

- `backupmeta`（T2.1）、`vl`（T2.2）、`s3store`（T2.3）三者互相独立，可并行。
- setup 链路（T4.2 ~ T4.5）只依赖 T1.2/T2.2/T2.3，可与 M3 并行开发。
- M3 必须等 M2 完成，因为恢复的校验依赖备份产物的真实结构。
- T5.4 演练不可省略：设计的并发默认值与磁盘系数（Q2）只能靠实测确认。
