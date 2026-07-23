package gateway

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"

	"golang.org/x/net/http2"
)

// proxyErrKey is a context key for capturing proxy transport errors.
// Each request gets its own *error pointer, so no concurrency issues.
type proxyErrKey struct{}

// Proxy defines the interface for forwarding requests to a backend instance.
type Proxy interface {
	Forward(w http.ResponseWriter, r *http.Request, targetEndpoint string) error
}

// sharedHTTP1Transport is the default HTTP/1.1 transport for backend connections,
// used when no custom idleConnTimeout is provided. Tests and legacy code paths
// that don't call NewReverseProxyWithIdleTimeout fall back to this transport.
var sharedHTTP1Transport = newBackendTransport(90 * time.Second)

// newBackendTransport creates an HTTP/1.1 transport for backend connections.
// idleConnTimeout controls how long idle connections stay in the pool before
// being closed. It should be less than the backend's keepalive timeout to
// avoid stale connections (e.g., 3s for uvicorn with default keepalive=5s).
func newBackendTransport(idleConnTimeout time.Duration) *http.Transport {
	return &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConnsPerHost: 100,
		MaxIdleConns:        1000,
		IdleConnTimeout:     idleConnTimeout,
	}
}

// proxyErrorHandler is a package-level function shared by all cached proxies,
// avoiding a closure allocation per proxy instance.
// It captures the transport error via the request context so Forward() can return it.
func proxyErrorHandler(w http.ResponseWriter, r *http.Request, err error) {
	if errPtr, ok := r.Context().Value(proxyErrKey{}).(*error); ok {
		*errPtr = err
	}
	http.Error(w, "proxy error: "+err.Error(), http.StatusBadGateway)
}

// proxyBufPool pools 32KB transfer buffers used by httputil.ReverseProxy.
// Without this, each concurrent SSE connection allocates a 32KB buffer internally,
// leading to ~320MB pinned memory at 10K concurrent connections.
type proxyBufPool struct {
	pool sync.Pool
}

func (p *proxyBufPool) Get() []byte {
	if v := p.pool.Get(); v != nil {
		return v.([]byte)
	}
	return make([]byte, 32*1024)
}

func (p *proxyBufPool) Put(buf []byte) {
	p.pool.Put(buf) //nolint:staticcheck // buf is fixed-size, safe to pool
}

var sharedBufPool = &proxyBufPool{}

// forwardViaCache implements the 3-tier cache lookup and proxy creation pattern:
//  1. Fast path: direct sync.Map lookup with the original endpoint key (zero-alloc).
//  2. Normalized-key lookup: prepend "http://" if missing, check again.
//  3. Create: url.Parse + new httputil.ReverseProxy, cache under both keys.
//
// transport may be nil, in which case httputil.ReverseProxy uses http.DefaultTransport.
func forwardViaCache(
	proxies *sync.Map,
	transport http.RoundTripper,
	w http.ResponseWriter,
	r *http.Request,
	targetEndpoint string,
) error {
	// Make POST/PUT/PATCH bodies replayable so Go's Transport can
	// automatically retry on stale pool connections (needs GetBody).
	// Inference request bodies are JSON prompts (typically < 10KB).
	if r.Body != nil && r.Body != http.NoBody && r.GetBody == nil {
		bodyBytes, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err != nil {
			return fmt.Errorf("buffer request body for replay: %w", err)
		}
		replayableBody(r, bodyBytes)
	}

	// Inject a per-request error slot so proxyErrorHandler can propagate
	// transport errors back to the caller. The *error is request-scoped,
	// so concurrent requests each get their own pointer — no data race.
	var capturedErr error
	r = r.WithContext(context.WithValue(r.Context(), proxyErrKey{}, &capturedErr))

	// Fast path: try direct lookup with original endpoint string first.
	// After the first request, schemeless endpoints are also cached under
	// their original key, avoiding the "http://" + key string allocation.
	if val, ok := proxies.Load(targetEndpoint); ok {
		val.(*httputil.ReverseProxy).ServeHTTP(w, r)
		return capturedErr
	}

	// Slow path: normalize scheme, parse URL, create proxy (first request per host).
	key := targetEndpoint
	if !hasScheme(key) {
		key = "http://" + key
	}

	// Check if another goroutine already cached under the normalized key.
	if val, ok := proxies.Load(key); ok {
		// Also cache under the original schemeless key for future fast-path hits.
		if key != targetEndpoint {
			proxies.Store(targetEndpoint, val)
		}
		val.(*httputil.ReverseProxy).ServeHTTP(w, r)
		return capturedErr
	}

	target, err := url.Parse(key)
	if err != nil {
		return fmt.Errorf("parse target: %w", err)
	}

	rp := &httputil.ReverseProxy{
		Transport: transport,
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
		},
		BufferPool:    sharedBufPool,
		ErrorHandler:  proxyErrorHandler,
		FlushInterval: -1,
	}

	actual, _ := proxies.LoadOrStore(key, rp)
	// Cache under original schemeless key for zero-alloc fast path.
	if key != targetEndpoint {
		proxies.Store(targetEndpoint, actual)
	}
	actual.(*httputil.ReverseProxy).ServeHTTP(w, r)
	return capturedErr
}

