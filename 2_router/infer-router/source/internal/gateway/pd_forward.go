package gateway

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/yzx/rl-router/internal/domain"
	"github.com/yzx/rl-router/internal/scheduler/policy"
	"github.com/yzx/rl-router/pkg/logger"
	"go.uber.org/zap"
)

// buildDisaggregateMutator returns a body mutator that injects the
// disaggregate_info object built from the (prefill, decode) instance pair into
// a parsed JSON request map. Used by the splitwise handler to keep PD body
// transformation logic in one place.
func buildDisaggregateMutator(prefill, decode *domain.Instance) func(map[string]any) (map[string]any, error) {
	return func(rawReq map[string]any) (map[string]any, error) {
		disagg, err := policy.BuildDisaggregateInfo(prefill, decode)
		if err != nil {
			return nil, err
		}
		rawReq["disaggregate_info"] = disagg
		return rawReq, nil
	}
}

// closeResponse closes a response body without draining it. Error and cancel
// paths must not wait for EOF because PD backends often return long-lived SSE
// streams; waiting would block cleanup until the backend finishes streaming.
func closeResponse(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_ = resp.Body.Close()
}

func closeResponseFrom(ch <-chan respResult) {
	res := <-ch
	closeResponse(res.resp)
}

type pdForwardRequest struct {
	Context         context.Context
	Client          *http.Client
	Log             *zap.Logger
	Headers         http.Header
	DecodeEndpoint  string
	PrefillEndpoint string
	Body            []byte
	Stream          bool
	OnPrefillDone   func()
}

type pdForwardResult struct {
	DecodeResp *http.Response
}

type respResult struct {
	resp *http.Response
	err  error
}

// PostToPD sends requests concurrently to both Prefill and Decode instances.
// Only the Decode response is returned to the caller. The Prefill response is
// consumed asynchronously by readPrefillRecv.
func PostToPD(req pdForwardRequest) (pdForwardResult, error) {
	ctx := req.Context
	if ctx == nil {
		ctx = context.Background()
	}
	client := req.Client
	if client == nil {
		client = http.DefaultClient
	}
	log := req.Log
	if log == nil {
		log = zap.NewNop()
	}
	decodeEndpoint := pdChatCompletionsURL(req.DecodeEndpoint)
	prefillEndpoint := pdChatCompletionsURL(req.PrefillEndpoint)

	// Construct two requests.
	decodeReq, err := http.NewRequestWithContext(ctx, "POST", decodeEndpoint, bytes.NewReader(req.Body))
	if err != nil {
		return pdForwardResult{}, fmt.Errorf("failed to create decode request: %w", err)
	}
	prefillReq, err := http.NewRequestWithContext(ctx, "POST", prefillEndpoint, bytes.NewReader(req.Body))
	if err != nil {
		return pdForwardResult{}, fmt.Errorf("failed to create prefill request: %w", err)
	}

	// Copy request headers.
	for k, v := range req.Headers {
		if k != "Content-Length" {
			decodeReq.Header[k] = v
			prefillReq.Header[k] = v
		}
	}
	decodeReq.Header.Set("Content-Type", "application/json")
	prefillReq.Header.Set("Content-Type", "application/json")

	prefillCh := make(chan respResult, 1)
	decodeCh := make(chan respResult, 1)

	// Concurrently send requests to P/D using shared connection pool.
	go func() {
		resp, err := client.Do(prefillReq)
		prefillCh <- respResult{resp: resp, err: err}
	}()
	go func() {
		resp, err := client.Do(decodeReq)
		decodeCh <- respResult{resp: resp, err: err}
	}()

	log.Info("v2-splitwise-central: PostToPD connecting",
		logger.Status(logger.StatusOK),
		zap.String("decode_endpoint", decodeEndpoint),
		zap.String("prefill_endpoint", prefillEndpoint),
	)

	// Wait for both responses with context cancellation protection.
	// On ctx cancel, drain any in-flight response in a detached goroutine to
	// prevent connection/FD leaks (the goroutines themselves cannot leak —
	// channels are buffered — but their Response.Body must be closed).
	var prefillRes, decodeRes respResult
	select {
	case prefillRes = <-prefillCh:
	case <-ctx.Done():
		go closeResponseFrom(prefillCh)
		go closeResponseFrom(decodeCh)
		return pdForwardResult{}, ctx.Err()
	}
	select {
	case decodeRes = <-decodeCh:
	case <-ctx.Done():
		closeResponse(prefillRes.resp)
		go closeResponseFrom(decodeCh)
		return pdForwardResult{}, ctx.Err()
	}

	// Prioritize returning Decode errors.
	if decodeRes.err != nil {
		closeResponse(prefillRes.resp)
		return pdForwardResult{}, fmt.Errorf("decode request failed: %w", decodeRes.err)
	}
	if prefillRes.err != nil {
		log.Warn("prefill request failed, closing decode response",
			zap.Error(prefillRes.err))
		closeResponse(decodeRes.resp)
		return pdForwardResult{}, fmt.Errorf("prefill request failed: %w", prefillRes.err)
	}

	log.Info("v2-splitwise-central: PostToPD success",
		logger.Event(logger.EventProxyPDForward),
		logger.Status(logger.StatusOK),
		zap.String("decode_endpoint", decodeEndpoint),
		zap.String("prefill_endpoint", prefillEndpoint),
	)

	// Start async consumption of Prefill response.
	if prefillRes.resp != nil {
		go readPrefillRecv(log, req.Stream, prefillRes.resp, req.OnPrefillDone)
	}

	return pdForwardResult{DecodeResp: decodeRes.resp}, nil
}

