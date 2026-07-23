package registry

import (
	"context"
	"maps"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/yzx/rl-router/internal/domain"
	"github.com/yzx/rl-router/pkg/logger"
	"github.com/yzx/rl-router/pkg/metrics"
)

// GatewayRegistry maintains the set of registered gateway nodes.
// Uses Copy-on-Write for the gateway map: writes clone the map under mutex,
// reads are lock-free via atomic pointer load.
// The gateway's advertise address is used as the unique key.
type GatewayRegistry struct {
	mu        sync.Mutex // protects write operations only (COW)
	gwPtr     atomic.Pointer[map[string]*domain.GatewayInfo]
	logger    *zap.Logger
	onExpired func(ctx context.Context, gatewayAddr string) // called when a gateway expires
}

func NewGatewayRegistry(logger *zap.Logger) *GatewayRegistry {
	r := &GatewayRegistry{
		logger: logger,
	}
	empty := make(map[string]*domain.GatewayInfo)
	r.gwPtr.Store(&empty)
	return r
}

// loadGateways returns the current gateway map (lock-free read).
func (r *GatewayRegistry) loadGateways() map[string]*domain.GatewayInfo {
	return *r.gwPtr.Load()
}

// cloneMap returns a shallow copy of the current gateway map.
// Must be called under r.mu.
func (r *GatewayRegistry) cloneMap() map[string]*domain.GatewayInfo {
	cur := r.loadGateways()
	newMap := make(map[string]*domain.GatewayInfo, len(cur)+1)
	maps.Copy(newMap, cur)
	return newMap
}

// SetOnExpired sets a callback that is invoked when a gateway expires.
// Must be called before StartExpireLoop.
func (r *GatewayRegistry) SetOnExpired(fn func(ctx context.Context, gatewayAddr string)) {
	r.onExpired = fn
}

// Register adds or replaces a gateway in the registry.
// Idempotent: the same address can register multiple times.
func (r *GatewayRegistry) Register(addr string, labels map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	cur := r.loadGateways()
	_, existed := cur[addr]

	newMap := r.cloneMap()
	newMap[addr] = &domain.GatewayInfo{
		GatewayAddr:   addr,
		Labels:        labels,
		LastHeartbeat: time.Now(),
	}
	r.gwPtr.Store(&newMap)

	metrics.RegisteredGateways.Set(float64(len(newMap)))

	if existed {
		r.logger.Info("gateway re-registered",
			logger.Event(logger.EventGatewayRegister),
			zap.String("gateway_addr", addr))
	} else {
		r.logger.Info("gateway registered",
			logger.Event(logger.EventGatewayRegister),
			zap.String("gateway_addr", addr),
			zap.Int("total_gateways", len(newMap)))
	}
}

// Heartbeat updates the last heartbeat time and active connections for a gateway.
// Returns false if the gateway is not registered (caller should re-register).
func (r *GatewayRegistry) Heartbeat(addr string, activeConns int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	cur := r.loadGateways()
	gw, ok := cur[addr]
	if !ok {
		r.logger.Warn("heartbeat from unregistered gateway",
			zap.String("gateway_addr", addr))
		return false
	}

	// COW: clone map, create updated GatewayInfo.
	newMap := r.cloneMap()
	updated := *gw // shallow copy
	updated.LastHeartbeat = time.Now()
	updated.ActiveConns = activeConns
	newMap[addr] = &updated
	r.gwPtr.Store(&newMap)

	metrics.HeartbeatTotal.WithLabelValues(addr).Inc()
	return true
}

// Unregister removes a gateway from the registry.
func (r *GatewayRegistry) Unregister(addr string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	cur := r.loadGateways()
	if _, ok := cur[addr]; ok {
		newMap := r.cloneMap()
		delete(newMap, addr)
		r.gwPtr.Store(&newMap)
		metrics.RegisteredGateways.Set(float64(len(newMap)))
		r.logger.Info("gateway unregistered",
			zap.String("gateway_addr", addr))
	}
}

// Count returns the number of registered gateways.
func (r *GatewayRegistry) Count() int {
	return len(r.loadGateways())
}

// GetAll returns the current gateway map snapshot (lock-free read via COW).
// The returned map must not be modified by the caller.
func (r *GatewayRegistry) GetAll() map[string]*domain.GatewayInfo {
	return r.loadGateways()
}

// StartExpireLoop runs a background goroutine that removes gateways
// whose heartbeat has exceeded the timeout. Removed gateways will
// automatically re-register via client-side self-healing.
func (r *GatewayRegistry) StartExpireLoop(ctx context.Context, timeout time.Duration) {
	ticker := time.NewTicker(timeout / 2)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.expireOnce(timeout)
			}
		}
	}()
}

func (r *GatewayRegistry) expireOnce(timeout time.Duration) {
	r.mu.Lock()

	cur := r.loadGateways()
	now := time.Now()
	var expired []string
	for addr, gw := range cur {
		since := now.Sub(gw.LastHeartbeat)
		if since > timeout {
			expired = append(expired, addr)
			r.logger.Warn("gateway removed after heartbeat timeout",
				logger.Event(logger.EventGatewayExpired),
				zap.String("gateway_addr", addr),
				zap.Duration("since_last_heartbeat", since),
				zap.Duration("timeout", timeout))
		}
	}

	if len(expired) > 0 {
		newMap := r.cloneMap()
		for _, addr := range expired {
			delete(newMap, addr)
		}
		r.gwPtr.Store(&newMap)
		metrics.RegisteredGateways.Set(float64(len(newMap)))
	}
	r.mu.Unlock()

	// Cleanup ghost allocations outside the lock to avoid blocking the registry.
	if r.onExpired != nil {
		for _, addr := range expired {
			r.onExpired(context.Background(), addr)
		}
	}
}
