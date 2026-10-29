# 插件 Manifest 规范 v1

状态：草案（`manifestVersion = 1`）
关联文档：[插件握手协议](./handshake-protocol.md)

## 1. 概述

Manifest 是插件向平台底座声明自身能力的**唯一事实来源**。底座依据它完成安装校验、注册扩展点、渲染 UI 入口、注入配置与权限。

设计原则：

1. **单一事实来源**：插件二进制的 `describe` 子命令输出为权威 manifest。插件包内可附带静态 `manifest.json` 仅供安装前预览，安装后以 `describe` 输出为准。
2. **最小必要**：只声明底座真正需要的字段，不做前瞻性设计。
3. **stdout 只出 JSON**：`describe` 的 stdout 必须只有单个 JSON 对象；日志、进度一律走 stderr。
4. **声明制权限**：插件不隐式获得任何能力。所有对外访问（存储、事件、HTTP、密钥、调度）必须显式声明，由底座审批并注入。

## 2. 获取方式

底座安装/校验插件时执行：

```
<plugin-binary> describe
```

- 输入：无（stdin 必须被忽略）
- 输出：stdout 单个 UTF-8 JSON 对象（本规范）
- 退出码：`0` 成功；非 0 视为无效插件

## 3. 顶层结构

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| manifestVersion | int | 是 | 固定 `1` |
| name | string | 是 | 插件唯一标识，`^[a-z][a-z0-9-]{1,62}$`，全局唯一 |
| displayName | string | 是 | 展示名 |
| version | string | 是 | SemVer，如 `1.4.2` |
| apiVersion | string | 是 | 依赖的底座 API 版本，如 `"1.x"` |
| description | string | 否 | 一句话描述 |
| modes | string[] | 是 | `["task"]` / `["daemon"]` / 两者 |
| binary | object | 是 | 启动方式，见 §3.1 |
| tasks | task[] | 条件 | `modes` 含 `task` 时必填且 ≥1，见 §3.2 |
| service | object | 条件 | `modes` 含 `daemon` 时必填，见 §3.3 |
| ui | object | 条件 | 有 UI 入口时必填；**声明 ui 必须包含 daemon 模式**，见 §3.4 |
| permissions | string[] | 否 | 声明制权限，见 §3.5 |
| config | map | 否 | 配置项 schema，见 §3.6 |

### 3.1 binary

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| path | string | 是 | 相对插件安装目录的可执行文件路径（含扩展名，如 `bin/monitor-agent.exe`） |
| args | string[] | 否 | 固定启动参数，置于子命令之前 |

底座完整启动方式：`<root>/<path> <args...> <subcommand> ...`。

子命令约定（三个保留字，插件不得挪作他用）：

| 子命令 | 用途 |
|---|---|
| `describe` | 输出 manifest（本规范） |
| `run <task>` | 执行一次性任务（[握手协议 §3](./handshake-protocol.md)） |
| `serve` | 启动常驻服务（[握手协议 §4](./handshake-protocol.md)） |

### 3.2 task

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| name | string | 是 | 任务名，`^[a-z][a-z0-9_-]{1,62}$`，插件内唯一 |
| title | string | 是 | 展示名 |
| description | string | 否 | 说明 |
| input | object | 否 | JSON Schema（draft 2020-12），底座用于校验 stdin 输入 |
| timeout | int | 否 | 默认 `60`，单位秒；超时强杀 |

### 3.3 service

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| protocol | string | 否 | 默认 `http.v1`；可选 `grpc.v1` |
| readyTimeout | int | 否 | 默认 `10`，单位秒；等待握手行 + ready 的上限 |
| stopTimeout | int | 否 | 默认 `15`，单位秒；优雅停止上限，超时强杀 |
| liveness | object | 否 | `{ "intervalSeconds": 15, "failureThreshold": 3 }`，均为默认值 |

### 3.4 ui

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| entries | entry[] | 是 | ≥1 |

entry 结构：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| kind | string | 是 | `iframe` / `link` / `page` |
| title | string | 是 | 菜单/入口标题 |
| path | string | 是 | 底座内挂载路径，如 `/monitor` |
| target | string | kind=iframe/link 时必填 | iframe：插件服务内的相对路径；link：外部 URL |
| icon | string | 否 | 图标标识 |
| order | int | 否 | 菜单排序，默认 `100` |

v1 约束：`iframe` 经底座反向代理访问插件服务（插件只监听回环，用户流量统一过底座鉴权/审计）；`page`（微前端）与外链 `link` 为保留字，v1 校验器可拒绝。

### 3.5 permissions

权限以字符串表达，格式 `<scope>:<glob>`：

| scope | 含义 | 示例 |
|---|---|---|
| storage.plugin-scope | 读写自身命名空间 KV | `storage.plugin-scope:*` |
| events.publish | 发布事件 | `events.publish:monitor.*` |
| events.subscribe | 订阅事件 | `events.subscribe:deploy.*` |
| http.call | 出站 HTTP | `http.call:https://api.example.com/*` |
| schedule | 定时调度 | `schedule:*/5 * * * *` |
| secret.read | 读密钥前缀 | `secret.read:monitor/` |

