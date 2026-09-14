# Receiver

`receive/` 是 Codex Remote 的 Go 服务端。它提供登录、静态 Web 客户端、事件流、Codex RPC、审批和项目文件访问，并连接本机已有的 Codex daemon；daemon 不可用时可按需启动 app-server。

## 构建与启动

从项目根目录创建私有配置，并填写不少于 12 字节的独立密码：

```bash
cp config.example.json config.json
chmod 600 config.json
cd receive
go build -o codex-remote ./cmd/codex-remote
cd ..
./receive/codex-remote serve --config ./config.json --web-root ./send
```

默认访问地址为 `http://localhost:18787`。

## 运行参数

| 参数 | 默认值 | 用途 |
|---|---|---|
| `--listen` | `127.0.0.1:18787` | HTTP 或 HTTPS 监听地址 |
| `--config` | 可执行文件上一级的 `config.json` | 保存登录密码的私有 JSON 文件 |
| `--web-root` | 可执行文件上一级的 `send/` | Web 客户端目录 |
| `--codex-bin` | `auto` | Codex 可执行文件；`auto` 自动发现原生程序 |
| `--codex-home` | `$CODEX_HOME`，否则 `~/.codex` | Codex 状态目录 |
| `--app-server-mode` | `auto` | 选择 `auto`、`daemon` 或 `spawn` |
| `--daemon-socket` | `$CODEX_HOME/app-server-control/app-server-control.sock` | 指定受管 app-server Unix socket |
| `--idle-timeout` | `2m` | 安全空闲多久后关闭 app-server 连接；`0` 禁用 |
| `--session-ttl` | `720h` | 浏览器登录会话有效期 |
| `--tls-cert` | 空 | TLS 证书链；必须与 `--tls-key` 同时提供 |
| `--tls-key` | 空 | TLS 私钥；必须与 `--tls-cert` 同时提供 |
| `--allow-public-http` | `false` | 明确允许非回环地址上的明文 HTTP，仅用于隔离环境 |
| `--trusted-proxy` | `false` | 信任 TLS 反向代理传入的转发头 |
| `--allow-root` | 不限制 | 已弃用，仅兼容旧命令，不限制访问 |

`auto` 优先连接共享 daemon，失败后启动 app-server；`daemon` 只连接共享 daemon，`spawn` 总是启动独立 app-server。管理员可选择任意现存绝对目录。普通用户通过会话归属校验和命名沙箱权限访问指定工作区；所有账户共享这一连接及全局 16 个 RPC 并发名额，不创建用户专属 Codex 进程。

带有 `clientRequestId` 的任务创建和消息提交会在上传目录的 `.submissions/` 子目录保留接收记录与结果。重试同一标识会返回原结果；重启后无法确认结果的提交不会重复执行。该目录应随 receiver 状态保留，上传文件的过期清理不会删除它。

完整参数可运行：

```bash
./receive/codex-remote serve --help
```

## 安全边界

- 不要把原始 Codex app-server 暴露到网络。
- 公网访问应使用 HTTPS；非回环明文监听默认被拒绝。
- receiver 的权限等同于运行它的系统用户；原工作站密码对应的管理员拥有完整权限。普通账户由服务端校验会话归属、功能范围和运行参数，并通过 Codex 命名权限配置约束命令的文件与网络访问。
- `config.json`、私钥、运行状态和构建产物不应提交到 Git。
- 普通用户不能访问认证设置、用户管理或扩大沙箱权限。工作区不能包含系统目录、工作站密码文件、Codex 状态或 Remote 用户私有目录。MCP、插件、钩子及全局环境变量不会授予普通会话。

仓库中的 [systemd 模板](deploy/codex-remote.service) 仅作起点，使用前应调整用户、路径和 Codex 环境。

## 测试

```bash
cd receive
go test ./...
go vet ./...
```

请求历史保存在上传目录的 `.remote-state.json`；提交核对记录位于 `.submissions/`。通知推送和订阅接口已移除，附件保留到用户手动删除。

工作站认证接口 `/api/codex/settings` 使用 `--codex-home` 下的 `config.toml`、`auth.json`。配置档案存放在 `remote-auth-profiles.json`，最近一次写入前的配置保存在 `remote-auth-backup.json`，均为 0600。多文件写入有恢复日志，启动时恢复未完成的写入；编辑使用版本校验，拒绝覆盖其他客户端的更改。OAuth 配置切出时保存最新文件令牌。此功能管理文件凭据，不导出系统钥匙串，不代办 OAuth 登录。

TOML 读写使用 `github.com/pelletier/go-toml/v2`；其余服务端功能使用 Go 标准库。`POST /api/files/archive` 流式输出 ZIP，支持 2000 个选择、递归 20000 项、原文件合计 2 GiB；不跨出会话项目，目录中的符号链接需改选实际文件。HTML/SVG 预览使用独立 CSP 和 sandbox，不降低主页面的脚本限制。


## 多用户状态与验收

用户管理入口见[项目说明](../README.md#创建用户与分配权限)。原 `config.json` 格式不变，用户密码用带随机盐的 PBKDF2-SHA256 摘要保存。登录 Cookie 绑定用户修订号，修改权限或密码后旧 Cookie 立即失效。

用户状态默认位于 `--codex-home` 上一级的 `.cache/codex-remote/users/`：

| 路径 | 内容 |
|---|---|
| `users.json` | 用户配置与密码摘要，0600 |
| `threads.json` | 会话 ID 到用户 ID 的归属表，0600 |
| `<用户 ID>/uploads/` | 附件、请求历史与提交核对记录 |

Codex 会话本体继续保存在共用的 `CODEX_HOME` 中。应一并备份这两个位置；不要在仍保留普通用户时删除 `threads.json`，归属缺失的历史只向管理员开放。用户实时事件缓存各自最多 256 条、256 KiB，不复制整份工作站历史。管理员在主工作站访问全部会话；查看某个用户时仍沿用该用户的执行权限。

本机可执行不消耗模型额度的实际协议与文件沙箱检查：

```bash
cd receive
CODEX_REMOTE_TEST_ISOLATION=1 go test ./internal/server -run 'TestIsolatedUserBackendHandshake|TestNamedUserSandboxFilesystem' -v
```

需安装支持命名权限配置的 Codex 与可用的 Linux 沙箱；当前实现已用 0.154.0 验证。权限语义参见 [Codex permissions](https://learn.chatgpt.com/docs/permissions)。测试使用临时配置及文件，不读取工作站认证，也不发起模型对话。
