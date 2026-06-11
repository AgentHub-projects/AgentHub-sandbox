# 项目流程与代码结构概览

这个项目可以先不用从代码细节看起。可以先把它理解成一个给 agent 使用的远程工作区服务。

它做三件核心事情：

```text
1. 给每个 agent 分配一个独立工作目录
2. 允许 agent/前端读写这个目录里的文件、运行命令
3. 用 Git worktree 管理每个 agent 的分支、提交、合并
```

## 整体角色

项目里有两个重要目录概念：

```text
REPO_ROOT
主仓库目录，相当于 main 分支所在的根目录

WORKTREE_ROOT
agent 工作区根目录，每个 agent 会在这里有一个单独目录
```

例如：

```text
REPO_ROOT = /workspace/repo
WORKTREE_ROOT = /workspace-worktrees
```

那么可能会有：

```text
/workspace/repo                     main
/workspace-worktrees/leader          agent/leader
/workspace-worktrees/worker-1        agent/worker-1
/workspace-worktrees/worker-2        agent/worker-2
```

每个 agent 都不是直接改 `main`，而是在自己的 Git worktree 里改。

用户侧文件浏览/编辑接口不属于 agent worktree：它默认读写 `/workspace/repo` 的 `main` 视图，并在用户修改成功后自动提交。

## 启动流程

入口是：

```text
cmd/sandbox/main.go
```

它做的事很少：

```text
读取配置
调用 app.New(cfg) 创建服务
启动 HTTP server
监听退出信号，优雅关闭
```

真正的装配在：

```text
internal/app/app.go
```

这里会创建几个核心对象：

```text
worktree.Registry
记录 agentId -> worktree 状态

watcher.Hub
负责把文件变化广播给 socket 客户端

filesystem.Service
负责安全读写文件、监听文件变化

executor.Manager
负责在 agent worktree 里执行命令

gitmgr.Manager
负责 git worktree、commit、merge、sync、promote
```

然后 `app.New` 会把 HTTP 路由和 Socket.IO 服务都挂到同一个 `http.ServeMux` 上。

## 关键数据结构

核心内存表在：

```text
internal/worktree/registry.go
```

它维护的是：

```go
agentId -> State
```

`State` 里面主要有：

```text
AgentID       agent 名字，比如 leader / worker-1
BranchName    对应分支，比如 agent/leader
RootPath       这个 agent 的工作目录
HeadSHA        当前 commit
PreparedAt     创建时间
ActiveExecIDs  当前还在跑的命令
```

所以整个系统很多操作第一步都是：

```text
根据 agentId 找到它的 RootPath
```

然后再去读文件、写文件、运行命令、执行 git。

## 懒加载 agent

当前版本里，一个 agent 不需要提前调用 prepare。

例如前端第一次访问：

```text
GET /git/agents/leader/status
```

或者 socket 连上来要读文件：

```text
agentId = leader
fs:read README.md
```

代码会发现 registry 里没有 `leader`，于是走：

```text
gitManager.Ensure("leader")
```

内部会创建：

```text
WORKTREE_ROOT/leader
分支 agent/leader
```

也就是执行类似：

```bash
git worktree add -B agent/leader /workspace-worktrees/leader main
```

然后登记到 registry 里。

## HTTP 这条线

HTTP 路由在：

```text
internal/transport/httpapi/router.go
```

主要接口分两类。

兼容接口：

```text
GET /health
POST /execute
GET /download/*path
```

`/execute` 会根据 `agentId` 在对应 worktree 里跑命令。

Git 编排接口：

```text
GET    /git/agents
GET    /git/agents/{agentId}/status
GET    /git/agents/{agentId}/diff
POST   /git/agents/{agentId}/complete
POST   /git/agents/{agentId}/sync
POST   /git/agents/{agentId}/merge
POST   /git/agents/{agentId}/merge/abort
POST   /git/agents/{agentId}/promote
DELETE /git/agents/{agentId}/worktree
```

HTTP 层自己不做复杂业务，它基本就是：

```text
读请求参数
调用 gitmgr/filesystem/executor
把结果转成 JSON
```

## Socket.IO 这条线

Socket.IO 在：

```text
internal/transport/socketio/server.go
```

它分两条 socket path：

```text
/filesystem/socket.io
/agents/socket.io
```

`/filesystem/socket.io` 更像前端文件浏览器：

```text
fs:list
fs:read
fs:update
fs:changed  # 连接后自动推送
main:committed  # main 产生新提交后自动推送
```

`/agents/socket.io` 更像 agent runtime 通道：

```text
agent:info
file:read
file:write
exec:start
exec:stdin
exec:kill
exec:stdout
exec:stderr
exec:exit
exec:error
```

简单说：