// ReverseProxy implements Proxy using Go's httputil.ReverseProxy.
// Proxy instances are cached per host to avoid per-request allocations
// of httputil.ReverseProxy, Director closures, url.Parse, and string concat.
type ReverseProxy struct {
	transport *http.Transport
	proxies   sync.Map // string → *httputil.ReverseProxy
}

// NewReverseProxy creates a ReverseProxy with the default (legacy) transport.
// Prefer NewReverseProxyWithIdleTimeout for production use.
func NewReverseProxy() *ReverseProxy {
	return &ReverseProxy{transport: sharedHTTP1Transport}
}

// NewReverseProxyWithIdleTimeout creates a ReverseProxy whose backend connection
// pool uses the given idle timeout. Set this to less than the backend's keepalive
// timeout to avoid fetching stale connections from the pool.
func NewReverseProxyWithIdleTimeout(idleConnTimeout time.Duration) *ReverseProxy {
	return &ReverseProxy{transport: newBackendTransport(idleConnTimeout)}
}

func (p *ReverseProxy) Forward(w http.ResponseWriter, r *http.Request, targetEndpoint string) error {
	return forwardViaCache(&p.proxies, p.transport, w, r, targetEndpoint)
}

// DrainBody reads and discards the body so the connection can be reused.
func DrainBody(body io.ReadCloser) {
	if body != nil {
		_, _ = io.Copy(io.Discard, body)
		_ = body.Close()
	}
}

// hasScheme reports whether endpoint starts with "http://" or "https://".
// Case-sensitive only (uppercase schemes return false).
// The len > 7 guard serves two purposes:
//  1. A bare "http://" (len 7) is not a valid endpoint — reject it.
//  2. Prevents out-of-bounds on the endpoint[:8] check for "https://".
func hasScheme(endpoint string) bool {
	return len(endpoint) > 7 && (endpoint[:7] == "http://" || endpoint[:8] == "https://")
}

// H2CReverseProxy implements Proxy using HTTP/2 cleartext (h2c) transport.
// This enables multiplexed streams over a single TCP connection to the backend.
// Proxy instances are cached per host, same as ReverseProxy.
type H2CReverseProxy struct {
	transport *http2.Transport
	proxies   sync.Map // string → *httputil.ReverseProxy
}

// NewH2CReverseProxy creates a proxy that connects to backends via h2c.
func NewH2CReverseProxy() *H2CReverseProxy {
	return &H2CReverseProxy{
		transport: &http2.Transport{
			AllowHTTP: true,
			DialTLS: func(network, addr string, _ *tls.Config) (net.Conn, error) {
				return net.DialTimeout(network, addr, 10*time.Second)
			},
		},
	}
}

func (p *H2CReverseProxy) Forward(w http.ResponseWriter, r *http.Request, targetEndpoint string) error {
	return forwardViaCache(&p.proxies, p.transport, w, r, targetEndpoint)
}
