# Main Git HTTP API

本文档描述面向用户侧的 main 分支提交历史和 diff 查询接口。

> 约束：这两个接口只允许操作 `main` 分支视图，不需要也不接受 `agentId` 或 `sessionId`。

## 通用约定

一个 workspace 只绑定一个 sandbox，因此接口不需要携带 workspace/session 选择参数：

```http
GET /filesystem/git/main/commits
GET /filesystem/git/main/commits/{commitSha}/diff/files
GET /filesystem/git/main/commits/{commitSha}/diff/file
```

返回时间统一使用 RFC3339 UTC 字符串。`comment` 表示 Git commit message 的完整文本。

`main` 分支有新提交时，sandbox 会通过 `/filesystem/socket.io` 推送 `main:committed` 事件。该事件只做轻量通知，客户端收到后再调用本文档里的 HTTP 接口刷新提交列表、变更文件列表或单文件 diff。

## 错误返回

```json
{
  "code": "INVALID_COMMIT",
  "message": "commit is not reachable from main"
}
```

错误码：

| code | 说明 |
|---|---|
| `WORKSPACE_NOT_READY` | main 仓库尚未初始化 |
| `INVALID_COMMIT` | commit hash 为空、格式非法，或不是 main 历史上的 commit |
| `INVALID_PATH` | 文件路径为空、非法，或不在该 commit 的差异列表中 |
| `INTERNAL_ERROR` | 其他内部错误 |

## 查询 Main Commit 列表

```http
GET /filesystem/git/main/commits?limit=100&cursor=<commit-sha>
```

语义：

- 返回 `main` 分支提交历史，按时间从新到旧排序。
- 不传 `limit` 默认返回 100 条。
- `limit` 最大 500。
- `cursor` 表示从某个 commit 之后继续向更旧的历史分页。
- 如果需要“所有 commit”，客户端按 `nextCursor` 连续翻页，直到 `hasMore=false`。

响应：

```json
{
  "branchName": "main",
  "items": [
    {
      "commitSha": "9fceb02a2f3d9d4f6e9e6ddcdb5f5f0df0c9a9e7",
      "committedAt": "2026-06-05T08:12:30Z",
      "comment": "workspace: update src/App.tsx"
    }
  ],
  "hasMore": true,
  "nextCursor": "4a1f4d6c8d6d0d4e86d2dbf9dfb9e2c31b85a012"
}
```

字段说明：

| 字段 | 类型 | 说明 |
|---|---|---|
| `branchName` | string | 固定为 `main` |
| `items[].commitSha` | string | 40 位 commit hash |
| `items[].committedAt` | string | commit 时间 |
| `items[].comment` | string | 完整 commit message |
| `hasMore` | boolean | 是否还有更旧的提交 |
| `nextCursor` | string | 下一页 cursor；没有更多提交时为空 |

## 查询不同文件列表

```http
GET /filesystem/git/main/commits/{commitSha}/diff/files
```

语义：

- `commitSha` 必须是 `main` 历史上的 commit。
- 返回这个 commit 相对它前一个 commit 的所有不同文件。
- merge commit 使用第一父提交作为“之前”版本。
- 初始 commit 没有父提交时，和空树比较，所有文件都视为新增。
- 这个接口只返回文件级摘要，不返回具体文件内容。

响应：

```json
{
  "branchName": "main",
  "commitSha": "9fceb02a2f3d9d4f6e9e6ddcdb5f5f0df0c9a9e7",
  "parentCommitSha": "4a1f4d6c8d6d0d4e86d2dbf9dfb9e2c31b85a012",
  "files": [
    {
      "path": "src/App.tsx",
      "status": "modified",
      "additions": 3,
      "deletions": 1
    },
    {
      "path": "src/NewPage.tsx",
      "status": "added",
      "additions": 42,
      "deletions": 0
    },
    {
      "path": "src/NewName.tsx",
      "oldPath": "src/OldName.tsx",
      "status": "renamed",
      "additions": 2,
      "deletions": 2
    }
  ]
}
```

字段说明：

| 字段 | 类型 | 说明 |
|---|---|---|
| `branchName` | string | 固定为 `main` |
| `commitSha` | string | 当前 commit |
| `parentCommitSha` | string | 当前 commit 的第一父提交；初始 commit 时为空 |
| `files[].path` | string | 当前 commit 中的文件路径 |
| `files[].oldPath` | string | rename 场景下上一版本中的旧路径 |
| `files[].status` | string | `added`、`modified`、`deleted`、`renamed` |
| `files[].additions` | number | 新增行数 |
| `files[].deletions` | number | 删除行数 |

## 查询单文件具体更改

```http
GET /filesystem/git/main/commits/{commitSha}/diff/file?path=<file-path>
```

语义：

- `commitSha` 必须是 `main` 历史上的 commit。
- `path` 必须来自 `GET /filesystem/git/main/commits/{commitSha}/diff/files` 返回的 `files[].path`。
- 返回这个文件在 commit 之前的原始内容和 unified diff。
- 新增文件时 `baseFile.exists=false`，此时 `baseFile.content=null`。

响应：

```json
{
  "branchName": "main",
  "commitSha": "9fceb02a2f3d9d4f6e9e6ddcdb5f5f0df0c9a9e7",
  "parentCommitSha": "4a1f4d6c8d6d0d4e86d2dbf9dfb9e2c31b85a012",
  "path": "src/App.tsx",
  "oldPath": "",
  "status": "modified",
  "baseFile": {
    "path": "src/App.tsx",
    "exists": true,
    "content": "export default function App() {\n  return null;\n}\n",
    "isBinary": false
  },
  "patch": "diff --git a/src/App.tsx b/src/App.tsx\n..."
}
```

字段说明：

| 字段 | 类型 | 说明 |
|---|---|---|
| `branchName` | string | 固定为 `main` |
| `commitSha` | string | 当前 commit |
| `parentCommitSha` | string | 当前 commit 的第一父提交；初始 commit 时为空 |
| `path` | string | 当前 commit 中的文件路径 |
| `oldPath` | string | rename 场景下上一版本中的旧路径 |
| `status` | string | `added`、`modified`、`deleted`、`renamed` |
| `baseFile.exists` | boolean | commit 之前是否存在该文件 |
| `baseFile.content` | string/null | commit 之前的原始文件内容；二进制文件为 `null` |
| `patch` | string | 该文件的 unified diff 文本 |

## 实现建议

Commit 列表可以用：

```text
git log main --format=%H%x1f%cI%x1f%B%x1e
```

Diff 可以用：

```text
git rev-list --parents -n 1 <commit>
git diff --find-renames --name-status <parent> <commit>
git diff --find-renames --numstat <parent> <commit>
git diff --find-renames --patch <parent> <commit> -- <path>
git show <parent>:<old-path>
```

如果 `<commit>` 是初始 commit，没有父提交，则 `<parent>` 使用 Git 空树：

```text
4b825dc642cb6eb9a060e54bf8d69288fbee4904
```

commit 校验建议：

```text
git merge-base --is-ancestor <commit> main
```

如果 commit 不在 `main` 历史上，返回 `INVALID_COMMIT`。