```text
filesystem socket 偏 UI 文件操作，默认 main，不需要 agentId
agents socket 偏 agent 执行与读写
```

两条链路复用底层 `filesystem.Service`；用户写入还会调用 `gitmgr.Manager` 自动提交到 main。

## 文件读写流程

文件逻辑在：

```text
internal/filesystem/service.go
```

以写文件为例：

```text
收到 fs:update 或 file:write
fs:update 使用 main 视图，不需要 agentId
file:write 根据 agentId 找 agent worktree，如果 agent 不存在就自动 Ensure
校验路径不能逃出 worktree
如果传了 expectedVersion，就检查版本是否冲突
fs:update 应用用户提交的文本变更，file:write 写入 agent 工具传入的内容
fs:update 成功后自动提交到 main
广播 fs:changed
如果 main 产生新 commit，再广播 main:committed
```

路径安全很重要。比如 agent 想写：

```text
../../Windows/System32/xxx
```

会被 `internal/security` 拦掉，防止越权写到 worktree 外面。

文件变化广播靠：

```text
fsnotify + watcher.Hub
```

也就是说，前端连接 filesystem socket 后，会收到 main 文件视图的 `fs:changed`；main 历史更新时还会收到 `main:committed`，再按需查询 main commit 列表和 diff。agent 自己 worktree 的变化不会直接暴露成用户文件视图。

## 命令执行流程

命令执行在：

```text
internal/executor/manager.go
```

以 `exec:start` 为例：

```text
socket 收到 exec:start
executor 根据 agentId 找 worktree
命令的 cwd 默认就是这个 agent 的 RootPath
启动进程
stdout/stderr 边产生边推给 socket
命令结束后推 exec:exit
```

它支持两种模式：

```text
Shell = true
用 shell 跑，比如 powershell / sh -lc

Shell = false
直接执行 command + args
```

HTTP 的 `/execute` 是阻塞式接口，本质上也是复用这套执行逻辑，只是它等命令结束后一次性返回 stdout/stderr/exit_code。

## Git 流程

Git 核心在：

```text
internal/gitmgr/manager.go
```

这里是这个项目最重要、也最复杂的模块。

它负责：

```text
Ensure / Prepare
创建 agent worktree

Status
查看 agent 当前有哪些改动

Diff
查看 agent 相对 main 的 diff

Complete
agent 完成后，有改动就提交，没有改动返回 clean

Sync
把 main 合进某个 agent 分支

Merge
把 worker 分支合进 leader 分支

Promote
把 leader 分支最终合进 main

DeleteWorktree
删除 agent 工作区
```

一个比较典型的多 agent 流程是：

```text
1. leader 第一次被访问
   自动创建 /workspace-worktrees/leader
   分支 agent/leader

2. worker-1 第一次被访问
   自动创建 /workspace-worktrees/worker-1
   分支 agent/worker-1

3. worker-1 修改文件、运行命令

4. worker-1 工作结束
   调用 complete
   如果有改动，提交到 agent/worker-1

5. leader 决定接收 worker-1 的结果
   调用 merge
   把 agent/worker-1 合进 agent/leader

6. leader 自己整理、解决冲突、完成最终结果

7. leader complete
   把 leader 的未提交改动提交到 agent/leader

8. promote leader
   把 agent/leader 合进 main
```

用图表示大概是：

```text
main
 |
 +-- agent/leader       leader 的工作区
       |
       +-- merge agent/worker-1
       +-- merge agent/worker-2
       |
       +-- promote 回 main

main
 |
 +-- agent/worker-1     worker-1 的工作区
```

## 核心思想

它不是一个普通 Web 后端，而是一个 AgentHub 的沙箱协调层。

它不直接关心“agent 怎么思考”，它只负责提供这些基础能力：

```text
agent 要文件？我安全读给你
agent 要写文件？我写到它自己的 worktree
agent 要跑命令？我在它自己的目录里跑
agent 完成了？我帮它 commit
worker 要交给 leader？我帮它 merge
leader 最终确认？我帮它 promote 到 main
```

所以看代码时，可以按这个顺序看：

```text
1. internal/app/app.go
   看服务怎么组装

2. internal/worktree/registry.go
   看 agent 状态怎么存

3. internal/gitmgr/manager.go
   看 worktree / commit / merge 主流程

4. internal/filesystem/service.go
   看文件读写和监听

5. internal/executor/manager.go
   看命令怎么跑

6. internal/transport/httpapi/router.go
   看 HTTP 怎么暴露

7. internal/transport/socketio/server.go
   看 socket 事件怎么暴露
```

一句话总结：这个项目就是给多个 agent 分配独立 Git worktree，让它们能安全地读写文件、执行命令，并通过 commit/merge/promote 把工作成果一步步合回主分支。
