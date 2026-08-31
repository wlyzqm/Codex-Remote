# Codex Remote

Codex Remote 是一个面向桌面、平板和手机浏览器的 Codex 远程控制台。静态 Web 客户端通过同源 Go receiver 访问一台 Linux 主机上的 Codex 会话，不需要单独的前端服务或数据库。

## 主要能力

- 新建、恢复、分叉、命名和归档会话
- 实时查看消息、推理摘要、计划、命令、文件变更和图片
- 向运行中的任务追加指令或中断任务
- 处理命令、文件、权限、用户问题和 MCP 请求
- 浏览当前项目文件并预览文本或图片
- 展示 Codex 多代理任务及父子关系
- 支持主题、字号、本地通知和 PWA 安装

## 快速开始

需要 Go 1.24 或更高版本，以及已经可用的 Codex 环境。

```bash
git clone https://github.com/wlyzqm/Codex-Remote.git
cd Codex-Remote
cp config.example.json config.json
chmod 600 config.json
```

在 `config.json` 中设置一个独立且不少于 12 字节的访问密码，然后构建并启动：

```bash
cd receive
go build -o codex-remote ./cmd/codex-remote
cd ..
./receive/codex-remote serve --config ./config.json --web-root ./send
```

默认在本机打开 `http://localhost:18787`。

## 公网访问

- 不要直接暴露 Codex app-server。
- 优先让 receiver 监听回环地址，再通过 HTTPS 反向代理访问。
- 应用必须部署在独立域名的根路径，不支持反向代理到子路径。
- 为每个部署设置唯一强密码，并保持 `config.json` 权限为 `0600`。
- 只需管理固定目录时，使用一个或多个 `--allow-root` 限制工作区。

更多启动参数见 [receiver 说明](receive/README.md)。

## 项目结构

```text
send/       浏览器与 PWA 客户端
receive/    Go receiver、测试和服务模板
config.example.json
```

前端说明见 [send/README.md](send/README.md)。

## 测试

```bash
node --test send/app.test.js send/markdown.test.js
cd receive && go test ./...
```

## 许可证

项目代码使用 [MIT License](LICENSE)。内置字体按 [HarmonyOS Sans 许可](send/fonts/HarmonyOS-Sans-LICENSE.txt) 分发。
