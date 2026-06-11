# Filesystem WebSocket API

本文档描述当前面向用户文件预览/编辑链路的 Socket.IO 接口。

> 注意：当前实现仍然走 WebSocket/Socket.IO。本文档只描述用户可见的 main 文件视图接口。

## 入口

```text
Socket.IO path: /filesystem/socket.io
Transport: websocket
```

通过 gateway 访问时：

```text
GET /filesystem/socket.io
```

一个 workspace 只绑定一个 sandbox，因此 gateway 不需要依赖 `sessionId` 选择 sandbox。

连接建立并校验通过后，sandbox 会自动订阅当前 main 文件视图根目录的变化；客户端不需要额外发送 watch/unwatch 事件。

## 连接参数

连接示例：

```ts
const socket = io(gatewayOrigin, {
  path: "/filesystem/socket.io",
  transports: ["websocket"]
});
```

字段说明：

| 字段 | 位置 | 必填 | 说明 |
|---|---|---:|---|
| 无 | - | - | 用户文件接口不需要 workspace/session/agent 选择参数 |

sandbox 默认把用户操作绑定到 `main` 分支所在的 repo root，不需要额外传工作区选择字段。

## 通用返回

所有客户端请求事件都支持 Socket.IO ack。

成功：

```json
{
  "requestId": "req-1",
  "ok": true,
  "data": {}
}
```

失败：

```json
{
  "requestId": "req-1",
  "ok": false,
  "error": {
    "code": "INVALID_PATH",
    "message": "path escapes workspace"
  }
}
```

如果调用方没有传 ack，服务端会推送：

```text
<event>:response
```

例如：

```text
fs:read:response
fs:update:response
```

## 错误码

常见错误码：

| code | 说明 |
|---|---|
| `WORKSPACE_NOT_READY` | main 文件视图尚未准备好 |
| `INVALID_PATH` | 路径非法，或试图逃逸工作区 |
| `INVALID_EDIT` | 提交的文本变更区间非法 |
| `VERSION_CONFLICT` | 写入时 `expectedVersion` 与当前文件版本不一致 |
| `INTERNAL_ERROR` | 其他内部错误 |

## 客户端请求事件

### `fs:list`

列出目录或文件。

请求：

```json
{
  "requestId": "list-1",
  "path": ".",
  "depth": 2
}
```

字段说明：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---:|---|
| `requestId` | string | 否 | 客户端请求 ID，用于关联响应 |
| `path` | string | 否 | 相对文件视图根目录的路径，默认可传 `"."` |
| `depth` | number | 否 | 遍历深度；小于等于 0 时按 1 处理 |

响应：

```json
{
  "requestId": "list-1",
  "ok": true,
  "data": {
    "entries": [
      {
        "path": "src/App.tsx",
        "name": "App.tsx",
        "kind": "file",
        "size": 1200,
        "mtime": "2026-06-04T05:30:00Z",
        "version": "1780121965847140350-1200"
      }
    ]
  }
}
```

`kind` 取值：

```text
file
dir
```

### `fs:read`

读取文件内容，支持按行裁剪。

请求：

```json
{
  "requestId": "read-1",
  "path": "src/App.tsx",
  "lineStart": 1,
  "lineEnd": 80
}
```

字段说明：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---:|---|
| `requestId` | string | 否 | 客户端请求 ID |
| `path` | string | 是 | 文件相对路径 |
| `lineStart` | number | 否 | 起始行，1-based；小于等于 0 表示从文件开头 |
| `lineEnd` | number | 否 | 结束行，包含该行；小于等于 0 表示到文件末尾 |

读取全文件：

```json
{
  "requestId": "read-all-1",
  "path": "README.md",
  "lineStart": 0,
  "lineEnd": 0
}
```

响应：

```json
{
  "requestId": "read-1",
  "ok": true,
  "data": {
    "path": "src/App.tsx",
    "content": "import React from \"react\";\n",
    "size": 1200,
    "mtime": "2026-06-04T05:30:00Z",
    "version": "40597e7f59193cbb8e3827ff585b355329b6ea989a9555e103f2645cbd6867fc"
  }
}
```

如果路径命中图片预览 manifest，`content` 为空，并额外返回 `preview`：

```json
{
  "requestId": "read-image-1",
  "ok": true,
  "data": {
    "path": "assets/generated/test.png",
    "content": "",
    "size": 123456,
    "mtime": "2026-06-09T00:00:00Z",
    "version": "...",
    "preview": {
      "path": "assets/generated/test.png",
      "previewUrl": "https://cdn.example.com/test.png",
      "mimeType": "image/png",
      "sizeBytes": 123456
    }
  }
}
```

`version` 是文件内容 hash，可用于后续写入的乐观锁。

### `fs:update`

提交文件变更。用户侧保存文件时，前端或 SDK 应自动对比本地编辑前后的内容，只把变化区间提交给 sandbox。变更应用成功后，sandbox 会自动提交到 `main`。

请求：

```json
{
  "requestId": "update-1",
  "path": "src/App.tsx",
  "expectedVersion": "40597e7f59193cbb8e3827ff585b355329b6ea989a9555e103f2645cbd6867fc",
  "edits": [
    {
      "startLine": 10,
      "startColumn": 3,
      "endLine": 12,
      "endColumn": 1,
      "text": "  return <main />;\n"
    }
  ],
  "createDirs": true
}
```

