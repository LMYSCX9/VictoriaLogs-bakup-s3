# spec.md —— vlogs-backup 需求规格

> 本文件承载**可验证的需求条目**。设计实现细节见 `docs/design/victorialogs-s3-backup-design.md`，硬约束见 `AGENTS.md` §3。
> 编号规则：`R-<域>-<序号>`。域：`SU` 配置初始化、`BK` 备份、`RS` 恢复、`MF` 清单、`GC` S3 清理、`DEL` 本地分区删除、`OPS` 运维接口、`PL` 插件协议。
> 任何代码改动都应能指回至少一条 R 编号；指不回说明要么缺需求、要么改动越界。

## 0. 验收总则

每个 R 条目必须能被一条自动化测试或一条可复现的手工验证覆盖。无法验证的条目不得进入实现阶段。

---

## 1. 备份（BK）

### R-BK-01 按天备份

**当**用户执行 `backup --date <YYYY-MM-DD>` **时**，系统应把该 UTC 日的分区备份为一个完整的备份单元。

验收：
- S3 上出现 `v1/<cluster>/<node>/<YYYYMMDD>/manifest.json` 与 `files/` 下全部数据文件；
- `manifest.partition`、S3 键中的日期、`--date` 三者一致；
- 备份前后 VL 进程未重启，其他日期的查询与写入不受影响。

### R-BK-02 批量与区间备份

**当**用户指定 `--from/--to` 或 `--latest N` **时**，系统应按日期逐天独立备份，单天失败不影响其余天，最终退出码反映最严重的失败类型。

验收：3 天区间中第 2 天失败时，第 1、3 天仍生成完整备份单元，退出码为第 2 天的失败原因。

### R-BK-03 快照一致性

**系统应**始终通过 VL 分区快照读取数据，**不得**直接读取 `partitions/<date>` 目录。

验收：代码审查 + 契约测试，确认 `internal/backup` 的数据源路径一律来自 `Snapshotter.Create` 返回值。

### R-BK-04 当天分区提示

**当**备份日期为 UTC 今天 **时**，系统应先调用强制落盘端点，并在摘要中显式提示「内存缓冲内容可能未纳入备份」。

验收：摘要含该提示字段；`--json` 输出中对应字段为 true。

### R-BK-05 幂等与续传

**当**目标对象已存在且大小一致 **时**，系统应跳过上传；**若**上次作业中断，重跑应只补传缺失或大小不一致的对象。

验收：人为中断后重跑，摘要中「跳过对象数」等于已存在对象数，且最终 manifest 的文件集合完整。

### R-BK-06 已存在备份的处理

**当**目标日期已有已提交备份 **时**，默认应跳过；仅在 `--if-exists=replace` 时应重新备份，且**必须先上传新对象并写入新 manifest，再清理旧 manifest 中不再被引用的对象**。

验收：`replace` 过程中强杀进程，旧 manifest 仍可用、`restore` 仍能成功。

### R-BK-07 快照生命周期

**系统应**在作业结束（无论成功失败）时删除本次创建的快照；**且应**在每次作业启动前清理存在超过阈值（默认 2h）且无对应作业锁的孤立快照。

验收：强杀进程后再次启动作业，孤立快照被清理并在日志中告警。

### R-BK-08 进度可见

**当**备份进行中 **时**，系统应周期性输出已传输字节数、文件数与当前阶段到 stderr；`--json` 模式下 stdout 只允许出现一行最终结果。

验收：重定向 stdout 到文件时，文件内容可被单次 JSON 解析。

---

## 2. 清单（MF）

### R-MF-01 提交语义

**系统应**把 `manifest.json` 作为唯一提交点：数据对象全部写入成功后才写入 manifest。

验收：故障注入使 manifest 写入失败时，S3 上该前缀处于「无 manifest」状态，`list` 不展示、`restore` 拒绝使用。

### R-MF-02 完整性声明

**manifest 应**包含每个文件的相对路径、字节数与 sha256，以及汇总的 `file_count`、`total_bytes`。

验收：manifest 可被独立解析，字段齐全，汇总值与 `entries` 实际累加一致。

### R-MF-03 版本兼容

**当** manifest 的 `format_version` 高于工具支持版本 **时**，系统应拒绝恢复并以退出码 5 结束，提示「请升级工具」。

验收：构造 `format_version: 99` 的 manifest，`restore` 与 `verify` 均返回 5。

