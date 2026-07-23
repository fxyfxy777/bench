结论：RC 可以在响应阶段做纯转发
RedirectComplete 只需要知道"请求结束了"即可释放负载（Redis 计数 -1），不需要解析 chunk 内容。实际上代码中已经有一个纯转发路径—— redirect() 函数（redirect.go:660-685），当路由策略是 StrategyRoundRobin 时就走纯透传。

必要处理阶段及伪代码
阶段 1：请求解析（必须）
对应 inference.go:29-89

Go

插入

复制
func ChatCompletionsHandler(ctx, w, req) {
    // 1.1 检查服务就绪
    if !service.IsReadyForInference() {
        return 400, "not ready"
    }

    // 1.2 读取 body
    bodyBytes = io.ReadAll(req.Body)

    // 1.3 结构化解析（提取路由决策需要的字段）
    var request ChatRequest
    json.Unmarshal(bodyBytes, &request)
    // 需要的字段: stream, session_id, need_ack, instance_info, model

    // 1.4 保留原始 body 用于透传（保证 extra_body 等 PaddleRL 扩展字段不丢失）
    var rawReq map[string]any
    json.Unmarshal(bodyBytes, &rawReq)
    request.RawReq = rawReq
}
为什么必须：RC 需要从请求中提取 session_id（亲和性路由）、instance_info（指定实例）、stream（流式/非流式分支）、need_ack（ACK 行为）。双重解析是为了确保 PaddleRL 传的 extra_body 中所有扩展字段在透传时不丢失。

阶段 2：选择后端实例（必须）
对应 redirect.go:118-128 + select_bucket.go

Go

插入

复制
func selectInstance(ctx, jobID, request) (*InstanceInfo, error) {
    // 2.1 如果请求自带 instance_info，直接使用
    if request.InstanceInfo != nil {
        return convertToInstanceInfo(request.InstanceInfo), nil
    }

    // 2.2 否则通过调度器选择实例
    //     支持 session 亲和性（同一 session_id 路由到同一实例）
    //     支持 round-robin / bucket-balance 策略
    instance, err = scheduler.SelectInstance(ctx, jobID, request)
    return instance, err
}
为什么必须：RC 的核心价值就是路由和负载均衡。

阶段 3：发送 ACK（必须）
对应 redirect.go:134-153

Go

插入

复制
// 3. 如果 need_ack=true，在转发到后端之前立即返回 ACK
if request.NeedACK || request.ExtraBody.NeedACK {
    w.Header.Set("Content-Type", "text/event-stream")
    w.WriteHeader(200)

    ack = {
        "id":               "ack",
        "object":           "chat.completion.chunk",
        "created":          time.Now().Unix(),
        "choices":          [],
        "model":            request.Model,
        "is_ack_response":  true
    }
    w.Write("data: " + json.Marshal(ack) + "\n\n")
    w.Flush()
}
为什么必须：PaddleRL inference.py:386 显式检查 is_ack_response 并跳过。如果不发 ACK，PaddleRL 侧超时机制可能误判请求未被接收。

阶段 4：转发请求到后端（必须）
对应 select_bucket.go:47-84

Go

插入

复制
// 4.1 构造后端请求
bodyBytes = json.Marshal(request.RawReq)  // 用 RawReq 保证全量透传
backendURL = fmt.Sprintf("http://%s:%s/v1/chat/completions",
                         instance.Host, instance.InferPort)
backendReq = http.NewRequest("POST", backendURL, bodyBytes)

// 4.2 透传请求头（移除 Content-Length）
for k, v in req.Header {
    if k != "Content-Length" {
        backendReq.Header[k] = v
    }
}

// 4.3 发送请求（带重试）
backendResp, err = httpClient.Do(backendReq)
为什么必须：RC 作为代理必须转发请求。

阶段 5：转发响应头（必须）
对应 redirect.go:188-200

Go

插入

复制
// 5. 复制后端响应头，新增 Instance-ID
for k, v in backendResp.Header {
    if k != "Content-Length" {
        w.Header[k] = v
    }
}
w.Header.Set("Instance-ID", instance.ID)
w.WriteHeader(backendResp.StatusCode)
为什么必须：流式场景必须移除 Content-Length，Instance-ID 用于链路追踪。

阶段 6：流式响应转发（可以纯转发）
这是核心问题。当前代码有两条路径：

路径	函数	行为
纯转发	redirect() :660	不解析 chunk，直接 Read → Write → Flush
续推	handleScan() :267	解析每个 chunk，提取 token IDs，缓存 stop 包，触发续推
去掉续推后，可以统一走纯转发路径，但需要增加一个轻量处理：检测 [DONE] 来释放实例负载。

Go

插入

