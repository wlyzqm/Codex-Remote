# Web client

`send/` 是 Codex Remote 的静态 Web 与 PWA 客户端，适配桌面、平板和手机浏览器。它由 receiver 与 `/api/*` 在同一来源下提供，不需要构建步骤或前端运行时依赖。

## 提供的功能

- 管理会话、任务、目标和工作目录
- 实时展示消息、命令、文件变更、图片与多代理状态
- 发送文字、图片和普通文件
- 处理审批、权限、用户输入与 MCP 请求
- 浏览当前项目中的文件
- 切换主题和字号

## 运行方式

请通过项目根目录的 receiver 启动完整应用，具体步骤见 [项目 README](../README.md)。不要用 `file://` 直接打开 `index.html`，因为登录、API、事件流和文件访问都要求同源服务。

公网部署需要 HTTPS，并应挂载在域名根路径。访问密码不会写入浏览器存储或 Service Worker 缓存；登录后使用 receiver 设置的 HttpOnly Cookie。

## 文件

- `index.html`：页面结构与对话框
- `styles.css`：响应式界面样式
- `app.js`：认证、会话、事件流和交互
- `markdown.js`：安全 Markdown 渲染
- `manifest.webmanifest`、`icon.png`、`sw.js`：PWA 资源

## 验证

```bash
node --check send/app.js
node --check send/markdown.js
node --check send/sw.js
node --test send/app.test.js send/markdown.test.js
```

浏览器回归：运行 `node send/browser-fixture.cjs`（在项目根目录），打开 `http://127.0.0.1:18788/#thread=fixture`。该服务只使用虚构任务和认证信息；可通过 Chrome DevTools 调用 `runBrowserChecks()`。完成后停止服务并关闭测试页面。
