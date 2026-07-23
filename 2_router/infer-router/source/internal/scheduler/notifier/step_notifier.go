package notifier

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/yzx/rl-router/internal/domain"
	"github.com/yzx/rl-router/pkg/jsonutil"
	"github.com/yzx/rl-router/pkg/logger"
	"github.com/yzx/rl-router/pkg/metrics"
)

// GatewayLister returns all registered gateways.
// registry.GatewayRegistry satisfies this interface.
type GatewayLister interface {
	GetAll() map[string]*domain.GatewayInfo
}

// StepNotifier pushes step-state changes to all registered gateways via HTTP.
// This eliminates the heartbeat-interval delay for critical state transitions.
type StepNotifier struct {
	registry GatewayLister
	client   *http.Client
	logger   *zap.Logger
	ctx      context.Context
	cancel   context.CancelFunc
}

const (
	notifyPath       = "/v1/internal/step-state"
	notifyTimeout    = 2 * time.Second
	notifyMaxRetries = 3
)

func NewStepNotifier(registry GatewayLister, logger *zap.Logger) *StepNotifier {
	ctx, cancel := context.WithCancel(context.Background())
	return &StepNotifier{
		registry: registry,
		client: &http.Client{
			Timeout: notifyTimeout,
			Transport: &http.Transport{
				MaxIdleConns:        200,
				MaxIdleConnsPerHost: 2,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		logger: logger,
		ctx:    ctx,
		cancel: cancel,
	}
}

// Stop cancels all in-flight and future notification goroutines.
// Safe to call multiple times.
func (n *StepNotifier) Stop() {
	n.cancel()
}

// BroadcastStepState sends the current step state to all registered gateways.
// Non-blocking: runs notifications concurrently in background goroutines.
func (n *StepNotifier) BroadcastStepState(state domain.StepState) {
	// Skip broadcast if already stopped.
	select {
	case <-n.ctx.Done():
		return
	default:
	}

	gateways := n.registry.GetAll()
	if len(gateways) == 0 {
		return
	}

	body, err := jsonutil.Marshal(state)
	if err != nil {
		n.logger.Error("failed to marshal step state notification",
			zap.Error(err))
		return
	}

	for _, gw := range gateways {
		go n.notifyGateway(gw.GatewayAddr, body)
	}
}

func (n *StepNotifier) notifyGateway(addr string, body []byte) {
	url := fmt.Sprintf("http://%s%s", addr, notifyPath)

	for attempt := 1; attempt <= notifyMaxRetries; attempt++ {
		ctx, cancel := context.WithTimeout(n.ctx, notifyTimeout)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			cancel()
			n.logger.Error("failed to create notification request",
				zap.String("gateway_addr", addr),
				zap.Error(err))
			return
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := n.client.Do(req)
		cancel()
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				n.logger.Debug("notified gateway of step state change",
					zap.String("gateway_addr", addr),
					zap.Int("attempt", attempt))
				metrics.StepNotifyTotal.WithLabelValues("success").Inc()
				return
			}
			n.logger.Warn("gateway notification returned non-OK status",
				zap.String("gateway_addr", addr),
				zap.Int("status", resp.StatusCode),
				zap.Int("attempt", attempt))
		} else {
			// If the parent context was cancelled, stop retrying.
			if n.ctx.Err() != nil {
				return
			}
			n.logger.Warn("gateway notification failed",
				zap.String("gateway_addr", addr),
				zap.Error(err),
				zap.Int("attempt", attempt))
		}

		if attempt < notifyMaxRetries {
			select {
			case <-time.After(time.Duration(attempt*100) * time.Millisecond):
			case <-n.ctx.Done():
				return
			}
		}
	}

	n.logger.Error("failed to notify gateway after all retries",
		logger.Event(logger.EventStepStart),
		logger.Status(logger.StatusFail),
		logger.Reason(logger.ReasonInternalError),
		zap.String("gateway_addr", addr),
		zap.String("url", url),
		zap.Int("max_retries", notifyMaxRetries))
	metrics.StepNotifyTotal.WithLabelValues("failure").Inc()
}