func pdChatCompletionsURL(endpoint string) string {
	if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
		return strings.TrimRight(endpoint, "/") + "/v1/chat/completions"
	}
	return "http://" + strings.TrimRight(endpoint, "/") + "/v1/chat/completions"
}

// readPrefillRecv asynchronously reads and discards the Prefill response.
// For streaming: releases counters as soon as the first byte arrives.
// For non-streaming: releases counters after the full body is read.
//
// Memory profile: this path used to allocate a 10MB scanner buffer per request
// (≈10MB × 1w concurrent = 100GB). We just need to (1) detect the first byte
// to release the counter, then (2) drain the rest with io.Copy. No buffering
// required — io.Copy uses a fixed 32KB internal buffer.
func readPrefillRecv(log *zap.Logger, isStream bool,
	backendResp *http.Response, releaseFunc func(),
) {
	released := false
	doRelease := func(reason string) {
		if released || releaseFunc == nil {
			return
		}
		releaseFunc()
		released = true
		log.Info("prefill release",
			zap.String("reason", reason),
			zap.Bool("is_stream", isStream))
	}
	defer func() {
		if !released && releaseFunc != nil {
			releaseFunc()
			log.Info("prefill release in defer (fallback)",
				zap.Bool("is_stream", isStream))
		}
	}()

	if backendResp == nil || backendResp.Body == nil {
		log.Info("prefill backendResp is nil or body is nil")
		return
	}
	defer backendResp.Body.Close()

	if isStream {
		// Detect first byte: read 1 byte to confirm SSE stream began, then
		// release the counter and drain the remainder.
		var probe [1]byte
		n, err := io.ReadFull(backendResp.Body, probe[:])
		if n > 0 {
			doRelease("first_chunk_received")
		}
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			log.Warn("prefill first-byte read error", zap.Error(err))
			return
		}
		if _, err := io.Copy(io.Discard, backendResp.Body); err != nil {
			log.Warn("prefill drain error", zap.Error(err))
		}
	} else {
		if _, err := io.Copy(io.Discard, backendResp.Body); err != nil {
			log.Warn("prefill copy error", zap.Error(err))
		}
		doRelease("non_stream_done")
		log.Info("prefill finish", zap.Bool("is_stream", isStream))
	}
}