### R-MF-04 身份校验

**当** manifest 中的 `cluster`/`node` 与请求参数不一致 **时**，系统应拒绝恢复（除非显式 `--allow-node-mismatch`），并以退出码 5 结束。

验收：跨节点恢复默认失败，加开关后成功且摘要中标注为「跨节点恢复」。

---

## 3. 恢复（RS）

### R-RS-01 按天在线恢复

**当**用户执行 `restore --date <YYYY-MM-DD> --yes` **时**，系统应在不重启 VL 的前提下，使该日数据恢复为备份时的状态。

验收：恢复后查询该日有数据；进程 PID 未变；其他日期查询结果与恢复前一致。

### R-RS-02 磁盘预检

**系统应**在开始下载前检查目标文件系统剩余空间 ≥ manifest 总字节 × 1.1，**不足时**以退出码 6 失败且不产生任何下载产物。

验收：构造小容量挂载点，验证失败早于首个下载请求。

### R-RS-03 校验先于替换

**系统应**在 `detach` 与 `rename` 之前完成全部文件的下载与校验（默认校验大小，`--deep` 校验 sha256）；**校验失败时**应删除临时目录、保持原分区不变，以退出码 5 结束。

验收：篡改 S3 上一个对象后恢复，原分区数据仍可查，临时目录已清理。

### R-RS-04 原子替换

**系统应**通过同文件系统内的 `rename` 完成目录替换，**不得**向目标目录逐文件写入。

验收：代码审查 + 故障注入（rename 前强杀），目标目录不存在半成品状态。

### R-RS-05 回滚

**当** `detach` 之后任一步骤失败 **时**，系统应把分区恢复到可用状态：优先恢复旧目录并 `attach`；无旧目录时 `attach` 已校验的新目录；两者都不可行时以退出码 1 结束，并输出「当前目录状态 + 人工处置命令」。

验收：设计文档 §7.3 矩阵的每个失败点均有对应测试用例，且断言最终分区可被 VL 正常查询。

### R-RS-06 旧目录保留策略

**当**目标分区已存在 **时**，默认应将其改名为 `<YYYYMMDD>.replaced-<ts>` 保留；仅在 `--on-existing=delete` 时删除。

验收：恢复后存在 `.replaced-*` 目录，摘要中给出其绝对路径。

### R-RS-07 冒烟查询

**系统应**在 `attach` 成功后对该日发起一次最小查询，**若**查不到任何数据，应在摘要中告警（不改变退出码）。

验收：摘要含 `smoke_query` 字段，空数据时给出显式提示。

### R-RS-08 当天分区保护

**当**恢复日期为 UTC 今天 **时**，系统应要求额外确认（交互环境下二次确认，非交互环境需 `--force-today`），并在摘要中提示实时日志缺口风险。

验收：无 `--force-today` 的非交互恢复返回退出码 7 或 2（参数缺失），不产生任何变更。

### R-RS-09 路径安全

**系统应**拒绝恢复目标落在 `--data-path/partitions/` 之外，拒绝含 `..` 或符号链接穿越的路径，以退出码 2 结束。

验收：构造恶意 `--data-path` 组合，全部被拒绝。

### R-RS-10 保留窗口前置校验

**系统应**在发起任何下载之前校验目标分区日期不早于 `now(UTC) - vl_retention_days`（配置项，默认 181，须与 VL `-retentionPeriod` 对齐，见 §8）；**超出窗口时**以退出码 6 拒绝恢复，且不产生任何下载产物与 detach 动作——防止 attach 后被 VL 原生 GC 回收，制造数据看似恢复实则丢失的事故。

验收：构造早于窗口的备份单元，恢复请求在 validating 阶段即失败（退出码 6），S3 无下载请求、分区目录未动；窗口边界日期（恰好等于 `vl_retention_days`）允许继续。

---

## 4. 清理（GC）

### R-GC-01 默认只读

**系统应**默认以 `--dry-run` 行为运行 `gc`，仅输出将删除的对象；`--yes` 才执行删除。

验收：不加 `--yes` 时 S3 对象数量不变。

### R-GC-02 删除顺序

**当**删除一个备份单元 **时**，系统应先删 `manifest.json`，再删 `files/` 下对象。

验收：删除过程中断后，该前缀处于「无 manifest」状态，不会出现 manifest 指向缺失对象。

### R-GC-03 未完成前缀

