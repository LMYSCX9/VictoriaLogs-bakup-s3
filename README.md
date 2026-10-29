# vlogs-backup 运维手册

> 版本：v0.1.0 ｜ 适用平台：Linux / Windows × amd64 / arm64
> 关联文档：[设计文档](design/victorialogs-s3-backup-design.md)、[AGENTS.md](../AGENTS.md)
>
> 本手册描述工具的**已实现行为**，所有 flag、默认值、退出码均与 v0.1.0 二进制实测一致。

---

## 目录

1. [工具是什么](#1-工具是什么)
2. [核心概念](#2-核心概念)
3. [安装与版本自检](#3-安装与版本自检)
4. [五分钟上手](#4-五分钟上手)
5. [配置参考](#5-配置参考)
6. [子命令详解](#6-子命令详解)
7. [退出码总表](#7-退出码总表)
8. [JSON 输出契约](#8-json-输出契约)
9. [典型运维场景](#9-典型运维场景)
10. [故障处理与回滚](#10-故障处理与回滚)
11. [安全须知](#11-安全须知)
12. [已知限制与注意事项](#12-已知限制与注意事项)

---

## 1. 工具是什么

`vlogs-backup` 是一个 Go 单二进制工具，在 **VictoriaLogs 不停机**的前提下：

- 把按 UTC 天分区的历史日志**备份**到 S3 兼容对象存储（生产为 RustFS）；
- 按**指定日期**把某天日志**在线恢复**回 VL（detach → 替换 → attach，不重启进程）；
- 提供**校验、本地删除、S3 清理**等运维原子能力；
- 同时暴露**插件协议**（`describe` / `run <task>`，OPS-PLUGIN-1），可被平台底座作为插件集成调用（见 §6.10）。

工具不解析日志内容，备份/恢复的最小粒度是**一整天分区**。冷热分层（30 天本地保留、冷层落 S3）不是内置流水线，而是由你的调度侧把 `backup → delete` 等原子命令编排起来实现。

## 2. 核心概念

| 术语 | 含义 |
| --- | --- |
| 分区（Partition） | VL 数据目录下的 `partitions/YYYYMMDD`（UTC 日）。所有命令的 `--date` 都指这个日期。 |
| 快照（Snapshot） | VL 为某分区生成的硬链接视图，秒级完成、几乎不占额外空间。备份只读快照，绝不直接拷分区目录。快照在作业结束/启动时自动清理。 |
| 备份单元（Backup Unit） | S3 上某节点某天的完整备份：`files/` 下数据文件 + 一个 `manifest.json`。 |
| manifest.json | **唯一提交点**，最后写入。没有 manifest 的前缀视为未完成备份，`restore` 拒绝使用、`gc` 负责清理。 |
| cluster / node | S3 仓库的命名空间标签：`v1/<cluster>/<node>/<YYYYMMDD>/`。cluster 区分哪套环境，node 区分哪台 VL 实例。**定好后不要再改**，否则恢复时身份校验不通过。 |
| 保留窗口 | 与 VL `-retentionPeriod` 对齐的天数（默认 181 天）。恢复前会校验分区未超出窗口，超出直接拒绝。**VL 的 `-retentionPeriod` 必须 ≥181 天**，否则 attach 回来的分区会被 VL 原生 GC 删除。 |

## 3. 安装与版本自检

四平台静态二进制（`CGO_ENABLED=0`，无外部依赖）：

| 文件 | 目标平台 |
| --- | --- |
| `vlogs-backup-linux-amd64` | Linux x86_64 |
| `vlogs-backup-linux-arm64` | Linux ARM64 |
| `vlogs-backup-windows-amd64.exe` | Windows x86_64 |
| `vlogs-backup-windows-arm64.exe` | Windows ARM64 |

安装步骤：

```bash
# 1. 选择对应平台二进制，放到 PATH 或固定目录
cp vlogs-backup-linux-amd64 /usr/local/bin/vlogs-backup
chmod +x /usr/local/bin/vlogs-backup

# 2. 校验 sha256（对照发布包里的 sha256sums.txt）
sha256sum /usr/local/bin/vlogs-backup

# 3. 自检
vlogs-backup version
```

`version` 输出构建版本、commit、构建时间与 Go 版本；加 `--json` 输出单行 JSON。

## 4. 五分钟上手

```bash
# 第 1 步：交互式生成配置（仅需一次）
vlogs-backup setup
#    → 逐项询问 VL 地址、鉴权、数据目录、cluster/node、S3 信息
#    → 自动做 VL / S3 连通性验证，通过后才落盘
#    → 生成 /etc/vlogs-backup/config.toml（0644）+ credentials（0600）

# 第 2 步：备份 UTC 昨天（默认）
vlogs-backup backup

# 第 3 步：查看 S3 上已提交的备份
vlogs-backup list

# 第 4 步：校验昨天那份备份
vlogs-backup verify --date 2026-10-27

# 第 5 步（需要时）：把 2026-10-27 恢复回 VL
vlogs-backup restore --date 2026-10-27 --yes
```

以上每一步都输出摘要（人读表格，`--json` 时单行 JSON），退出码 0 = 成功。

## 5. 配置参考

### 5.1 三层配置与优先级

```
命令行 flag（仅 --config） > VLB_* 环境变量 > config.toml / credentials > 内置默认值
```

`--config` 指定配置文件路径；不指定时默认读 `/etc/vlogs-backup/config.toml`，**该文件不存在则跳过文件层**（仅用环境变量与默认值）。

### 5.2 config.toml（非敏感项，0644）

| 键 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `vl_url` | string | — | VL 地址，如 `http://127.0.0.1:9428`。**勿带路径后缀**，工具自行拼接 API 路径 |
| `vl_auth_mode` | string | `none` | VL（或前置 vmauth）鉴权模式：`none` / `basic` / `bearer` |
| `vl_retention_days` | int | `181` | 与 VL `-retentionPeriod` 对齐的保留窗口；restore 据此拒绝超窗分区 |
| `data_path` | string | — | VL `--storageDataPath` 的值 |
| `cluster` | string | `default` | 环境分组名，见 §2；**定好后不改** |
| `node` | string | `default` | 节点名（单机填主机名即可）；同 cluster 下必须唯一；**定好后不改** |
| `s3_endpoint` | string | — | S3 端点，如 `http://rustfs:9000` |
| `s3_region` | string | 空 | 区域（自建存储通常留空） |
| `s3_bucket` | string | — | 桶名 |
| `s3_prefix` | string | `vlogs` | 桶内前缀，备份落在 `<prefix>/v1/<cluster>/<node>/<date>/` |
| `s3_path_style` | bool | `true` | 自建 S3（RustFS/MinIO 等）保持 `true` |
| `s3_multipart_threshold` | int | `67108864` | 分段上传阈值（字节，64MiB） |
| `s3_multipart_part_size` | int | `8388608` | 分片大小（字节，8MiB） |
| `concurrency` | int | `8` | 上传/下载并发 worker 数 |
| `io_timeout` | duration | `10m` | 单次网络 IO 超时 |
| `job_timeout` | duration | `0`（不限） | 整个作业超时 |
| `disk_check_factor` | float | `1.1` | 恢复磁盘预检：可用空间须 ≥ 分区大小 × 此系数 |
| `orphan_snapshot_age` | duration | `2h` | 孤立快照年龄阈值，启动清理用 |

duration 写法：`10m`、`2h`、`30s`；`job_timeout` 填 `0` 表示不限。

### 5.3 credentials（敏感项，0600）

与 config.toml 同目录，可整文件缺席（凭据全走环境变量时）：

```toml
s3_access_key = "..."
s3_secret_key = "..."
vl_username   = "..."   # basic 模式
vl_password   = "..."   # basic 模式
vl_token      = "..."   # bearer 模式
```

### 5.4 VLB_* 环境变量（优先级高于文件）

| 环境变量 | 对应键 |
| --- | --- |
| `VLB_VL_URL` / `VLB_VL_AUTH_MODE` | `vl_url` / `vl_auth_mode` |
| `VLB_VL_USERNAME` / `VLB_VL_PASSWORD` / `VLB_VL_TOKEN` | credentials 中的同名敏感项 |
| `VLB_VL_RETENTION_DAYS` | `vl_retention_days` |
| `VLB_DATA_PATH` / `VLB_CLUSTER` / `VLB_NODE` | `data_path` / `cluster` / `node` |
| `VLB_S3_ENDPOINT` / `VLB_S3_REGION` / `VLB_S3_BUCKET` / `VLB_S3_PREFIX` | 同名键 |
| `VLB_S3_PATH_STYLE` | `s3_path_style` |
| `VLB_S3_ACCESS_KEY` / `VLB_S3_SECRET_KEY` | credentials 中的 S3 密钥 |
| `VLB_S3_MULTIPART_THRESHOLD` / `VLB_S3_MULTIPART_PART_SIZE` | 同名键 |
| `VLB_CONCURRENCY` / `VLB_IO_TIMEOUT` / `VLB_JOB_TIMEOUT` | 同名键 |
| `VLB_DISK_CHECK_FACTOR` / `VLB_ORPHAN_SNAPSHOT_AGE` | 同名键 |

### 5.5 各命令必填项

缺必填项一律退出码 2，报错形如 `missing required config vl_url (set via flag, VLB_* env or config file)`。

| 命令 | 必填项 |
| --- | --- |
| `backup` / `restore` / `delete` | `vl_url`、`data_path`、`s3_endpoint`、`s3_bucket` |
| `list` / `verify` / `gc` | `s3_endpoint`、`s3_bucket` |
| `setup` | 无（不读已有配置） |

鉴权组合校验：`basic` 必须提供用户名+密码；`bearer` 必须提供 token，否则启动即退出码 2。

## 6. 子命令详解

### 6.0 命令树总览

```text
vlogs-backup
│
├── setup                交互式初始化向导（必须 TTY；无 task 形态）
├── backup               在线备份（CLI 形态，详见 6.2）
├── restore              在线恢复（CLI 形态，详见 6.3）
├── list                 列举已提交备份（CLI 形态，详见 6.4）
├── verify               校验备份完整性（CLI 形态，详见 6.5）
├── delete               删除本地分区（CLI 形态，详见 6.6）
├── gc                   清理 S3 未完成前缀（CLI 形态，详见 6.7）
├── version              版本自检（仅 CLI，无 task 形态）
│
└── 插件协议（OPS-PLUGIN-1，详见 6.10）
    ├── describe         输出插件 manifest JSON（stdout 单个 JSON 对象）
    └── run <task-name>  task 形态执行子命令，stdin 收 JSON、stdout 出信封
        可用 task：backup / restore / list / verify / delete / gc
        （setup 为交互向导、version 无参数，均不提供 task 形态）
```

两种形态的分工：**CLI 形态**给人与 cron/脚本用（flag 参数、表格摘要）；**task 形态**给平台底座调用（JSON 参数、信封输出）。同一命令的业务行为（护栏、确认矩阵、退出码语义）在两种形态下完全一致。

### 6.1 setup —— 交互式初始化

```text
vlogs-backup setup [--output-dir DIR] [--skip-check] [--json]
```

| flag | 默认 | 说明 |
| --- | --- | --- |
| `--output-dir` | `/etc/vlogs-backup` | 配置生成目录 |
| `--skip-check` | 关 | 跳过 VL / S3 连通性验证 |
| `--json` | 关 | 结果单行 JSON 输出到 stdout（向导交互文案转 stderr） |

**向导流程**（必须在 TTY 中运行，非交互环境拒绝、退出码 2）：

1. 若目标 `config.toml` 已存在，要求输入 `overwrite` 确认覆盖，其他输入取消（退出码 7）；
2. 按固定顺序询问：

| 顺序 | 询问项 | 默认值 | 备注 |
| --- | --- | --- | --- |
| 1 | VL 地址 | `http://127.0.0.1:9428` | 校验 http/https 格式 |
| 2 | VL 鉴权模式 | `none` | `none/basic/bearer` 之外重问 |
| 3 | VL 用户名 / 密码 | — | 仅 basic 模式；密码不回显 |
| 3' | VL Token | — | 仅 bearer 模式；不回显 |
| 4 | 数据目录 | 必填 | 必须绝对路径 |
| 5 | cluster | `default` | 环境分组名（§2） |
| 6 | node | `default` | 节点名（§2） |
| 7 | S3 endpoint | `http://127.0.0.1:9000` | |
| 8 | S3 region | 空 | 可留空 |
| 9 | S3 bucket | 必填 | |
| 10 | S3 prefix | `vlogs` | |
| 11 | path-style | `true` | 输入 true/false |
| 12 | S3 Access Key / Secret Key | 空 | 可留空；不回显 |

3. 连通性验证：VL 走快照 list 能力探测，S3 走 HeadBucket（各 10 秒超时）。失败时选择 `[r]` 重试 / `[v]` 重新输入 / `[a]` 中止（中止退出码 3）。**验证未通过前不落盘任何文件**；
4. 落盘 `config.toml`（0644）与 `credentials`（0600，全空凭据时不生成）；
5. 输出生成文件清单。

> 交互说明：每项询问前有一行以 `·` 引导的中文说明，解释该配置的用途与取值形式；输错可用退格键修正（`^H` 与 `^?` 均有效，兼容不同 SSH 客户端键码），Ctrl-U 清空整行重输，Ctrl-C 随时中止（退出码 7）。

**实际输入示例解读**（一次真实会话，每行输入对应什么）：

```text
VL 地址              : http://192.168.1.53:9428      ← VictoriaLogs 的 HTTP 地址；工具在它后面拼 API 路径
                                                        （/internal/partition/...），所以不填 /logs/ 之类的路径后缀
VL 鉴权模式          : basic                         ← VL（或前置 vmauth）要求 Basic Auth 时选 basic；
                                                        VL 本身没开鉴权就选 none（选了 none 不会再问用户名密码）
VL 用户名            : victoriametrics               ← 仅 basic 模式询问
VL 密码              : ********                      ← 仅 basic 模式询问；不回显，写入 credentials 文件
数据目录             : /vmlog-data                   ← VL 启动参数 --storageDataPath 的值，工具到这里的
                                                        partitions/ 下做快照/替换/删除
cluster              : default                       ← S3 仓库的环境分组名（§2），单环境直接回车用 default
node                 : 192.168.1.53                  ← 这台 VL 实例的名字，单机填主机名或 IP，同 cluster 内唯一
S3 endpoint          : http://192.168.1.133:9000     ← RustFS/MinIO 等兼容存储的地址，不带桶名不带路径
S3 region            : (空)                          ← 自建存储留空即可（内部占位 us-east-1）
S3 bucket            : vlogs-backup                  ← 桶名，需已存在；备份落在
                                                        <bucket>/<s3_prefix>/v1/<cluster>/<node>/<日期>/
S3 prefix            : vlogs                         ← 桶内前缀，多套工具共用一个桶时区分
使用 path-style      : true                          ← 自建 S3 一律 true；只有 AWS S3 等需要虚拟主机寻址才改 false
S3 Access Key        : AKIA...（不回显）             ← 必填！留空则 backup/restore 等全部命令退出码 2
S3 Secret Key        : ********（不回显）            ← 同上，写入 credentials 文件
```

输入完成后自动做连通性验证（VL 探测 + S3 HeadBucket），通过后才落盘。

**本次会话（skywalking）需要修正的 3 处输入**：

| 问题 | 存进了什么 | 后果 | 修正 |
| --- | --- | --- | --- |
| VL 地址带 Markdown 反引号 | `` `http://192.168.1.53:8427/logs/` `` | 旧版二进制勉强容忍（本次会话 `backup` 仍到达了 VL，报错在快照匹配阶段）；**新版工具会直接拒绝**（退出码 2，`vl url ... must use http or https`） | 编辑 `/etc/vlogs-backup/config.toml` 去掉反引号：`http://192.168.1.53:8427/logs`。本次会话已证实该地址可达（报错发生在快照路径匹配而非连接阶段），`/logs` 为前置代理（vmauth）路由前缀时**保留**，末尾斜杠可有可无 |
| S3 endpoint 带反引号 | `` `http://192.168.1.133:9000` `` | 新版工具同理拒绝 | 改为 `http://192.168.1.133:9000` |
| S3 Access Key / Secret Key 已正常输入 | credentials 文件两项已写入（setup 摘要以 `***` 掩码显示，掩码即非空） | 无 | 无需补录；若后续 S3 操作报鉴权错误（退出码 1/3），再到 RustFS 控制台核对该 AK/SK |

修正后用 dry-run 验证配置可用：

```bash
vlogs-backup backup --date 2026-10-27 --dry-run
```

> 注意：凭据不会出现在日志与摘要中；向导里输入的地址不要带 Markdown 反引号等多余字符（详见上表「本次会话需要修正」）。

### 6.2 backup —— 在线备份

```text
vlogs-backup backup [--date 日期 | --from 起 --to 止 | --latest N] [选项]
```

| flag | 默认 | 说明 |
| --- | --- | --- |
| `--date` | UTC 昨天 | 备份单日分区。接受 `YYYY-MM-DD` 或 `YYYYMMDD` |
| `--from` / `--to` | — | 批量区间（含端点），两者必须成对出现 |
| `--latest N` | — | 备份最近 N 天（含 UTC 今天） |
| `--if-exists` | `skip` | 该日期已有已提交备份时：`skip`（跳过）/ `replace`（先传新数据再删旧对象） |
| `--keep-snapshot` | 关 | 作业后保留快照用于排障（摘要会提示磁盘占用，需人工清理） |
| `--dry-run` | 关 | 只探测 VL 能力并列出计划分区，不上传 |
| `--json` | 关 | 单行 JSON 摘要；批量时每天一行（JSONL） |
| `--config` | `/etc/vlogs-backup/config.toml` | 配置文件路径 |

**日期选择三种方式互斥**，同时给出退出码 2。

**行为要点**：

1. 自动获取作业锁（同 cluster/node/date 并发第二个作业退出码 6）；
2. 启动时清理历史孤立快照；对 UTC 今天分区自动强制 flush（快照只含已落盘数据）并在摘要标 `today=true` 提示可能有约 1 秒内存数据差异；
3. 数据文件来自 VL 快照（硬链接，不读原分区），并发上传，HEAD 大小一致的文件跳过（幂等续传，`skipped` 计数）；
4. `manifest.json` 最后写入，写入成功即提交；
5. 无论成败，作业结束清理快照；
6. 进度按文件/字节节流输出到 stderr（不污染 stdout 摘要）。

示例：

```bash
vlogs-backup backup                          # 备份 UTC 昨天
vlogs-backup backup --date 2026-10-20        # 指定日期
vlogs-backup backup --from 2026-10-01 --to 2026-10-07   # 区间批量
vlogs-backup backup --latest 30              # 最近 30 天（含今天）
vlogs-backup backup --date 2026-10-20 --if-exists=replace   # 重备并替换旧备份
vlogs-backup backup --date 2026-10-20 --dry-run             # 演练
```

### 6.3 restore —— 在线恢复

```text
vlogs-backup restore --date 日期 [选项]
```

| flag | 默认 | 说明 |
| --- | --- | --- |
| `--date` | UTC 昨天 | 恢复的分区日期 |
| `--on-existing` | `keep` | 目标分区已存在时：`keep`（旧目录改名为 `<date>.replaced-<ts>` 保留在数据目录下）/ `delete`（新数据校验全部通过后删除旧目录） |
| `--deep` | 关 | 下载后逐文件比对 sha256（默认只比对大小） |
| `--force-today` | 关 | 允许恢复 UTC 今天分区（会造成实时写入缺口） |
| `--allow-node-mismatch` | 关 | 放行 manifest 中 cluster/node 与当前配置不一致的备份（摘要标 `cross_node=true`） |
| `--yes` | 关 | 跳过交互确认（**非交互环境必须显式给出**） |
| `--dry-run` | 关 | 探测 VL 能力并打印计划，不下载不替换 |
| `--json` | 关 | 单行 JSON |
| `--config` | 同上 | |

**确认矩阵**（先于配置加载检查）：

| 场景 | 要求 |
| --- | --- |
| 恢复 UTC 今天分区 | 必须显式 `--force-today`，否则退出码 2 |
| 非 TTY（cron/脚本） | 必须显式 `--yes`，否则退出码 2 |
| TTY + 今天分区 | 输入完整日期串（如 `2026-10-27`）确认；输 `y`/空行/错日期退出码 7 |
| TTY + 非今天分区 | 输入 `yes` 确认 |

**执行流程**（对应设计文档 §7.2，任一步失败自动回滚，见 §10.1）：

1. 前置探测：VL 版本能力检查（过老退出码 6）；
2. 拉取 manifest，校验 cluster/node 身份、partition 日期、format_version；
3. 校验分区日期未超出保留窗口（`vl_retention_days`），超出退出码 6；
4. 磁盘预检：可用空间 ≥ 备份大小 × `disk_check_factor`；
5. 并发下载到数据目录下的临时目录，逐文件校验；
6. detach 分区 → rename 原子替换 → attach；
7. 冒烟查询该天是否有数据，摘要字段 `smoke_query`：`ok` / `empty`（attach 成功但查询为空）/ `failed`（不影响退出码，仅告警日志）。

```bash
vlogs-backup restore --date 2026-10-27 --yes
vlogs-backup restore --date 2026-10-27 --deep --on-existing=delete --yes
vlogs-backup restore --date 2026-10-27 --dry-run
```

### 6.4 list —— 列举已提交备份

```text
vlogs-backup list [--from 起 --to 止] [--json]
```

| flag | 默认 | 说明 |
| --- | --- | --- |
| `--from` / `--to` | 不限 | 过滤日期区间（含端点），必须成对出现，否则退出码 2 |
| `--json` | 关 | 单行 JSON（聚合后的单元/对象/字节合计） |

只展示**有 manifest 的完整备份单元**。文本输出每行：日期、文件数、字节数，末尾有汇总行。

```bash
vlogs-backup list
vlogs-backup list --from 2026-10-01 --to 2026-10-27
```

### 6.5 verify —— 校验备份完整性

```text
vlogs-backup verify [--date 日期] [--deep] [--json]
```

| flag | 默认 | 说明 |
| --- | --- | --- |
| `--date` | 全部 | 只校验该日备份；不给则逐一校验所有已提交单元 |
| `--deep` | 关 | 下载全量对象比对 sha256（默认只比对对象大小与计数） |
| `--json` | 关 | 单行 JSON（聚合摘要） |

轻校验快速（只读 manifest 与 HEAD）；`--deep` 消耗流量但能发现内容损坏。任何分区校验失败 → 退出码 5，stderr 打印缺失/不一致的对象明细。

```bash
vlogs-backup verify                     # 全量轻校验（可进 cron）
vlogs-backup verify --date 2026-10-27 --deep
```

### 6.6 delete —— 删除本地分区（高危）

```text
vlogs-backup delete [--date 日期 | --from 起 --to 止] [选项]
```

| flag | 默认 | 说明 |
| --- | --- | --- |
| `--date` | UTC 昨天 | 删除单日本地分区 |
| `--from` / `--to` | — | 批量区间（含端点），成对出现 |
| `--dry-run` | 关 | 只输出确认提示与规模统计，不执行删除 |
| `--yes` | 关 | 跳过交互确认（**非交互环境必须显式给出**） |
| `--force-no-backup` | 关 | 允许删除没有已提交备份的分区（数据将无法找回） |
| `--force-today` | 关 | 允许删除 UTC 今天分区（造成实时日志缺口） |
| `--json` | 关 | 单行 JSON；批量时每天一行 |

**护栏与确认**：

1. 非 TTY 环境未给 `--yes` → 退出码 2；
2. 本地分区不存在 → 退出码 4；
3. 删 UTC 今天分区未给 `--force-today` → 退出码 6；
4. 无已提交备份未给 `--force-no-backup` → 退出码 6（防止删掉唯一副本）；
5. TTY 中必须**输入完整日期串**（如 `2026-10-27`）确认，输 `y`/空行/错日期 → 退出码 7；批量时逐天独立确认；
6. 无论哪种模式（含 dry-run）都会先输出 6 字段确认提示：分区日期、绝对路径、文件数、字节数、是否有备份、影响说明。

**执行顺序**（不可逆操作，顺序固定）：detach → rename 移出 `partitions/`（改名为数据目录下 `.deleting-<date>-<ts>`）→ 递归删除 → 复查。启动时自动清理历史 `.deleting-*` 残留。**`delete` 永远不碰 S3 对象**——删 S3 备份是 `gc` / 桶生命周期的职责。

```bash
vlogs-backup delete --date 2026-10-01 --dry-run        # 先看规模
vlogs-backup delete --date 2026-10-01                  # 交互确认
vlogs-backup delete --date 2026-10-01 --yes            # cron/脚本
vlogs-backup delete --from 2026-09-01 --to 2026-09-30 --yes   # 30 天窗口清理
```

### 6.7 gc —— 清理 S3 未完成前缀

```text
vlogs-backup gc [--stale-after 24h] [--yes] [--json]
```

| flag | 默认 | 说明 |
| --- | --- | --- |
| `--stale-after` | `24h` | 无 manifest 前缀的陈旧阈值（对象最后修改时间早于 now−阈值 才清理） |
| `--yes` | 关 | **执行删除；缺省即 dry-run，只打印计划** |
| `--json` | 关 | 单行 JSON |

只清理「无 manifest 的未完成前缀」（中断作业的残留）。含 manifest 的完整单元**不在默认范围**——超过 181 天的淘汰交给 S3 桶生命周期规则（RustFS 配置 181d 过期）。人工确需移除完整单元时，保持「先删 manifest 后删数据」顺序。

```bash
vlogs-backup gc                        # 默认演练，打印待删清单
vlogs-backup gc --yes                  # 真正删除
vlogs-backup gc --stale-after 1h --yes
```

### 6.8 version —— 版本自检

```text
vlogs-backup version [--json]
```

输出构建版本、commit、构建时间、Go 版本。`--json` 为单行 JSON：

```json
{"version":"0.1.0","commit":"e1a2b3c","build_time":"2026-10-28T08:00:00Z","go_version":"go1.x"}
```

### 6.9 通用约定

- `--config` 可作所有命令（除 `version`）的全局选项：`vlogs-backup --config /path/config.toml backup`，或作为子命令 flag：`backup --config /path/config.toml`；
- `--date` 一律按 **UTC** 解释，接受 `2026-10-27` 与 `20261027` 两种写法；
- 日志（slog 文本、含时间戳/op/cluster/node）全部走 **stderr**，stdout 只有摘要，方便管道与重定向；
- 路径与错误信息带绝对路径，便于直接定位。

### 6.10 插件协议（describe / run）

> 规范文档：[manifest-spec.md](docs/plugin/manifest-spec.md)、[handshake-protocol.md](docs/plugin/handshake-protocol.md)（OPS-PLUGIN-1）。
> 本组子命令供平台底座把 vlogs-backup 作为插件集成时调用；不影响上述 CLI 子命令的任何行为。

#### describe —— 输出插件 manifest

```bash
vlogs-backup describe
```

退出码 0 时向 stdout 输出**恰好一个** JSON 对象（插件 manifest v1），包含：6 个 task 声明（含每个 task 的 JSON Schema 参数说明与超时）、完整 config schema（secret 项标记 `"secret": true`，不含真实凭据值）、permissions。stderr 不承载协议输出。

各 task 的**参数文档以 describe 输出为权威来源**（`run` 的参数名、类型、枚举、默认值都在其中）。

#### run —— task 形态执行子命令

```bash
echo '{"date": "2026-10-27", "dry_run": true}' | vlogs-backup run backup
```

- **输入**：stdin 传一个 JSON 对象，key 为该 task 的参数名（与 CLI flag 同名，kebab-case / snake_case 等价，如 `if-exists` 与 `if_exists`）；无参数传 `{}` 或留空；
- **输出**：stdout 恰好一个 JSON 信封；日志与进度一律走 stderr：

```json
{"ok": true,  "data": { "op": "backup", "status": "success", ... }}
{"ok": false, "error": {"code": "INPUT_INVALID", "message": "...", "retryable": false}}
```

- **帮助**：`vlogs-backup run -h`（或 `--help` / `help`）输出专属帮助页；`run` 缺 task 名也输出帮助（退出码 2）。

**task 形态的自动注入与护栏**（业务护栏与 CLI 形态完全一致，只改变确认方式）：

| 注入项 | 范围 | 说明 |
| --- | --- | --- |
| `json=true` | 全部 task，强制注入不可覆盖 | 信封包装依赖单行 JSON 输出 |
| `yes=true` | 仅 restore / delete，可被显式传入覆盖 | 等价非 TTY 确认路径 |

危险开关**不做默认放行**：`run restore` 恢复 UTC 今天分区仍需显式传 `"force_today": true`；`run delete` 无备份分区仍需 `"force_no_backup": true`；`run gc` 默认仍是演练，真删需 `"yes": true`。

#### 插件协议退出码

进程以 task 形态退出时，按 §7 的原始退出码分类映射为协议退出码：

| 协议退出码 | 来源（§7 原始码） | 信封 error.code |
| --- | --- | --- |
| 0 | 0 | —（ok=true） |
| 1 | 其他未列举码 | EXEC_FAILED |
| 2 | 2、7 | INPUT_INVALID |
| 3 | 1、3、4、5、6 | EXEC_FAILED（原始 3 时 `retryable=true`） |
| 4 | — | PERMISSION_DENIED（当前工具不产生） |
| 124 | — | 由底座超时强杀产生 |

#### 底座集成要点

- 底座通过 `describe` 完成安装校验与扩展点注册（manifest v1 校验规则见规范文档）；
- 底座通过 `run <task>` 调用业务能力，按信封 `ok` 字段判定结果、按协议退出码触发重试/告警；
- 配置经底座注入（`<DATA_DIR>/config.json` 文件或 `OPS_PLUGIN_CONFIG` 环境变量，语义等价），config schema 见 describe 输出。

## 7. 退出码总表

cron / 告警消费的唯一依据，语义固定不得复用：

| 码 | 含义 | 典型场景 |
| --- | --- | --- |
| 0 | 成功 | |
| 1 | 未分类错误 / 回滚失败 | 回滚自身也失败，需人工介入 |
| 2 | 参数或配置错误 | 缺 `--yes`、缺必填配置、flag 非法、日期非法 |
| 3 | 远端不可达 / 网络失败 | VL / S3 连不上 |
| 4 | 目标不存在 | 恢复目标备份不存在；删除的本地分区不存在 |
| 5 | 校验失败 | verify 哈希/大小不一致；manifest 不兼容 |
| 6 | 前置条件不满足 | 并发锁冲突；VL 版本过老；超出保留窗口；删今天/无备份分区未授权 |
| 7 | 用户取消 | 交互确认输错或放弃 |

批量作业（backup/delete）按天独立执行，最终退出码取各天中的**最大值**；单天失败不中断其他天。

> 插件协议形态（`run <task>`）的进程退出码按上表分类映射为协议退出码（0/1/2/3/4/124），映射表见 §6.10；cron/告警消费 CLI 形态时直接使用上表原始码。

## 8. JSON 输出契约

所有命令 `--json` 时 stdout 只有一行（批量作业每天一行）JSON 摘要。**例外**：`describe` 输出单个 manifest JSON 对象、`run <task>` 输出单个信封（均见 §6.10），不遵循本节契约；下表适用于其余子命令。基础字段：

| 字段 | 说明 |
| --- | --- |
| `op` | setup / backup / restore / list / verify / delete / gc |
| `status` | success / error / canceled |
| `cluster` / `node` | 身份 |
| `partition` | YYYY-MM-DD（UTC） |
| `files` / `bytes` | 文件数 / 总字节 |
| `elapsed_ms` | 耗时毫秒 |
| `error_code` | 失败时的退出码（成功无此字段） |

op 专属字段（仅对应命令出现）：

| 字段 | 出现于 | 说明 |
| --- | --- | --- |
| `partitions` | backup / delete 批量 | 计划/处理的日期数组 |
| `skipped` | backup | HEAD 命中跳过的对象数 |
| `today` | backup | 是否含 UTC 今天分区（有内存缓冲差异） |
| `snapshot_retained` | backup | `--keep-snapshot` 时为 true，磁盘待人工清理 |
| `smoke_query` | restore | ok / empty / failed |
| `replaced_path` | restore | 旧目录保留路径（on-existing=keep） |
| `cross_node` | restore | 跨节点恢复标注 |
| `path` / `has_backup` | delete | 分区绝对路径 / 是否有已提交备份 |
| `dry_run` | backup / restore / delete / gc | 演练模式 |
| `units` / `objects` / `leftovers` | gc | 单元数 / 删除对象数 / 残留对象数 |

告警示例（cron 判断退出码，附 JSON 摘要进告警正文）：

```bash
vlogs-backup backup --json >> /var/log/vbackup.log 2>> /var/log/vbackup.err
rc=$?
[ $rc -ne 0 ] && echo "vlogs-backup failed rc=$rc $(tail -1 /var/log/vbackup.log)" | alert
```

## 9. 典型运维场景

### 9.1 cron 每日备份 + 告警

```cron
# 每天 02:30 备份 UTC 昨天（VL 与 cron 均按 UTC 口径）
30 2 * * * vlogs-backup backup --json >> /var/log/vlogs-backup.log 2>&1
```

退出码非 0 时按 §7 表分类告警；`skipped` 大说明重复备份在空转（检查上游是否重复调度）。

### 9.2 冷热分层（30 天本地窗口，调度侧编排）

工具只提供原子能力，分层序列由调度侧组装：

```bash
# 每日序列（调度侧）：
vlogs-backup backup                    # 1. 备份 UTC 昨天；退出 0 才继续
vlogs-backup verify --date $(UTC昨天)  # 2. 轻校验
# 3. 每月一次：对 30 天前的分区做 --deep 抽查
vlogs-backup delete --from <30天前> --to <30天前> --yes   # 4. 释放本地磁盘
# 5. >181 天的 S3 对象由桶生命周期规则淘汰（不需要本工具）
```

顺序保证：`backup` 退出码 0（manifest 已提交）之后才允许 `delete`；若误删了有备份的分区，`delete` 的护栏会拦（退出码 6）。

### 9.3 单天误删恢复

```bash
# 确认 S3 上有该天备份
vlogs-backup list --from 2026-10-27 --to 2026-10-27

# 恢复（目标分区存在时旧目录自动保留，可回退）
vlogs-backup restore --date 2026-10-27 --deep --yes

# 查询验证数据回来了，确认无误后清理保留的旧目录
vlogs-backup verify --date 2026-10-27
```

### 9.4 磁盘紧张快速释放

```bash
vlogs-backup delete --dry-run                       # 先看各分区规模与备份状态
vlogs-backup delete --from 2026-09-01 --to 2026-09-30 --yes
```

### 9.5 定期完整性审计

```bash
# 每周日全量轻校验；每月随机一天 deep 校验
0 4 * * 0 vlogs-backup verify --json
0 5 1 * * vlogs-backup verify --date $(上月随机一天) --deep
```

### 9.6 gc 日常巡检（cron 默认演练）

```bash
# 演练输出直接进日志，出现 leftovers 增长再人工 --yes
0 6 * * * vlogs-backup gc --json
```

## 10. 故障处理与回滚

### 10.1 restore 失败回滚矩阵

| 失败点 | 现场状态 | 工具自动动作 | 人工动作 |
| --- | --- | --- | --- |
| 下载/校验阶段 | 目标分区未动 | 删除临时目录，原状态不变 | 重跑即可 |
| detach 后、替换前 | 分区已摘除 | attach 原目录恢复原状 | 查网络/S3 后重跑 |
| 替换后、attach 前 | 新目录就位，旧目录已改名 | 旧目录改回原名 → attach | 查 VL 日志后重跑 |
| attach 失败 | 目录正确但未挂载 | 重试；仍失败输出含绝对路径的人工命令 | 手工执行提示的 attach 命令 |
| 回滚自身失败 | 不确定 | 停止后续步骤（退出码 1），打印当前目录状态 + 两条可选人工命令 + 数据风险说明 | 按输出逐条确认执行 |

原则：**回滚不追求恢复原样，而追求不存在半可用状态**——宁可停在「分区缺失 + 明确指引」，也不 attach 半成品目录。

### 10.2 delete 失败

| 失败点 | 处理 |
| --- | --- |
| rename 后、删除前崩溃 | 数据目录残留 `.deleting-<date>-<ts>`；下次任一变更性命令启动时自动清理（也可手工删） |
| detach 成功但 rename 失败 | 自动 attach 回原分区，退回 present |

### 10.3 常见报错速查

| 报错（关键字） | 退出码 | 处置 |
| --- | --- | --- |
| `missing required config` | 2 | 按 §5.5 补配置 |
| `refuses to run without --yes` | 2 | 非 TTY 环境显式加 `--yes` |
| `pass --force-today to proceed` | 2/6 | 确实要操作今天分区再加授权 |
| `pass --force-no-backup to proceed` | 6 | 该分区无备份，删除不可恢复；确认知情后加 flag |
| `vl version too old` | 6 | 升级 VictoriaLogs 至支持分区管理端点的版本 |
| `another job holds the lock` | 6 | 同日期作业已在跑；确认后清理锁或等待 |
| `no committed backup found`（restore） | 4 | 该日期无完整备份单元，先 `list` 确认 |
| `vl returned no snapshot for partition`（backup） | 4 | 三步排查：① `curl -u <user>:<pass> -X POST <vl_url>/internal/partition/list` 确认分区存在（日期按 UTC 解释）；② 确认配置 `data_path` 与 VL `--storageDataPath` 一致——工具直接读取快照硬链接，**必须与 VL 存储同机部署**；③ 确认使用 2026-10-29 修复后构建的二进制（旧版误把快照基名当分区日期匹配，与真实 VL 快照路径 `<dataPath>/partitions/<YYYYMMDD>/snapshots/<快照名>/` 不符，必然报此错） |
| `partition ... beyond retention window` | 6 | 分区超出 `vl_retention_days`，确认后再考虑放宽配置 |
| `hash mismatch` / `size mismatch`（verify） | 5 | 备份对象损坏，用 `backup --if-exists=replace` 重备该天 |

## 11. 安全须知

- **凭据零落盘到日志**：日志、摘要、错误信息、manifest、命令回显中均不含凭据（自动化扫描覆盖）；credentials 文件权限 0600，config.toml 0644；
- 密码/Token 输入不回显；`setup --json` 时交互文案转 stderr，stdout 不泄漏；
- 优先用 `VLB_*` 环境变量或受限文件权限承载凭据；不要把 `--yes` 与凭据写进同一个可被他人读取的脚本；
- 恢复今天分区、删除无备份分区这类**不可逆/影响实时写入**的操作都有独立授权 flag，勿在调度脚本里顺手开启。

## 12. 已知限制与注意事项

1. **VL `-retentionPeriod` 必须 ≥181 天**（与 `vl_retention_days` 对齐）：设成 30d 会让 attach 恢复的分区立刻被 VL 原生 GC 删除。本地 30 天窗口清理由调度侧调 `delete` 实现，不依赖 VL retention。
2. **批量 `backup --json` 是 JSONL**（每天一行 JSON），不是严格单行；消费方按行解析。
3. `delete --dry-run` 会在数据目录创建 `.vlogs-backup/locks` 锁目录（只读副作用）。
4. 备份粒度固定为一整天；不支持单条日志、字段级恢复，不支持 PITR。
5. 恢复冒烟查询返回 `empty` 不算失败：可能该天备份本身就是空分区或索引仍在构建，用 `verify` 与业务查询进一步确认。
6. 快照是硬链接视图，不额外占数据空间，但 `--keep-snapshot` 保留的快照需人工清理（摘要会提示）。
7. cluster/node 定好后不要再改；跨环境恢复需显式 `--allow-node-mismatch` 并理解风险。
8. 工具自身重试只发生在网络访问层（指数退避有限次）；作业层失败即终止并回滚，不做整体重跑——重跑由调度侧决定。