复制
// 阶段 6：流式响应转发（最小化版本）
func streamForward(ctx, w, backendResp, jobID, instance, sessionID, bucketSize) {
    defer backendResp.Body.Close()

    scanner = bufio.NewScanner(backendResp.Body)
    scanner.Buffer(make([]byte, 10*1024*1024), 10*1024*1024)

    for scanner.Scan() {
        line = scanner.Text()

        // 空行跳过
        if len(line) == 0 {
            continue
        }

        // 非 data: 前缀行
        if !strings.HasPrefix(line, "data: ") {
            // 如果包含 error，透传后释放负载
            if strings.Contains(line, "error") {
                w.Write(line + "\n\n")
                w.Flush()
                service.RedirectComplete(ctx, jobID, bucketSize, instance.ID, sessionID, 0)
                return
            }
            continue
        }

        jsonData = strings.TrimPrefix(line, "data: ")

        // [DONE] → 透传并释放负载
        if jsonData == "[DONE]" {
            w.Write(line + "\n\n")
            w.Flush()
            service.RedirectComplete(ctx, jobID, bucketSize, instance.ID, sessionID, 0)
            return
        }

        // 普通 chunk → 直接透传，不解析
        w.Write(line + "\n\n")
        w.Flush()
    }

    // scanner 错误 → 补发 [DONE]，释放负载
    if err := scanner.Err(); err != nil {
        w.Write("data: [DONE]\n\n")
        w.Flush()
        service.RedirectComplete(ctx, jobID, bucketSize, instance.ID, sessionID, 0)
    }
}
与当前 handleScan 的区别：

当前 handleScan	最小化版本
json.Unmarshal 每个 chunk → ChatChunk	不解析 chunk JSON
提取 CompletionTokenIDs	不提取
提取 finish_reason，缓存 stop 包	不缓存，直接透传
recover_stop → 触发续推	不续推
续推首包跳过	不需要
累计 tokenIDs	不累计
RedirectComplete(len(tokenIDs))	RedirectComplete(0) — totalLen 只做统计，传 0 不影响负载释放
完整最小化处理流程伪代码
Go

插入

复制
// ==================== 完整处理流程 ====================
func ChatCompletionsHandler(ctx, w, req) {
    // ========== 阶段 1: 请求解析 ==========
    if !service.IsReadyForInference() {
        return 400
    }
    bodyBytes = io.ReadAll(req.Body)

    var request ChatRequest
    json.Unmarshal(bodyBytes, &request)     // 结构化：提取 stream/session_id/need_ack/instance_info/model

    var rawReq map[string]any
    json.Unmarshal(bodyBytes, &rawReq)      // 原始：保留全量字段用于透传
    request.RawReq = rawReq

    // ========== 阶段 2: 选择后端实例 ==========
    instance, bucketSize, err = selectInstance(ctx, jobID, request)
    if err != nil {
        return 500
    }

    // ========== 阶段 3: 发送 ACK ==========
    if request.NeedACK {
        w.Header.Set("Content-Type", "text/event-stream")
        w.WriteHeader(200)
        ack = {"id":"ack", "object":"chat.completion.chunk",
               "created": now(), "choices":[], "model": request.Model,
               "is_ack_response": true}
        w.Write("data: " + json.Marshal(ack) + "\n\n")
        w.Flush()
    }

    // ========== 阶段 4: 转发请求到后端 ==========
    bodyBytes = json.Marshal(request.RawReq)
    backendReq = http.NewRequest("POST", backendURL, bodyBytes)
    // 透传 Header（去除 Content-Length）
    for k, v in req.Header {
        if k != "Content-Length" {
            backendReq.Header[k] = v
        }
    }
    backendResp = httpClient.Do(backendReq)   // 带重试

    // ========== 阶段 5: 转发响应头 ==========
    for k, v in backendResp.Header {
        if k != "Content-Length" {
            w.Header[k] = v
        }
    }
    w.Header.Set("Instance-ID", instance.ID)
    w.WriteHeader(backendResp.StatusCode)

    // ========== 阶段 6: 流式响应纯转发 ==========
    if request.Stream {
        scanner = bufio.NewScanner(backendResp.Body)
        for scanner.Scan() {
            line = scanner.Text()
            if len(line) == 0 { continue }

            if !strings.HasPrefix(line, "data: ") {
                if strings.Contains(line, "error") {
                    w.Write(line + "\n\n"); w.Flush()
                    service.RedirectComplete(ctx, jobID, bucketSize, instance.ID, sessionID, 0)
                    return
                }
                continue
            }

            // ★ 唯一需要检测的：[DONE] → 释放实例负载
            if strings.TrimPrefix(line, "data: ") == "[DONE]" {
                w.Write(line + "\n\n"); w.Flush()
                service.RedirectComplete(ctx, jobID, bucketSize, instance.ID, sessionID, 0)
                return
            }

            // ★ 其余 chunk 全部原样透传，不解析 JSON
            w.Write(line + "\n\n"); w.Flush()
        }

        // scanner 断连兜底
        if scanner.Err() != nil {
            w.Write("data: [DONE]\n\n"); w.Flush()
            service.RedirectComplete(ctx, jobID, bucketSize, instance.ID, sessionID, 0)
        }
    } else {
        io.Copy(w, backendResp.Body)
    }
}