**当**某前缀无 manifest 且最后修改时间早于 `--stale-after`（默认 24h）**时**，判定为未完成备份并可被清理。

验收：构造无 manifest 前缀，24h 后被纳入清理清单，未满 24h 时不被纳入。

### R-GC-04 淘汰职责边界

**系统应**把「按保留期删除过期备份单元」的职责移交 S3 桶生命周期规则（见 §8）：`gc` **不提供** `--keep-days` / `--before` 类按日期批量删除完整单元的参数；`gc` 的清理范围仅限无 manifest 的未完成前缀（R-GC-03）与人工显式授权的完整单元移除（走「先 manifest 后数据」顺序）。

验收：`gc --help` 不含 `--keep-days` / `--before`；构造含 manifest 的完整单元，默认 `gc` 不将其纳入清理清单。

---

## 5. 删除（DEL）

> 与 `gc` 的分工：`gc` 清理 **S3 上无 manifest 的未完成备份前缀**，`delete` 删除 **本地在线分区** `partitions/<YYYYMMDD>`；二者对象不同、代码不共用。
> `delete` 删除后**不可恢复**（除非该天已有备份并重新 `restore`），因此确认提示是本域的第一等需求。

### R-DEL-01 删除本地指定分区

**当**用户执行 `delete --date <YYYY-MM-DD>` **时**，系统应删除本地 `--data-path/partitions/<YYYYMMDD>` 整个目录，且不影响其他日期分区。

验收：删除后该目录不存在，其他日期目录与 VL 查询结果不变；VL 进程 PID 未变。

### R-DEL-02 删除前必须输出确认提示

**在执行任何删除动作之前**，系统必须向 stderr 输出确认提示，且提示必须包含：分区日期（UTC）、分区的绝对路径、文件数与总字节、该天是否已有已提交备份、影响面说明、可恢复性说明。

验收：提示文本包含上述 6 个字段；`--json` 模式下 stdout 不被提示污染（仍只有一行结果 JSON）。

### R-DEL-03 交互确认方式

**当**在 TTY 交互环境执行 **时**，系统必须要求用户输入**完整日期串**（如 `2026-10-27`）才继续，输入不匹配即取消并以退出码 7 结束；**不得**用 `y/n` 作为确认方式。

验收：输入 `y`、空行、`2026-10-28` 均取消且分区未变；输入正确日期串才执行删除。

### R-DEL-04 非交互确认

**当**在非 TTY 环境执行 **时**，系统必须要求显式 `--yes`，缺失时以退出码 2 结束且不产生任何变更；`--dry-run` 只输出提示与规模统计，不进入确认流程。

验收：无 `--yes` 的非交互执行不改变目录；`--dry-run` 前后目录签名一致。

### R-DEL-05 detach 先于目录操作

**系统必须**在改动目录前调用 detach，并在 detach 失败时直接失败、保持目录原状。

验收：mock detach 返回 5xx 时退出码 3，目录内容与 mtime 不变。

### R-DEL-06 rename-out 原子性

**系统必须**把分区目录 `rename` 到 `<data-path>/.deleting-<YYYYMMDD>-<ts>`（同文件系统）后再递归删除，**不得**直接删除 `partitions/<YYYYMMDD>`。

验收：注入「rename 后删除中途失败」，`partitions/` 下不存在该日期目录，残留只出现在 `.deleting-*`，日志给出其绝对路径。

### R-DEL-07 无备份二次授权

**当**该天在 S3 上无已提交备份 **时**，系统应拒绝执行并以退出码 6 结束；仅在显式 `--force-no-backup` 时才继续。

验收：构造无 manifest 的日期，默认执行被拒且目录不变；加开关后完成删除且摘要标注「无备份删除」。

### R-DEL-08 当天分区保护

**当**目标日期为 UTC 今天 **时**，系统应拒绝执行，仅在显式 `--force-today` 时才继续，并在确认提示中标注实时日志缺口风险。

验收：删除今天的分区默认以退出码 6 结束，目录不变。

### R-DEL-09 失败回滚

**当** detach 成功但 rename 之前失败 **时**，系统必须把分区 `attach` 回 VL；**当** rename 已成功 **时**不回滚，输出残留目录路径与人工处置命令，并以退出码 1 结束。

验收：故障注入覆盖两个分支；回滚分支下 VL 仍能查询该日数据。

### R-DEL-10 不触碰 S3 与区间独立性

