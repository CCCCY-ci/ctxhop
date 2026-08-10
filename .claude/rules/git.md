# Git 分支与提交规范（AgentSync）

---

## 1. 分支策略

### 1.1 受保护的 main

* `main` 必须始终可构建、可发布。
* **禁止直接向 main 提交**（仓库初始化提交除外）。所有改动通过 PR 合入。

### 1.2 分支命名

| 前缀 | 用途 | 示例 |
|---|---|---|
| `feature/` | 新功能 | `feature/s3-remote-driver` |
| `fix/` | 缺陷修复 | `fix/path-rewrite-unc-prefix` |
| `refactor/` | 不改变行为的重构 | `refactor/extract-shard-reader` |
| `docs/` | 只改文档 | `docs/prd-conflict-semantics` |
| `poc/` | 可行性验证，不要求达到生产质量 | `poc/claude-code-cross-device` |

### 1.3 必须从最新的 `origin/main` 切出

> [!WARNING]
> **永远从 `origin/main` 创建分支，而不是本地 `main`。**
> 本地 `main` 可能落后、领先或分叉，带着尚未推送、尚未验收的提交。从它切分支会把无关提交带进 PR，而 squash 合并会把分支上所有非 base 提交打包成一个 commit 合入。

```bash
git fetch origin
git checkout -b feature/your-feature origin/main
```

推送或开 PR 前必须自检提交范围：

```bash
git log --oneline origin/main..HEAD   # 只应看到本次功能的提交
```

发现混入无关提交，**先停下**，rebase 到 `origin/main` 或重切分支。

---

## 2. 提交信息（Angular 规范）

```text
<type>(<scope>): <subject>

<body>

<footer>
```

> [!IMPORTANT]
> 每个提交必须有 `<type>` 前缀，Header（首行）不超过 50 字符。

### 2.1 Type

`feat` `fix` `docs` `style` `refactor` `perf` `test` `build` `ci` `chore` `revert`

### 2.2 Scope（推荐填写）

按包名：`adapter` `remote` `crypto` `syncer` `project` `config` `cli` `repo` `docs`

### 2.3 Subject

简明描述；首字母不大写；结尾不加句号。

### 2.4 Body

**复杂改动必须写 Body，解释"为什么"而不是"改了什么"**（改了什么看 diff 就行）。
涉及 PRD 约束的改动，在 Body 中引用条款号。

### 2.5 示例

```text
feat(adapter): rewrite absolute paths on claude code restore

Session bodies embed the source machine's working directory in several
places, and the project directory name itself encodes the absolute path.
A plain copy therefore lands in a project the agent cannot resolve.

Paths outside the project root are left untouched rather than guessed
(§9.3, BR-10).
```

```text
fix(remote): map s3 NoSuchKey to ErrNotFound

Transport failures were surfacing as "not found", which made the sync
layer conclude the other device had pushed nothing and skip a fast
forward.
```

---

## 3. PR 流程

1. 从 `origin/main` 切分支
2. 按 Spec 开发（见 workflow.md），本地测试达标
3. `git commit`（本地提交）
4. **用户确认后**才 `git push`
5. 开 PR，说明改动动机与验证方式
6. Code Review 通过后 **Squash and Merge**，删除分支
7. 发布节点在 main 上打 SemVer tag（`v0.1.0`）

### 3.1 PR 前自检清单

- [ ] `gofmt -l .` 无输出
- [ ] `go vet ./...` 通过
- [ ] `go test -race -cover ./...` 通过且覆盖率达标
- [ ] `./scripts/build.sh` 六平台全部编译通过
- [ ] 无新增 cgo 依赖
- [ ] 无遥测、无对外网络请求
- [ ] 无真实会话数据、无凭据、无绝对路径进入仓库
- [ ] 受影响的文档已同步更新

---

## 4. 关于推送

本仓库的 push **不会触发任何生产部署**（没有服务端）。但仍然遵守：

> **未经用户明确确认，不执行 `git push`。**

原因是 push 到公开仓库不可撤销——一旦推上去，泄露的内容会进入 Git 历史和各种镜像。
