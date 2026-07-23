# RC 排查 Case 汇总 — OneRouter 对照分析

> **目的**：汇总 rollout-controller（RC）线上排查遇到的典型故障 case，逐一说明 OneRouter 为何不会复现。
> 本文档持续更新，每遇到新 case 按模板追加即可。

---

## 目录

| # | Case | 关键词 | RC 根因 | OneRouter 状态 |
|---|------|--------|---------|---------------|
| 1 | [上游消费过慢，网关提前关闭连接](#case-1-上游消费过慢网关提前关闭连接) | SSE / TCP RST / BrokenResourceError | 自定义 goroutine 回收连接 | ✅ 已规避 |

---

## Case 1: 上游消费过慢，网关提前关闭连接

### 现象

经全链路日志排查，RC 本身并未报错，所有 8 个 gen (0–7) 均返回 `status=200`。问题仅出现在 **RC → Python 客户端** 的下游连接上，且只影响了 `gen_id=6`：

| 指标     | RC 侧                  | Python 侧                        |
| -------- | ---------------------- | --------------------------------- |
| 完成时间 | 19:17:31               | 19:23:32 (ReadError)              |
| token 数 | 24361                  | 22646 chunks                      |
| 状态     | 成功 (status=200)      | 失败 (BrokenResourceError)        |

时间差约 6 分钟，缺失约 1715 个 chunk。

### 根因

RC 的 Go 代码在 `redirect.go:310-327` 中，收到 `[DONE]` 后执行 `w.Flush()` 然后直接 `return`。`Flush()` 只是将数据写入 OS 内核的 TCP send buffer，并不保证客户端已读取完毕。Go goroutine 退出后连接进入回收流程（**RST 关闭**），而 Python 侧消费速度跟不上（每 gen 约 24000 tokens / 47MB SSE 流，gen_id=6 消费速度仅 ~26 chunk/s），在 buffer 被 drain 完之前连接已被强制关闭 → TCP RST → `httpx.ReadError(BrokenResourceError())`。

核心问题：**自定义 goroutine 管理连接生命周期，goroutine 退出即回收连接，未等待客户端消费完成。**

### 旧版建议

- **P0**: 开启 `use_replay_buffer=True`，让 broken group 可重试
- **P1**: RC 侧在 `handleScan()` return 前增加下游连接 drain 等待逻辑
- **P1**: 排查 Python 侧 streaming 消费瓶颈（gen_id=6 消费速度仅 ~26 chunk/s）

### OneRouter 为何不会复现

**1. 连接生命周期由 `net/http` Server 统一管理，而非自定义 goroutine 回收**

旧版 RC 使用自定义 goroutine 管理连接，goroutine `return` 后连接即被回收（TCP RST）。OneRouter 中 handler 返回后，连接由标准库 `net/http` 接管：Keep-Alive 连接保持在连接池中不关闭；非 Keep-Alive 连接发送 TCP FIN（优雅关闭），客户端仍可读取缓冲区中的剩余数据。

**2. 两条转发路径均保证数据完整传输后才返回**

- **V1 路径**（`httputil.ReverseProxy`）：内部通过 `io.Copy` 将后端 body 完整传输到 ResponseWriter，直到后端 EOF 才返回，且设置 `FlushInterval: -1` 保证逐块 Flush。
- **V2 ACK 路径**（`streamForward` 手动 read-write 循环）：逐块 `Read` → `Write` → `Flush`，直到后端返回 `io.EOF` 才 `return`，所有数据在函数返回前已写入 ResponseWriter。

**3. HTTP Server 未设置 WriteTimeout**

`http.Server` 未配置 `WriteTimeout`，长时间运行的 SSE 连接不会被超时机制截断。

**4. 对比总结**

| 维度               | 旧版 RC                                      | OneRouter                                 |
| ------------------ | -------------------------------------------- | ----------------------------------------- |
| 连接管理           | 自定义 goroutine，手动回收                    | `net/http` Server 统一管理                |
| 流结束行为         | `Flush()` + `return` → goroutine 退出 → RST  | handler 返回 → Keep-Alive 或 FIN 优雅关闭 |
| 客户端能否读完缓冲 | ❌ 连接被 RST 截断                            | ✅ FIN 允许客户端完成 drain                |

> **唯一边界情况**：服务端主动关停（Shutdown）时，若 `ShutdownGrace` 窗口内仍有超长 SSE 流未传输完，超时后 `Close()` 会强制关闭所有连接。此场景可通过调大 `shutdown_grace` 配置缓解，但属于运维层面而非代码缺陷。

---

<!--
## Case N: <标题>

### 现象
<线上表现、错误信息、影响范围>

### 根因
<RC 代码层面的根本原因分析>

### 旧版建议
<当时给 RC 的修复建议>

### OneRouter 为何不会复现
<从架构/代码层面说明 OneRouter 如何规避，附对比表格>

---
-->