**系统必须**保证 `delete` 不调用任何 S3 写接口；**当**指定 `--from/--to` 时，逐天独立确认、独立执行，某天失败不影响其余天，退出码取最严重的失败类型。

验收：只读凭据下 `delete` 可正常执行；3 天区间中第 2 天不存在时，第 1、3 天被删除，退出码为 4。

### R-DEL-11 残留自检

**系统应**在启动时清理 `<data-path>/.deleting-*` 残留目录，并在日志中告警。

验收：预置一个 `.deleting-*` 目录后启动任意子命令，该目录被删除且产生告警日志。

---

## 6. 运维接口（OPS）

### R-OPS-01 退出码

**系统应**严格按设计文档 §11.2 的 8 个退出码返回，**不得**复用或扩展含义。

验收：表驱动测试覆盖每个退出码的触发路径。

### R-OPS-02 JSON 输出

**当**指定 `--json` **时**，系统应只向 stdout 输出一行结果 JSON，字段包含 `op`、`status`、`cluster`、`node`、`partition`、`files`、`bytes`、`elapsed_ms`、`error_code`。

验收：`| jq .` 可解析，单行，无额外输出。

### R-OPS-03 凭据保密

**系统应**确保凭据不出现在 stdout、stderr、日志文件、manifest 与进程参数中。

验收：以字符串扫描方式检查全部输出与产物，零命中。

### R-OPS-04 能力探测

**系统应**在作业开始前探测 VL 快照与分区管理端点的可用性，**若**返回 404 应直接失败并提示「VL 版本过低」，不得进入部分执行。

验收：mock 端点返回 404 时，作业在 validating 阶段即失败，无副作用。

### R-OPS-05 版本信息

**当**执行 `version` **时**，系统应输出工具版本、commit、构建时间、Go 版本。

验收：`-ldflags` 注入的三个值均正确显示。

### R-OPS-06 并发可控

**系统应**支持通过配置调整并发度，**且**在并发度为 1 时行为与串行完全等价。

验收：并发 1 与并发 8 的最终产物 manifest 完全一致。

### R-OPS-07 传输参数可配置

**系统应**把影响传输与磁盘预检行为的容量类参数（分段上传阈值、磁盘预检系数等）全部做成配置项，**不得**在代码中写死阈值；未显式配置时使用文档化的默认值。

验收：修改配置后分段上传行为与预检结论随之变化；单测覆盖默认值与显式覆盖两条路径。

### R-OPS-08 VL 鉴权模式

**系统应**支持 `none` / `basic` / `bearer` 三种 VL 鉴权模式（配置项 `vl_auth_mode`，默认 `none`），经 vmauth 代理访问分区管理与快照端点：`basic` 发送标准 `Authorization: Basic base64(user:pass)`，`bearer` 发送标准 `Authorization: Bearer <token>`；**鉴权方式与 header 一律按配置动态构造，不得硬编码**。`basic` 缺 username/password、`bearer` 缺 token 时应以退出码 2 在启动期失败。

验收：表驱动测试覆盖三种模式生成的请求头（mock 服务端断言）；缺凭据组合返回退出码 2 且不发任何请求。

### R-OPS-09 四平台构建

**系统应**在 `CGO_ENABLED=0` 下完成四平台交叉编译：`linux/amd64`、`linux/arm64`、`windows/amd64`、`windows/arm64`；四个产物均应通过 `version` 子命令自检。

验收：构建脚本产出四个二进制（Windows 带 `.exe`），在各目标平台（或等价模拟）运行 `version` 退出码 0。

---

## 7. 配置初始化（SU）

> `setup` 是唯一的配置文件生成入口，面向首次安装的人工初始化。文件布局与交互流程见设计文档 §10.3。

### R-SU-01 生成的文件与权限

**当**用户执行 `setup` 并完成交互 **时**，系统应在目标目录（默认 `/etc/vlogs-backup`，可用 `--output-dir` 覆盖）生成：

- `config.toml`（0644）：非敏感项（VL 地址、`vl_auth_mode`、data_path、cluster/node、`vl_retention_days`、S3 endpoint/region/bucket/prefix/path-style、并发）；
- `credentials`（0600）：敏感项（S3 AK/SK；`basic` 模式的 VL username/password、`bearer` 模式的 VL token）；留空的项不写入。

验收：生成后用 `stat` 断言两个文件的权限位；目录不存在时以 0700 自动创建。

### R-SU-02 交互项固定、带默认值且有中文说明