说明：

- `ui.menu` 由 `ui.entries` 隐式给出，不单独声明。
- v1 中 `http.call` 的实际放行由底座出站代理实现；插件进程自身网络不受强制拦截（进程级沙箱属 v2+ 演进），但未声明的出站访问会在审计中标记。
- `secret.read` 仅声明前缀，实际值经配置注入通道下发（§5），插件不得直接访问底座密钥存储。

### 3.6 config

`config` 是有序 map，key 为配置名 `^[a-z][a-zA-Z0-9_]{1,62}$`：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| type | string | 是 | `string` / `int` / `bool` / `enum` / `stringList` |
| title | string | 是 | 展示名 |
| description | string | 否 | 帮助文本 |
| default | any | 否 | 默认值，类型须匹配 type |
| required | bool | 否 | 默认 `false` |
| enum | string[] | type=enum 时必填 | 可选值 |
| secret | bool | 否 | 默认 `false`；`true` 时 UI 不回显、日志不落明文 |

## 4. 完整示例

```json
{
  "manifestVersion": 1,
  "name": "monitor-agent",
  "displayName": "监控采集",
  "version": "1.4.2",
  "apiVersion": "1.x",
  "description": "主机与服务指标采集、本地可用性探测",
  "modes": ["task", "daemon"],
  "binary": {
    "path": "bin/monitor-agent.exe",
    "args": []
  },
  "tasks": [
    {
      "name": "probe",
      "title": "立即探测",
      "description": "对指定目标执行一次可用性探测",
      "input": {
        "type": "object",
        "properties": {
          "target": { "type": "string", "description": "host:port 或 URL" },
          "timeoutSeconds": { "type": "integer", "minimum": 1, "maximum": 300, "default": 10 }
        },
        "required": ["target"]
      },
      "timeout": 120
    }
  ],
  "service": {
    "protocol": "http.v1",
    "readyTimeout": 10,
    "stopTimeout": 15,
    "liveness": { "intervalSeconds": 15, "failureThreshold": 3 }
  },
  "ui": {
    "entries": [
      { "kind": "iframe", "title": "监控总览", "path": "/monitor", "target": "/", "order": 10 }
    ]
  },
  "permissions": [
    "storage.plugin-scope:*",
    "events.publish:monitor.*",
    "events.subscribe:deploy.*",
    "http.call:https://alert.example.com/*",
    "schedule:*/5 * * * *",
    "secret.read:monitor/"
  ],
  "config": {
    "listenPort": {
      "type": "int", "title": "服务端口", "default": 9201, "required": true
    },
    "scrapeInterval": {
      "type": "int", "title": "采集间隔(秒)", "default": 30
    },
    "logFormat": {
      "type": "enum", "title": "日志格式", "default": "json", "enum": ["json", "text"]
    },
    "extraLabels": {
      "type": "stringList", "title": "附加标签", "default": []
    },
    "remoteToken": {
      "type": "string", "title": "远端令牌", "secret": true, "required": true
    }
  }
}
```

## 5. 配置注入

底座将用户配置以两个**语义等价**的通道注入插件进程，插件任选其一读取：

1. 文件：`<OPS_PLUGIN_DATA_DIR>/config.json`，形如 `{"listenPort": 9201, "remoteToken": "..."}`；
2. 环境变量：`OPS_PLUGIN_CONFIG`，同一 JSON 的 UTF-8 字符串。

规则：

- secret 项只经这两个通道传递，底座保证不出现在日志与审计明文中；
- 插件对 config 的修改不回写，变更统一由底座保存；
- v1 配置变更以**重启插件进程**生效（无热 reload）。

## 6. 底座校验清单

安装/启用插件时，底座依次执行：

1. `describe` 退出码为 `0`，stdout 为合法 JSON；
2. `manifestVersion` 受支持（v1 底座只接受 `1`）；
3. `name` / `version` / `apiVersion` 符合语法；`name` 与已装插件不冲突；
4. `modes` 合法；`tasks` / `service` / `ui` 条件满足（声明 `ui` ⇒ 含 `daemon`）；
5. `tasks[].name` 唯一，`input` 为合法 JSON Schema；
6. `permissions` 逐条可解析（未知 scope 拒绝安装或标记，取决于底座策略）；
7. `config` 类型 / 默认值 / enum 自洽。

任一步失败：插件置 `INVALID`，不注册任何扩展点。

## 7. 目录约定

```
<base-data>/
  plugins/
    registry.json          # 已安装插件索引：name、version、状态、binary 摘要
    <name>/                # 插件安装目录（二进制 + 静态资源）
    data/<name>/           # 插件运行时数据目录（即 OPS_PLUGIN_DATA_DIR）
```

Windows 注意：运行中的 exe 不能被覆盖，插件升级采用"停旧 → rename 交换 → 起新"，见[握手协议 §5.3](./handshake-protocol.md)。

## 8. 演进规则

- `manifestVersion` 递增即破坏性变更；v1 内只允许**追加可选字段**；
- 底座必须拒绝 `manifestVersion` 高于自身支持的插件；
- `apiVersion` 兼容策略：底座支持 N 与 N-1。
