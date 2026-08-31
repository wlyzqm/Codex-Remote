# Receiver

`receive/` 是 Codex Remote 的 Go 服务端。它提供登录、静态 Web 客户端、事件流、受限 Codex RPC、审批和项目文件访问，并连接本机已有的 Codex daemon；daemon 不可用时可按需启动 app-server。

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
| `--allow-root` | 不限制 | 限制可远程选择的工作区根目录，可重复传入 |

`auto` 优先连接共享 daemon，失败后启动 app-server；`daemon` 只连接共享 daemon，`spawn` 总是启动独立 app-server。未传入 `--allow-root` 时可选择任意现存绝对目录，但配置、Codex 状态、Web 资源、receiver 和 TLS 私钥等保护路径仍不可直接作为工作区或文件目标。

完整参数可运行：

```bash
./receive/codex-remote serve --help
```

## 安全边界

- 不要把原始 Codex app-server 暴露到网络。
- 公网访问应使用 HTTPS；非回环明文监听默认被拒绝。
- receiver 的权限等同于运行它的系统用户，应使用专用账户或 `--allow-root` 收窄工作区。
- `config.json`、私钥、运行状态和构建产物不应提交到 Git。
- 浏览器请求和审批会在 receiver 中重新校验，前端按钮不是安全边界。

仓库中的 [systemd 模板](deploy/codex-remote.service) 仅作起点，使用前应调整用户、路径和 Codex 环境。

## 测试

```bash
cd receive
go test ./...
go vet ./...
```