**系统应**按固定顺序询问 VL 地址、VL 鉴权模式（`none` / `basic` / `bearer`；`basic` 追问 username 与 password、`bearer` 追问 token，输入均不回显）、数据目录、cluster/node、S3 endpoint/region、bucket/prefix、path-style、AK/SK，每项提供默认值或可留空说明；URL 项应做格式校验，不合法则要求重输。

**系统应**在每项询问前输出一行中文说明（以 `·` 引导），简明解释该配置的用途与取值形式；进入询问流程前应输出一行总引导（说明每项含义、可退格修正、Ctrl-C 中止）。

**系统应**在交互终端上支持行内编辑：退格键 `^H`（0x08）与 `^?`（0x7F）均执行删除（兼容不同 SSH 客户端键码），Ctrl-U 清空整行，Ctrl-C 中止向导；无法进入 raw 模式时降级为普通行读取，不得报错退出。

验收：以脚本喂入空输入，全部落到默认值（鉴权模式默认 `none`，不追问凭据）；喂入非法 URL 时被要求重输；每项询问输出含对应中文说明行；行编辑器对 `^H` 与 `^?` 字节均执行删除（单元测试断言）。

### R-SU-03 凭据不回显不打印

**系统应**保证 secret 输入不回显（`term.ReadPassword`），且生成文件的最终摘要中凭据以 `***` 代替；**不得**把任何 secret 打到 stdout、stderr 或日志。

验收：伪 TTY 下捕获全部输出，以 secret 字符串扫描零命中。

### R-SU-04 连通性验证

**系统应**在写文件前做连通性验证：VL 走能力探测（快照 list 端点可达）、S3 走 `HeadBucket`；任一失败时应显示原因并允许「重试 / 重新输入 / 中止」，中止以退出码 3 结束；**验证未通过前不得落盘任何文件**；`--skip-check` 可跳过。

验收：mock 端点不可达时不产生任何文件；选择中止返回 3；`--skip-check` 时直接生成文件。

### R-SU-05 覆盖确认

**当**目标 `config.toml` 已存在 **时**，系统应显示现有文件路径并要求输入 `overwrite` 才覆盖，其他输入取消并以退出码 7 结束。

验收：预置旧文件后输入 `overwrite` 之外任意内容，文件未被修改。

### R-SU-06 非 TTY 拒绝

**当**在非 TTY 环境执行 `setup` **时**，系统应以退出码 2 拒绝，并提示改用 flag / 环境变量直接运行。

验收：stdin 重定向自 /dev/null 时退出码为 2 且无文件产生。

### R-SU-07 生成结果可用

**系统应**保证生成的配置能被 `internal/config.Load()` 直接消费：验证通过后无需手改即可执行 `vlogs-backup --config <生成的 config.toml> backup --date 2026-10-27 --dry-run`。

验收：以 `--output-dir` 指向临时目录完成 setup（`--skip-check`），随后 dry-run 退出码为 0。

---

## 8. 数据生命周期背景（冷热分层，Q4 已决）

> 本节是**背景与约束**，不新增独立验收条目；相关行为由既有条目承载：落位顺序护栏见 R-DEL-07 与 R-BK-01/R-MF-01（manifest 提交语义），恢复窗口校验见 R-RS-10。

冷热分层是**使用者的调度业务方案，不是二进制内置的固定流水线**。本工具只提供独立、可单独调用的原子能力（`backup` / `restore` / `delete` / `gc` 等），不内置绑定完整串行工作流；分层链路（何时备份、何时删本地、窗口多长）由调用方 / 调度侧编排。

调度侧参考模型（时间均为 UTC）：

| 层 | 范围 | 位置 | 说明 |
| --- | --- | --- | --- |
| 热层 | 0 ~ 30 天 | 本地 `partitions/` attach | 30 天保留窗口由调度侧定期调 `delete` 清出，不依赖 VL retention |
| 冷层 | 30 ~ 181 天 | 仅 S3 备份单元 | 落位序列：`backup` 成功（manifest 已提交）后调度侧再 `delete` 本地分区；按需 `restore` 回灌 |
| 淘汰层 | > 181 天 | 无 | S3 桶生命周期规则自动清理，数据不可恢复 |

关键约束（实现与文档均不得违背）：

