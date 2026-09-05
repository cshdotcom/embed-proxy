# embed-proxy

SiliconFlow Embedding 清洗代理。解决 OpenWebUI / LiteLLM 调用硅基流动 `/v1/embeddings` 时因注入 `"encoding_format": null` 导致返回 `20015 参数非法` 的问题。

## 功能

- **encoding_format 清洗**：`POST /v1/embeddings` 自动删除 `encoding_format` 字段后转发上游
- **全局 API Key 鉴权**：请求必须携带 `Authorization: Bearer <proxy_auth_key>`，防止公网暴露被滥用
- **自定义路由映射**：任意路径改写，内置示例 `/v1/audio/transcriptions -> /v1/chat/completions`
- **自定义上游地址**：默认硅基流动 `https://api.siliconflow.cn/v1`，可改为任意 OpenAI 兼容 API
- 默认监听 `0.0.0.0:16540`，可被 gost / frp / cloudflared 等内网穿透工具直接转发
- 启动弹出交互菜单：一键安装 systemd 服务（开机自启）、卸载、改端口、设 Key、改上游、管理路由
- 重装/升级服务时**自动合并保留已有配置**（API Key / 端口 / 上游 / 鉴权 Key / 路由不丢失）
- 单文件静态二进制，无任何外部依赖

## 快速开始

```bash
chmod +x embed-proxy
sudo ./embed-proxy
```

菜单操作：

1. `4` 设置上游 API Key（硅基 sk-xxx）
2. `5` 设置代理鉴权 Key（首次运行自动生成随机 Key，banner 中可查看；客户端访问需携带）
3. `6` 修改上游 API 地址（默认硅基流动，回车恢复默认）
4. `7` 管理自定义路由映射（添加/删除）
5. `1` 安装为系统服务（开机自启动，自动保留已有配置）
6. `3` 修改监听端口（默认 16540）

## OpenWebUI 接入

管理员 → 设置 → 文档：

- 语义向量模型引擎：`OpenAI`
- 语义向量模型：`BAAI/bge-m3`
- 基础 URL：`http://<服务器IP>:16540/v1`
  （OpenWebUI 以 Docker 运行时，加 `--add-host=host.docker.internal:host-gateway`，填 `http://host.docker.internal:16540/v1`）
- **API Key：填代理鉴权 Key（`proxy_auth_key`）**，不是随便的字符串

## 配置文件

路径：`/etc/embed-proxy/config.json`（可用环境变量 `EMBED_PROXY_CONFIG` 覆盖）

```json
{
  "port": 16540,
  "siliconflow_api_key": "sk-xxx",
  "upstream_base": "https://api.siliconflow.cn/v1",
  "proxy_auth_key": "你的鉴权密钥",
  "route_mappings": [
    { "source": "/v1/audio/transcriptions", "target": "/v1/chat/completions" }
  ]
}
```

环境变量 `SILICONFLOW_API_KEY`、`EMBED_PROXY_PORT` 优先于配置文件。

## 测试

```bash
go test -v ./...
```

## License

MIT
