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

`auto` 优先连接共享 daemon，失败后启动 app-server；`daemon` 只连接共享 daemon，`spawn` 总是启动独立 app-server。登录后的用户可选择任意现存绝对目录，并可浏览工作站上的源码、配置与其他文件。

带有 `clientRequestId` 的任务创建和消息提交会在上传目录的 `.submissions/` 子目录保留接收记录与结果。重试同一标识会返回原结果；重启后无法确认结果的提交不会重复执行。该目录应随 receiver 状态保留，上传文件的过期清理不会删除它。

完整参数可运行：

```bash
./receive/codex-remote serve --help
```

## 安全边界

- 不要把原始 Codex app-server 暴露到网络。
- 公网访问应使用 HTTPS；非回环明文监听默认被拒绝。
- receiver 的权限等同于运行它的系统用户，登录用户拥有该系统用户的文件权限。
- `config.json`、私钥、运行状态和构建产物不应提交到 Git。
- 保留登录、来源校验、数据格式与资源大小校验，不额外加入工作区授权或文件审批证据门槛。

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