字段说明：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---:|---|
| `requestId` | string | 否 | 客户端请求 ID |
| `path` | string | 是 | 文件相对路径 |
| `expectedVersion` | string | 否 | 可选乐观锁；传入后服务端会校验当前内容 hash |
| `edits` | object[] | 是 | 文本变更列表，至少 1 项 |
| `createDirs` | boolean | 否 | 是否自动创建父目录 |

`edits` 字段说明：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---:|---|
| `startLine` | number | 是 | 起始行，1-based |
| `startColumn` | number | 是 | 起始列，1-based |
| `endLine` | number | 否 | 结束行，1-based；不传时等于 `startLine` |
| `endColumn` | number | 否 | 结束列，1-based；不传时等于 `startColumn` |
| `text` | string | 是 | 替换区间的新文本；空字符串表示删除 |

区间语义为 `[start, end)`：从起始位置开始，替换到结束位置之前。插入文本时，让起始位置和结束位置相同。

响应：

```json
{
  "requestId": "update-1",
  "ok": true,
  "data": {
    "path": "src/App.tsx",
    "size": 46,
    "mtime": "2026-06-04T05:31:00Z",
    "version": "6503f281e40232e4e13f1c75f757d966f087d3f45b0705819cf9d04cc9c1ff7c",
    "branchName": "main",
    "commitSha": "9fceb02a2f3d9d4f6e9e6ddcdb5f5f0df0c9a9e7"
  }
}
```

变更应用成功并自动提交后，服务端会主动推送 `fs:changed` 和 `main:committed`。

## 服务端推送事件

### `fs:changed`

连接建立后，当当前文件视图发生变化时，服务端主动推送该事件。

```json
{
  "path": "src/App.tsx",
  "changeType": "write",
  "mtime": "2026-06-04T05:31:00Z",
  "version": "6503f281e40232e4e13f1c75f757d966f087d3f45b0705819cf9d04cc9c1ff7c",
  "actor": "ui"
}
```

字段说明：

| 字段 | 类型 | 说明 |
|---|---|---|
| `path` | string | 变化文件或目录的相对路径 |
| `changeType` | string | 变化类型 |
| `mtime` | string | 文件或目录修改时间 |
| `version` | string | 文件内容 hash；目录或删除场景可能为空 |
| `actor` | string | 触发者，常见为 `ui`、`workspace` 或 `git` |

`changeType` 常见取值：

```text
write
create
remove
rename
```

### `main:committed`

当 `main` 分支产生新 commit 时，服务端主动推送该事件。它只用于通知“main 历史已经更新”，不携带文件列表或 diff 内容；客户端收到后按需调用 `GET /filesystem/git/main/commits` 和 diff 查询接口刷新提交历史或具体变更。

```json
{
  "branchName": "main",
  "commitSha": "9fceb02a2f3d9d4f6e9e6ddcdb5f5f0df0c9a9e7",
  "parentCommitSha": "4a1f4d6c8d6d0d4e86d2dbf9dfb9e2c31b85a012",
  "committedAt": "2026-06-05T08:12:30Z",
  "comment": "workspace: update src/App.tsx"
}
```

字段说明：

| 字段 | 类型 | 说明 |
|---|---|---|
| `branchName` | string | 固定为 `main` |
| `commitSha` | string | 新产生的 main commit hash |
| `parentCommitSha` | string | 新 commit 的第一父提交；初始 commit 时为空 |
| `committedAt` | string | commit 时间，RFC3339 UTC |
| `comment` | string | 完整 commit message |

触发场景：

| 场景 | 说明 |
|---|---|
| 用户侧 `fs:update` 自动提交 | 保存文件后 sandbox 自动提交到 `main` |
| leader promote/merge 到 main | agent 变更进入用户可见主分支 |
| 其他内部 main 写入收尾 | 只要最终产生新的 `main` commit，都应推送该事件 |

## HTTP 下载

虽然主要文件接口当前走 Socket.IO，但下载仍有 HTTP 入口。

通过 gateway：

```http
GET /filesystem/download/<path>
```

示例：

```http
GET /filesystem/download/src/App.tsx
```

响应：

```http
HTTP/1.1 200 OK
Content-Type: text/plain; charset=utf-8

<file content>
```

当前实现中 gateway 会把它改写到 sandbox：

```http
GET /download/<path>
```

## 客户端示例

```ts
import { io } from "socket.io-client";

const socket = io("http://agenthub-gateway.default.svc.cluster.local:8080", {
  path: "/filesystem/socket.io",
  transports: ["websocket"]
});

socket.on("fs:changed", (event) => {
  console.log("changed", event);
});

socket.on("main:committed", (event) => {
  console.log("main committed", event);
});

socket.emit(
  "fs:list",
  {
    requestId: "list-1",
    path: ".",
    depth: 2
  },
  (response) => {
    console.log(response);
  }
);
```

## 当前实现边界

- 用户文件接口默认读写 `main` 分支所在的 repo root。
- 路径必须是相对路径，不能逃逸文件视图根目录。
- `.git` 元数据目录不会出现在文件树中，也不能通过文件接口读写。
- `fs:update` 只接收文本变更区间，不要求用户侧传输整个文件内容。
- `fs:update` 成功后会自动创建一次 main 分支提交。
- `fs:changed` 的推送订阅跟随当前 Socket.IO 连接生命周期，断开后自动释放。
- `main:committed` 只做轻量通知；提交列表、文件列表、单文件 diff 仍走 main git HTTP API 查询。
