# Changelog

本项目所有对外可感知的变化都记录在这里。格式参考 [Keep a Changelog](https://keepachangelog.com/)，版本号遵循语义化版本。

## [1.0.0] - 2026-11-25

首个正式版本。基于上游 `linguo2625469/workbuddy2api-panel @ 73a7560` 的个人增强分支（二开）。

### 新增

- **跨平台**：`Makefile`（`build` / `cross` / `test` / `vet` / `run` / `clean`）产出 Windows / Linux / macOS（amd64 + arm64）单文件二进制；macOS / Linux 控制台 `scripts/wb2api.sh`（命令与 Windows 版 `wb2api.ps1` 对齐）
- **`/v1/responses`**：Responses ↔ Chat 双向转换（含 reasoning items），DSH 等新协议客户端可直连
- **账号 JSON 导入导出**：六种常见导出形态兼容（数组 / 信封 / 字典 / 两段式…），写入前 dry-run 预览（新增/覆盖/跳过计数），批内 uid 去重，导出文件可直接回灌；`?redact=1` 导出不含凭据版
- **试聊「指定账号」**：`X-WB2API-Account` 头钉死单号发请求——忽略冷却、不轮换，失败返回该号自己的上游错误；面板下拉可选
- **思考档位下限**：`upstream.min_effort`（只抬不降，模型不支持时退到其最高档）；试聊页可选 `low → max`
- **视觉模态声明**：按网关 `supports_images` 给 DSH provider 写 `input: [text, image]`，修复"贴图无反应"
- **上游错误近因聚合**：`internal/errmon` 有界环形缓冲 + 按码聚合；面板顶部红标，点击跳日志；`GET /panel/api/error_summary?hours=N`
- **客户端身份对齐**：`x-stainless-*` / `b3` / `traceparent` / `baggage` / `acp-connection-id` 按真实客户端报文补齐；DSH 提示词指纹逐句改写
- **诊断**：`doctor` 十项体检（配置/进程/端口/健康/账号/凭证目录/磁盘/上游连通/近 24h 错误/模型锁池）；`smoke [模型]` 一键自检
- **运维脚本**：`wb2api.ps1`（Windows）与 `scripts/wb2api.sh`（macOS/Linux）统一控制台；DSH 与 WorkBuddy 桌面应用模型清单同步脚本

### 变更

- 队列码 `6020` / `6021` 归为「上游忙」（`ErrUpstreamBusy`）：**不再触发账号冷却**；真限流（`6004`）冷却时长可配（默认 5 分钟）
- 日志改为**进程内按大小轮转**（`logging.file` / `max_mb` / `keep`），不再依赖启动脚本重定向
- 版本号重置为 `1.0.0`（可用 `-X main.appVersion=...` 覆盖）

### 移除

- 中转融合渠道（`upstreams` 多上游池）——按需求裁剪
- 免费档模型默认屏蔽（`models.hidden`：`hy3` / `hy4free` / `hy4flash-free` / `hy4-preview-f`）
