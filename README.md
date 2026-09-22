# cpa-plugin-opencodezen

<div align="center">

![logo](logo.svg)

**OpenCode Zen free-tier models as a native CLIProxyAPI provider**

[![Go](https://img.shields.io/badge/go-1.26-00ADD8?logo=go)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)
[![Release](https://img.shields.io/github/v/release/Victor9578/cpa-plugin-opencodezen?include_prereleases)](https://github.com/Victor9578/cpa-plugin-opencodezen/releases)

</div>

用于 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 的 **原生执行器插件**，让你可以直接在 CLIProxyAPI 中无痛使用 [OpenCode Zen](https://opencode.ai/zen) 的免费系列模型（如 `mimo-v2.6-flash-free`、`muse-spark-1.3-contributor-free` 等），无需修改或重新编译 CPA 宿主二进制。

---

## 解决的问题

1. **端点路由自动分流（Endpoint Split）**：
   - OpenCode Zen 的部分模型（如 `muse-spark`）只响应 `/responses` 端点，而其余大部分免费模型响应 `/chat/completions`。
   - 插件内置智能分流规则：识别 `muse` 自动走 `/responses`，其他模型自动走 `/chat/completions`，无需手动在配置中区分。

2. **免费层门禁自动伪装（FreeTier Gate）**：
   - Zen 服务端对未通过官方 CLI 发起的请求有严格的校验（检查 `ses_` / `msg_` 规范格式会话头、客户端特征头 `cli`、`bash`/`read` 工具集、强制 `stream: true` 等），缺一不可，否则返回 `403 FreeTierError` 并封禁 Key 约 15 分钟。
   - 插件全自动伪装为官方 CLI 规范，确保百分百顺利通行。

3. **SSE 流式传输重拼与保活过滤（Stream Reassembly）**：
   - 修复上游分片造成的跨包 SSE 截断问题，确保下游收到的每个 `data: {...}` 均为完整合法的 JSON 对象。
   - 自动过滤上游的 `: keep-alive` 心跳注释帧，避免 LobeHub、NextChat 等客户端因非法 JSON 报错崩溃。

4. **无感集成，原生提供商体验**：
   - 配置完全脱离插件管理面板，用户只需在凭证目录或 Web 认证面板中添加一个标准的 `zen` 认证文件即可，服务地址与 API Key 支持动态感知生效。

---

## 安装方法

从 [Releases](https://github.com/Victor9578/cpa-plugin-opencodezen/releases) 下载适用于你系统架构的编译包（例如 `zen_0.3.0_linux_amd64.zip`）。

解压后将 `zen.so`（或 `zen.dylib` / `zen.dll`）放入 CPA 的插件目录（文件名必须为 `zen.so` / `zen.dylib` / `zen.dll`）：

```bash
/CLIProxyAPI/plugins/linux/amd64/zen.so
```

---

## 使用指南

### 1. 启用插件（`config.yaml`）

在 `config.yaml` 中启用插件即可：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    zen:
      enabled: true
```

### 2. 添加凭证（认证目录）

在 CPA 的 `auth-dir`（例如 `/root/.cli-proxy-api/`）目录下创建一个 JSON 文件（例如 `zen-key.json`），或通过 Web 管理中心添加：

```json
{
  "type": "zen",
  "provider": "zen",
  "api_key": "sk-your-zen-api-key-here",
  "base_url": "https://opencode.ai/zen/v1"
}
```

> **说明**：
> - 插件默认已注册以下免费模型：`mimo-v2.6-flash-free`、`mimo-v2.5-free`、`ling-3.0-flash-fin-free`、`nemotron-3-ultra-free`、`muse-spark-1.3-contributor-free`。
> - 如果官方上线了新模型，可以在该 JSON 文件中添加 `"models": [{"name": "新模型名"}]` 即可动态生效，无需重新编译插件。

---

## 验证与测试

```bash
# 重启 CPA
docker restart cli-proxy-api

# 测试聊天
curl -s http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $CPA_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"mimo-v2.6-flash-free","stream":true,"messages":[{"role":"user","content":"Hello"}]}'
```

---

## 致谢与参考

- [cpa-plugin-opencode-session-mapper](https://github.com/ahoo/cpa-plugin-opencode-session-mapper) - 会话头映射插件的先行者，为会话 ID 规范化处理提供了思路。
- [OpenCode2API](https://github.com/TiaraBasori/OpenCode2API) - 早期的 OpenCode 接入探索，为 Zen 免费层伪装提供了宝贵参考。

---

## License

[MIT](LICENSE)
