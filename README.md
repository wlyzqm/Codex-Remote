# Codex Remote

Codex Remote 是一个面向桌面、平板和手机浏览器的 Codex 远程控制台。静态 Web 客户端通过同源 Go receiver 访问一台 Linux 主机上的 Codex 会话，不需要单独的前端服务或数据库。

## 主要能力

- 新建、恢复、分叉、命名和归档会话
- 实时查看消息、推理摘要、计划、命令、文件变更和图片
- 向运行中的任务追加指令或中断任务
- 按任务保存文字草稿，断网后核对同一次发送，避免重复执行
- 处理命令、文件、权限、用户问题和 MCP 请求
- 浏览当前项目文件并预览文本或图片
- 展示 Codex 多代理任务及父子关系
- 支持主题、字号和 PWA 安装

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
- 登录后拥有工作站用户的文件访问能力；可浏览绝对路径、父目录、源码和符号链接目标。

更多启动参数见 [receiver 说明](receive/README.md)。

## 项目结构

```text
send/       浏览器与 PWA 客户端
receive/    Go receiver、测试和服务模板
config.example.json
```

前端说明见 [send/README.md](send/README.md)。

运行中的可见任务会定期同步其他 Codex 客户端产生的进展，连接设置中可查看最近同步时间。发送结果不明时，使用“核对发送”继续确认原提交；新建任务的首条消息失败后，会保留在同一任务中继续处理。文字和附件草稿按任务保存，刷新后恢复；附件依赖浏览器本地存储，清理站点数据会一并移除。

## 当前功能

- 历史列表按游标分页，可筛选项目或归档记录。
- 长会话默认渲染最近 80 项，点击顶部按钮加载更早消息并保留阅读位置。
- 附件支持刷新恢复、图片预览、取消上传、工作站列表和手动删除；不再自动删除历史附件。
- 请求中心保留最近 100 条处理记录，区分处理、过期、断连和结果未确认；这些记录用于回看，不是授权门槛。
- 任务菜单的“本轮结果”汇总回复、变更文件和命令退出码；命令成功不等于产品验收通过。
- 目标窗体展示后端提供的 tokensUsed、tokenBudget 和 timeUsedSeconds，不估算缺失数据。
- 连接设置统一为分组表单，提供主题、字号、额度、认证管理及连接诊断；已移除任务搜索、本地系统通知和后台推送。
- 流式消息保持滚动位置；在底部跟随新内容，上翻阅读时保留位置。
- 项目文件按文件夹优先排序，可跨目录勾选文件和文件夹打包 ZIP。默认源码预览并自动换行，可切换 Markdown、HTML、SVG、JSON 格式化预览。HTML/SVG 为隔离的静态展示，不运行脚本或加载外部资源。
- 工作站认证支持直接编辑 `config.toml` / `auth.json`，保存、切换和删除多个 API / 官方 OAuth 文件配置。切换只替换模型、服务商及认证字段，MCP、插件、钩子等保持共用；TOML 排版可能重排，配置值保留。保存不重载 Codex，下一次启动生效；手动重载会检查当前任务，共享 daemon 会影响其他连接。

receiver 不再配置路径保护区、强制自动审核或限制登录用户的工作目录。`--allow-root` 仅为旧启动命令保留兼容解析，不再限制访问。会话未加载时自动恢复后续发；结果不明的提交仍保留原标识核对，避免重复执行。

## 测试

```bash
node --test send/app.test.js send/markdown.test.js
cd receive && go test ./...
```

## 许可证

项目代码使用 [MIT License](LICENSE)。内置字体按 [HarmonyOS Sans 许可](send/fonts/HarmonyOS-Sans-LICENSE.txt) 分发。
