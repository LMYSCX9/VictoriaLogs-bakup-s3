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
| M6 | 双后端与子命令路由（2026-11-13） | 本地文件备份后端与 S3 并列可用；六类命令支持 `file`/`s3` 子命令 + legacy 自动路由；R-ST-01~03 全部落地 |

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
| T2.1 | manifest 契约 | `internal/manifest`：结构、校验、版本兼容 | R-MF-02、R-MF-03 | T1.3 |
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

## M6 双后端与子命令路由（2026-11-13 迭代，已完成）

> 背景：并非所有部署场景都有 S3 对象存储。新增本地文件系统备份后端，并把六类命令升级为 `file` / `s3` 子命令路由；对应 `spec.md` §9（R-ST-01 ~ R-ST-03）与设计决策 D15。**不触碰任何 C 类硬约束**（快照来源、manifest 提交点、detach/attach 流程均在编排层，未改动）。

| # | 任务 | 产出 | 验收 | 状态 |
| --- | --- | --- | --- | --- |
| T6.1 | 本地后端实现 | `internal/s3store/local.go`：`ObjectStore` 接口 + `LocalStore`（键→路径映射、Put 临时文件+rename 原子落位、List 按前缀剪枝 + ctx 取消、HeadDir 连通性探测）；`purge` 编译期断言扩展 | R-ST-01：两种后端产物同构；manifest 提交语义（C3）不变 | ✅ |
| T6.2 | 配置层扩展 | `internal/config`：`storage_type` / `local_backup_path` 键、`VLB_STORAGE_TYPE` / `VLB_LOCAL_BACKUP_PATH` 环境变量、`EffectiveStorageType()`（local_backup_path 非空强制 local）、`RequireBackend()` | R-ST-02：优先级 `local_backup_path` 非空 > `storage_type` > 默认 `s3`；非法值退出码 2 | ✅ |
| T6.3 | 命令路由改造 | 六类命令（setup/backup/restore/list/verify/gc）顶层 `runXxx` 路由 → `runXxxCore(requireStorageType)`；legacy 自动路由 + stderr 废弃警告 + JSON `deprecated:true`；`delete` 传 `"skip"` 与后端解耦；`loadConfigFor` 后端强制校验 + `backendFixHint` 可操作指引 | R-ST-03：后缀不匹配退出码 2；legacy 行为兼容 | ✅ |
| T6.4 | setup 向导条件裁剪 | `Options.ForceBackend`；后端选择步骤 + 跨后端共用项 + 后端专属项二选一；`defaultCheckLocal` 本地目录可写探测；`configMap` / `printSummary` 按后端分支 | `setup file` 生成 `storage_type="local"`，`setup s3` 保持原流程；spec R-ST-01 向导条目 | ✅ |
| T6.5 | 布局裁决与文档同步 | **裁决：local 模式忽略 `s3_prefix`**，本地布局为 `<root>/v1/<cluster>/<node>/`（buildRuntime 对 local 传空前缀）；同步 AGENTS.md §4、设计文档 §4/§5(D15)/§6.1/§8.2/§10.3、spec §9、README、manual | spec R-ST-01 与设计文档 §6.1 口径一致；S3 布局与 `format_version` 未变 | ✅ |
| T6.6 | 代码审查与修复 | 双验证员交叉审查 7 项问题全部修复：setup file→local 映射、错误指引可操作化、布局三方对齐、usageText 更新、List 前缀剪枝、ctx 取消传播、pathToKey 一致性 | 门禁全绿（gofmt / vet / test 14 包 / build） | ✅ |
| T6.7 | 本地后端补充测试 | （部分完成 2026-11-13）cmd 接线层回归测试 `runtime_test.go::TestEffectiveKeyPrefix`（E2E 曾因 backup/restore/purge 三处直读 `cfg.S3Prefix` 导致 local 布局多出 prefix 层，已修复为 `effectiveKeyPrefix` 单一事实源）；LocalStore.Delete 增加空父目录清理。**仍待办**：`setup file` 分支端到端向导测试（ask / configMap / printSummary / defaultCheckLocal） | R-ST-01 / R-ST-03 本地分支用例补齐 | 🔶 部分 |

**遗留风险记录**：`setup file` 分支的向导流程只有 S3 分支测试桩覆盖（T6.7 余项）；`gc file` 本地清理路径已于 2026-11-13 在 192.168.1.53 实测通过（伪造 stale 前缀 dry-run 列出 → --yes 删除 → 空目录树一并清理 → 完整单元无损）。

---

## 并行建议

```text
M0 ──► M1 ──┬──► M2 ──► M3 ──► M4 ──► M5
            └──► T1.x 可并行完成后，T2.1/T2.2/T2.3 三条线可并行开发
M5 ──► M6（双后端迭代，2026-11-13 已完成；T6.7 补测试待办）
```

- `manifest`（T2.1）、`vl`（T2.2）、`s3store`（T2.3）三者互相独立，可并行。
- setup 链路（T4.2 ~ T4.5）只依赖 T1.2/T2.2/T2.3，可与 M3 并行开发。
- M3 必须等 M2 完成，因为恢复的校验依赖备份产物的真实结构。
- T5.4 演练不可省略：设计的并发默认值与磁盘系数（Q2）只能靠实测确认。
- M6 为 M5 之后的增量迭代：双后端（T6.1/T6.2）先行，路由（T6.3）与向导（T6.4）依赖配置层；T6.7 补测试可与任意后续迭代并行。
