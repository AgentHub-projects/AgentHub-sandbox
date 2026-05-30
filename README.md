# AgentHub Sandbox

`AgentHub-sandbox` 是一个单 session 的 Go 沙箱服务，负责为 `leader` / `worker-*` 提供：

- 基于 Socket.IO 的文件实时读写
- 基于 Socket.IO 的命令执行与文件访问
- 基于 HTTP 的 git worktree / branch / merge / promote 编排

如果是第一次读这个项目，建议先看 [项目流程与代码结构概览](docs/project-overview.md)。

## 设计约束

- sandbox 不再接收 `sessionId`
- sandbox 不再要求外部传 `worktreeId`
- 内部只维护 `agentId -> WorktreeState` 的内存映射
- `/filesystem/socket.io` 与 `/agents/socket.io` 强制走 websocket transport
- `/git` 只提供编排能力，是否并入 `main` 由 leader 决定

## 配置

环境变量：

- `HOST`，默认 `0.0.0.0`
- `PORT`，默认 `8080`
- `REPO_ROOT`，默认 `/workspace/repo`
- `WORKTREE_ROOT`，默认 `/workspace-worktrees`

## HTTP 接口

### `GET /health`

返回：

```text
ok
```

### `POST /execute`

请求头优先读取 `X-AgentHub-Agent-Id`，兼容 query `agentId`。

请求：

```json
{
  "command": "go test ./..."
}
```

返回：

```json
{
  "stdout": "",
  "stderr": "",
  "exit_code": 0
}
```

### `GET /download/*path`

请求头优先读取 `X-AgentHub-Agent-Id`，兼容 query `agentId`。

返回文件原文。

### `GET /git/agents`

返回所有已由工具调用懒加载初始化过的 agent：

```json
{
  "items": [
    {
      "agentId": "leader",
      "branchName": "agent/leader",
      "rootPath": "/workspace-worktrees/leader",
      "headSha": "abc123",
      "preparedAt": "2026-05-27T12:00:00Z"
    }
  ]
}
```

### `GET /git/agents/{agentId}/status`

返回 branch、head、staged、unstaged、untracked、conflicted；如果 agent worktree 还不存在，会在处理请求时自动初始化。

### `GET /git/agents/{agentId}/diff?base=main`

返回文件级 diff 摘要和 patch。

### `POST /git/agents/{agentId}/complete`

agent/worker 完成时由 runtime 自动调用，用于检查该 agent worktree 并在存在改动时自动提交。没有改动返回 `status: "clean"`，有改动返回 `status: "committed"` 和 `commitSha`，存在冲突时返回 409。顶层 leader 会话完成后，runtime 会在 complete `agent/leader` 成功后调用 promote，把 `agent/leader` 合入 `main`。

```json
{
  "message": "agent(worker-1): complete session work",
  "authorName": "AgentHub Worker",
  "authorEmail": "worker@agenthub.local"
}
```

### `POST /git/agents/{agentId}/sync`

把主仓库目标 ref 合入该 agent 自己的 worktree。默认 `fromRef` 为 `main`；如果 worktree 有未提交内容，会返回 `status: "dirty"` 并拒绝合并。

```json
{
  "fromRef": "main",
  "noFF": false
}
```

### `POST /git/agents/{agentId}/merge`

请求：

```json
{
  "sourceAgentId": "worker-1",
  "noFF": false
}
```

语义：把 `sourceAgentId` 的 branch merge 到目标 agent 当前 worktree。

### `POST /git/agents/{agentId}/merge/abort`

返回：

```json
{
  "ok": true
}
```

### `POST /git/agents/{agentId}/promote`

请求：

```json
{
  "targetBranch": "main",
  "noFF": false
}
```

### `DELETE /git/agents/{agentId}/worktree`

删除 worktree 并从内存映射移除。

## Socket.IO

两个 socket path：

- `/filesystem/socket.io`
- `/agents/socket.io`

连接参数：

- `auth.agentId`，推荐
- `query.agentId`，兼容

通用 ack 返回：

```json
{
  "requestId": "req-1",
  "ok": true,
  "data": {}
}
```

错误返回：

```json
{
  "requestId": "req-1",
  "ok": false,
  "error": {
    "code": "WORKTREE_NOT_PREPARED",
    "message": "agent worktree is not prepared"
  }
}
```

### `/filesystem/socket.io`

- `fs:info`
- `fs:list`
- `fs:read`
- `fs:write`
- `fs:watch`
- `fs:unwatch`
- 服务端推送 `fs:changed`

### `/agents/socket.io`

- `agent:info`
- `exec:start`
- `exec:stdin`
- `exec:kill`
- `file:read`
- `file:write`
- 服务端推送 `exec:stdout`
- 服务端推送 `exec:stderr`
- 服务端推送 `exec:exit`
- 服务端推送 `exec:error`

## 内部模块

- `cmd/sandbox/main.go`：启动入口
- `internal/app`：装配所有模块
- `internal/worktree`：维护 `agentId -> WorktreeState`
- `internal/filesystem`：安全读写、版本戳、watch 广播
- `internal/executor`：命令执行、stdout/stderr 流式回传、kill/timeout
- `internal/gitmgr`：git worktree / status / diff / complete / merge / promote
- `internal/transport/httpapi`：HTTP 路由
- `internal/transport/socketio`：Socket.IO 事件
- `internal/security`：路径归一化和 worktree 越界保护

## 本地运行

```bash
go test ./...
go run ./cmd/sandbox
```