1. VL `-retentionPeriod` 必须 ≥ 181d、禁止 30d——它是 VL 原生 GC 规则，超出窗口的挂载分区会被后台自动删除；设 30d 会使冷层分区 attach 后立即被 GC，恢复功能失效。
2. 本地 30 天清理由调度侧调用本工具 `delete` 实现，禁止依赖 VL retention 自动清理本地分区。
3. 过期备份淘汰交给 S3 桶生命周期规则；`gc` 不提供按保留期删除完整单元的能力（R-GC-04）。
4. `restore` 对超出 `vl_retention_days`（默认 181）窗口的目标直接拒绝（R-RS-10，退出码 6）。

---

## 9. 插件协议（PL）

> 规范文档（权威）：[manifest-spec.md](../../docs/plugin/manifest-spec.md)（插件 manifest v1）与 [handshake-protocol.md](../../docs/plugin/handshake-protocol.md)（OPS-PLUGIN-1）。
> 本域描述 vlogs-backup 作为插件被平台底座集成时的行为。**不改变既有 R 条目定义的 CLI 行为**；既有子命令在 task 形态下的业务语义与其 CLI 形态完全一致。

### R-PL-01 describe 输出

**当**执行 `vlogs-backup describe` **时**，系统应在退出码 0 下向 stdout 输出恰好一个 UTF-8 JSON 对象（插件 manifest v1），stderr 不承载协议输出。

验收：
- 输出经单次 JSON 解析成功，`manifestVersion=1`、`modes=["task"]`；
- 含 6 个 task（backup/restore/list/verify/delete/gc），每个 task 有 name/title/input schema/timeout；
- config schema 中 secret 项（`s3_secret_key`、`vl_password`、`vl_token`）标记 `secret: true`，且输出不含任何真实凭据值（C9）；
- manifest 通过 `internal/plugin` 的 `Validate()`（name/modes/binary.path/task 名唯一性/ui⇒daemon 约束）。

### R-PL-02 run 信封协议

**当**执行 `vlogs-backup run <task-name>` **时**，系统应从 stdin 读取一个 JSON 对象并翻译为对应子命令的 flag argv（JSON key 的 kebab-case 与 snake_case 等价），向 stdout 输出恰好一个 JSON 信封，随后退出。

验收：
- 成功：`{"ok": true, "data": <摘要 JSON>}`；失败：`{"ok": false, "error": {"code", "message", "retryable"}}`；
- stdout 除该信封外无任何其他内容；日志与进度一律走 stderr；
- 退出码映射符合设计文档 §11.2 插件协议映射表（原始 2/7→协议 2；原始 3→协议 3 且 retryable=true；原始 0→协议 0 等）；
- 未知 task 名返回协议退出码 2。

### R-PL-03 非交互注入与护栏保留

**当**以 task 形态执行 restore/delete **时**，系统应自动注入非交互默认值（`yes=true`、`json=true`），等价于非 TTY 确认路径（C10）；**且**危险开关（`force-today`、`force-no-backup`）不得被默认放行，仍需调用方在输入 JSON 中显式给出。

验收：`run restore` 恢复 UTC 今天分区且未传 `force_today` 时被拒（协议退出码 2，INPUT_INVALID）；显式传入后行为与 CLI `restore --yes --force-today` 一致。

### R-PL-04 原子命令 CLI 兼容

**系统应**保证新增 `describe` / `run` 子命令不改变既有 7 个子命令的参数解析、输出格式与退出码语义（R-OPS-01/02 不受影响）。

验收：`backup -h`、`restore -h`、`version`、`--help` 的输出与改造前一致；既有表驱动测试全部通过。

### R-PL-05 setup 无 task 形态

**系统应**不提供 `run setup`：执行时返回协议退出码 2，并在 stderr 说明 setup 为交互式向导、不适合 task 形态。

验收：`echo '{}' | vlogs-backup run setup` 退出码 2，无信封写入 stdout。

### R-PL-06 帮助页

**系统应**在 `vlogs-backup -h` 中列出 `describe` 与 `run`；`run -h` / `run --help` / `run help` 向 stdout 输出 run 专属帮助页（退出码 0），内容含可用 task 清单、stdin JSON 输入格式、信封格式与退出码表，并注明各 task 的完整参数 schema 以 `describe` 输出为权威来源；`run` 缺 task 名时向 stderr 输出该帮助页（退出码 2）。

验收：`vlogs-backup run -h` 退出码 0 且 stdout 非空；帮助文本含 `backup`、`restore`、`describe` 关键字。
