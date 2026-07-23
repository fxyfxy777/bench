package forensics

import (
	"context"
	"errors"
	"net"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// DisconnectSource identifies why a request ended abnormally.
type DisconnectSource string

const (
	DisconnectNone         DisconnectSource = "none"          // normal completion
	DisconnectClient       DisconnectSource = "client"        // client closed connection
	DisconnectBackend      DisconnectSource = "backend"       // backend error / disconnect
	DisconnectTimeout      DisconnectSource = "timeout"       // deadline exceeded
	DisconnectQueueFull    DisconnectSource = "queue_full"    // rate limiter queue full
	DisconnectQueueTimeout DisconnectSource = "queue_timeout" // rate limiter queue timeout
	DisconnectAllocFail    DisconnectSource = "alloc_fail"    // no available instances
)

// DisconnectPhase identifies which request lifecycle phase a disconnect occurred in.
// Used together with DisconnectSource to answer "who disconnected" + "at which stage".
type DisconnectPhase string

const (
	PhaseQueue    DisconnectPhase = "queue"    // disconnect during rate-limit queuing
	PhaseAllocate DisconnectPhase = "allocate" // disconnect waiting for instance allocation
	PhaseForward  DisconnectPhase = "forward"  // disconnect during backend connection establishment
	PhaseStream   DisconnectPhase = "stream"   // disconnect during SSE streaming
	PhaseResponse DisconnectPhase = "response" // disconnect waiting for non-stream response
)

// ClassifyProxyDisconnect determines the disconnect source for a streaming
// proxy request based on the proxy error, client context state, and whether
// the SSE stream completed normally (saw [DONE]).
//
// Decision tree:
//  1. No error + stream done → none (normal)
//  2. No error + stream not done → backend (abnormal EOF)
//  3. Client context cancelled → client disconnected
//  4. Deadline exceeded → timeout
//  5. Otherwise → backend error
func ClassifyProxyDisconnect(proxyErr error, clientCtx context.Context, streamDone bool) DisconnectSource {
	if proxyErr == nil {
		if streamDone {
			return DisconnectNone
		}
		// Stream ended without [DONE] and no proxy error — backend closed early.
		return DisconnectBackend
	}

	// Client context cancelled means the downstream client disconnected.
	if clientCtx.Err() != nil {
		if errors.Is(clientCtx.Err(), context.Canceled) {
			return DisconnectClient
		}
		return DisconnectTimeout
	}

	// Proxy error but client still connected — backend side issue.
	if errors.Is(proxyErr, context.DeadlineExceeded) {
		return DisconnectTimeout
	}

	return DisconnectBackend
}

// ClassifyTransportError determines the disconnect source for a non-streaming
// request that encountered a transport error (from http.Client.Do).
//
// Used in the non-stream retry loop to distinguish client disconnect from
// backend failure, which determines whether to retry.
func ClassifyTransportError(err error, clientCtx context.Context) DisconnectSource {
	if err == nil {
		return DisconnectNone
	}

	// Client initiated cancellation.
	if clientCtx.Err() != nil {
		if errors.Is(clientCtx.Err(), context.Canceled) {
			return DisconnectClient
		}
		return DisconnectTimeout
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return DisconnectTimeout
	}

	if errors.Is(err, context.Canceled) {
		return DisconnectClient
	}

	// Check for network-level errors (connection refused, reset, etc.).
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		return DisconnectBackend
	}

	return DisconnectBackend
}

// ClassifyAllocError determines the disconnect source when allocation fails.
// Distinguishes client cancellation, rate limiting, internal errors, and
// genuine "no available backend" conditions using gRPC status codes.
func ClassifyAllocError(err error, clientCtx context.Context) DisconnectSource {
	if err == nil {
		return DisconnectNone
	}

	if clientCtx.Err() != nil {
		if errors.Is(clientCtx.Err(), context.Canceled) {
			return DisconnectClient
		}
		return DisconnectTimeout
	}

	// Inspect gRPC status code for finer classification.
	if s, ok := status.FromError(err); ok {
		switch s.Code() {
		case codes.ResourceExhausted:
			return DisconnectQueueFull // scheduler rate limit
		case codes.Internal:
			return DisconnectBackend // internal error (e.g. marshaling failure)
		}
	}

	return DisconnectAllocFail
}
