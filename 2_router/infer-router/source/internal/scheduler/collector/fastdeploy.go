package collector

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// FastDeploy Prometheus metric names.
const (
	fdMetricWaiting   = "fastdeploy:num_requests_waiting "
	fdMetricRunning   = "fastdeploy:num_requests_running "
	fdMetricAvailable = "fastdeploy:available_gpu_block_num "
	fdMetricCacheUsage = "fastdeploy:gpu_cache_usage_perc "
)

// fdMetricCount is the total number of target metrics to find.
const fdMetricCount = 4

// FastDeployFetcher scrapes FastDeploy's /metrics endpoint and extracts
// waiting count, running count, and available GPU blocks.
type FastDeployFetcher struct {
	client *http.Client
}

// NewFastDeployFetcher creates a fetcher for FastDeploy backend instances.
func NewFastDeployFetcher(client *http.Client) *FastDeployFetcher {
	return &FastDeployFetcher{client: client}
}

func (f *FastDeployFetcher) Name() string { return "fastdeploy" }

// Fetch scrapes the FastDeploy /metrics endpoint and returns parsed metrics.
// Uses streaming line-scan with prefix matching and early exit after finding
// all 3 target metrics — zero intermediate allocation.
func (f *FastDeployFetcher) Fetch(ctx context.Context, metricsEndpoint string) (InstanceMetrics, error) {
	url := "http://" + metricsEndpoint + "/metrics"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return InstanceMetrics{}, fmt.Errorf("create request for %s: %w", url, err)
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return InstanceMetrics{}, fmt.Errorf("fetch metrics from %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Drain body to reuse connection.
		_, _ = io.Copy(io.Discard, resp.Body)
		return InstanceMetrics{}, &HttpStatusError{StatusCode: resp.StatusCode, URL: url}
	}

	return parseFastDeployMetrics(resp.Body)
}

// parseFastDeployMetrics parses a Prometheus text exposition body, extracting
// the 3 target FastDeploy metrics. Uses bufio.Scanner for streaming line-by-line
// processing. Early-exits after all 3 metrics are found.
func parseFastDeployMetrics(r io.Reader) (InstanceMetrics, error) {
	var m InstanceMetrics
	found := 0

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()

		// Skip comments (# HELP, # TYPE) and empty lines.
		if len(line) == 0 || line[0] == '#' {
			continue
		}

		switch {
		case strings.HasPrefix(line, fdMetricWaiting):
			v, err := parseMetricValue(line, len(fdMetricWaiting))
			if err != nil {
				return m, fmt.Errorf("parse %s: %w", fdMetricWaiting[:len(fdMetricWaiting)-1], err)
			}
			m.WaitingCount = v
			found++
		case strings.HasPrefix(line, fdMetricRunning):
			v, err := parseMetricValue(line, len(fdMetricRunning))
			if err != nil {
				return m, fmt.Errorf("parse %s: %w", fdMetricRunning[:len(fdMetricRunning)-1], err)
			}
			m.RunningCount = v
			found++
		case strings.HasPrefix(line, fdMetricAvailable):
			v, err := parseMetricValue(line, len(fdMetricAvailable))
			if err != nil {
				return m, fmt.Errorf("parse %s: %w", fdMetricAvailable[:len(fdMetricAvailable)-1], err)
			}
			m.AvailableBlocks = v
			found++
		case strings.HasPrefix(line, fdMetricCacheUsage):
			v, err := parseMetricValue(line, len(fdMetricCacheUsage))
			if err != nil {
				return m, fmt.Errorf("parse %s: %w", fdMetricCacheUsage[:len(fdMetricCacheUsage)-1], err)
			}
			m.GpuCacheUsagePerc = float64(v) / 100.0  // Convert to 0-1 range
			found++
		}

		if found >= fdMetricCount {
			break // early exit — all target metrics found
		}
	}

	if err := scanner.Err(); err != nil {
		return m, fmt.Errorf("scan metrics body: %w", err)
	}

	return m, nil
}

// parseMetricValue extracts the numeric value from a Prometheus metric line,
// starting at the given offset (after the metric name + space).
// Handles both integer and float formats (truncates float to int64).
func parseMetricValue(line string, offset int) (int64, error) {
	valueStr := line[offset:]
	// Trim trailing whitespace or timestamp.
	if idx := strings.IndexByte(valueStr, ' '); idx >= 0 {
		valueStr = valueStr[:idx]
	}

	// Try integer first (common case, avoids float parsing overhead).
	if v, err := strconv.ParseInt(valueStr, 10, 64); err == nil {
		return v, nil
	}

	// Fall back to float (e.g. "3.0" or scientific notation).
	f, err := strconv.ParseFloat(valueStr, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid metric value %q: %w", valueStr, err)
	}
	return int64(f), nil
}
