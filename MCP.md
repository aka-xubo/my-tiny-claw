# MCP 配置

CLI 启动时会从 `~/.claw/mcp.json` 加载 MCP Server。文件不存在时不会影响内置工具。

可以复制仓库中的 [`mcp.example.json`](mcp.example.json) 作为起点。配置完成后，运行 CLI 即可在启动阶段发现远程工具；工具会以 `mcp__<server>__<tool>` 的名称提供给模型。

支持两种 HTTP 传输：

- `streamable-http`：有状态连接，适用于支持 MCP `2025-11-25` 的 Server。
- `stateless-http`：优先使用 MCP `2026-07-28` 无状态连接；旧 Server 会回退为有状态连接并记录警告。

带 OAuth 的 Server 在首次启动时会打开浏览器授权。客户端凭据和 Token 保存在 `~/.claw/mcp-oauth.json`，文件权限会自动设置为 `0600`。

Header 支持 `${ENV_NAME}` 环境变量展开。不要把 Token 直接写入配置文件；使用 HTTP 传输敏感 Header 时，凭据会以明文发送。

第一版暂不支持 stdio、旧 SSE、MCP Resources、Prompts、Sampling、Elicitation、Roots，以及 Session 失效后的自动重建。
