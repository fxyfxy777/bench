package bench

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/http2"
)

// Result holds benchmark results for one framework.
type Result struct {
	Framework      string
	Concurrency    int
	TotalRequests  int64
	FailedRequests int64
	TotalTokens    int64
	TotalDuration  time.Duration
	AvgLatency     time.Duration
	P50Latency     time.Duration
	P90Latency     time.Duration
	P99Latency     time.Duration
	FirstTokenAvg  time.Duration
	FirstTokenP50  time.Duration
	FirstTokenP90  time.Duration
	ReqPerSec      float64
	TokensPerSec   float64
}

// newHTTP1Client creates an http.Client with tuned connection pool for HTTP/1.1.
func newHTTP1Client(concurrency int) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        concurrency * 2,
			MaxIdleConnsPerHost: concurrency * 2,
			MaxConnsPerHost:     concurrency * 2,
			IdleConnTimeout:     90 * time.Second,
			DisableKeepAlives:   false,
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		},
		Timeout: 120 * time.Second,
	}
}

// newHTTP2Client creates an http.Client that forces h2c (HTTP/2 cleartext).
func newHTTP2Client() *http.Client {
	return &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLS: func(network, addr string, _ *tls.Config) (net.Conn, error) {
				return net.DialTimeout(network, addr, 5*time.Second)
			},
		},
		Timeout: 120 * time.Second,
	}
}

// Run executes the SSE benchmark against the given addr using HTTP/1.1 with tuned pool.
func Run(framework, addr string, concurrency, totalReqs int) Result {
	client := newHTTP1Client(concurrency)
	return runWith(client, framework, addr, concurrency, totalReqs)
}

// RunH2C executes the SSE benchmark using HTTP/2 cleartext (h2c).
func RunH2C(framework, addr string, concurrency, totalReqs int) Result {
	client := newHTTP2Client()
	return runWith(client, framework, addr, concurrency, totalReqs)
}

func runWith(client *http.Client, framework, addr string, concurrency, totalReqs int) Result {
	url := fmt.Sprintf("http://%s/sse", addr)

	var (
		wg          sync.WaitGroup
		completed   int64
		failed      int64
		totalTokens int64
		mu          sync.Mutex
		latencies   = make([]time.Duration, 0, totalReqs)
		firstTokens = make([]time.Duration, 0, totalReqs)
	)

	sem := make(chan struct{}, concurrency)
	start := time.Now()

	for range totalReqs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			reqStart := time.Now()
			resp, err := client.Get(url)
			if err != nil {
				atomic.AddInt64(&failed, 1)
				return
			}
			defer resp.Body.Close()

			reader := bufio.NewReader(resp.Body)
			tokens := 0
			var firstTokenTime time.Duration
			firstTokenRecorded := false

			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					if err != io.EOF {
						atomic.AddInt64(&failed, 1)
					}
					break
				}
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				if strings.HasPrefix(line, "data: ") {
					data := strings.TrimPrefix(line, "data: ")
					if data == "[DONE]" {
						break
					}
					tokens++
					if !firstTokenRecorded {
						firstTokenTime = time.Since(reqStart)
						firstTokenRecorded = true
					}
				}
			}

			latency := time.Since(reqStart)
			atomic.AddInt64(&completed, 1)
			atomic.AddInt64(&totalTokens, int64(tokens))

			mu.Lock()
			latencies = append(latencies, latency)
			if firstTokenRecorded {
				firstTokens = append(firstTokens, firstTokenTime)
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	totalDuration := time.Since(start)

	slices.Sort(latencies)
	slices.Sort(firstTokens)

	res := Result{
		Framework:      framework,
		Concurrency:    concurrency,
		TotalRequests:  completed,
		FailedRequests: failed,
		TotalTokens:    totalTokens,
		TotalDuration:  totalDuration,
	}

	if len(latencies) > 0 {
		var sum time.Duration
		for _, l := range latencies {
			sum += l
		}
		res.AvgLatency = sum / time.Duration(len(latencies))
		res.P50Latency = percentile(latencies, 0.50)
		res.P90Latency = percentile(latencies, 0.90)
		res.P99Latency = percentile(latencies, 0.99)
	}

	if len(firstTokens) > 0 {
		var sum time.Duration
		for _, l := range firstTokens {
			sum += l
		}
		res.FirstTokenAvg = sum / time.Duration(len(firstTokens))
		res.FirstTokenP50 = percentile(firstTokens, 0.50)
		res.FirstTokenP90 = percentile(firstTokens, 0.90)
	}

	secs := totalDuration.Seconds()
	if secs > 0 {
		res.ReqPerSec = float64(completed) / secs
		res.TokensPerSec = float64(totalTokens) / secs
	}

	return res
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p)
	return sorted[idx]
}

// PrintResult prints benchmark result in a formatted way.
func PrintResult(r Result) {
	fmt.Printf("\n╔══════════════════════════════════════════════════╗\n")
	fmt.Printf("║  %-46s  ║\n", r.Framework)
	fmt.Printf("╠══════════════════════════════════════════════════╣\n")
	fmt.Printf("║  Concurrency        : %-26d║\n", r.Concurrency)
	fmt.Printf("║  Total Requests     : %-26d║\n", r.TotalRequests)
	fmt.Printf("║  Failed Requests    : %-26d║\n", r.FailedRequests)
	fmt.Printf("║  Total Tokens       : %-26d║\n", r.TotalTokens)
	fmt.Printf("║  Total Duration     : %-26s║\n", r.TotalDuration.Round(time.Millisecond))
	fmt.Printf("║──────────────────────────────────────────────────║\n")
	fmt.Printf("║  Req/s              : %-26.2f║\n", r.ReqPerSec)
	fmt.Printf("║  Tokens/s           : %-26.2f║\n", r.TokensPerSec)
	fmt.Printf("║──────────────────────────────────────────────────║\n")
	fmt.Printf("║  Latency (avg)      : %-26s║\n", r.AvgLatency.Round(time.Microsecond))
	fmt.Printf("║  Latency (p50)      : %-26s║\n", r.P50Latency.Round(time.Microsecond))
	fmt.Printf("║  Latency (p90)      : %-26s║\n", r.P90Latency.Round(time.Microsecond))
	fmt.Printf("║  Latency (p99)      : %-26s║\n", r.P99Latency.Round(time.Microsecond))
	fmt.Printf("║──────────────────────────────────────────────────║\n")
	fmt.Printf("║  TTFT (avg)         : %-26s║\n", r.FirstTokenAvg.Round(time.Microsecond))
	fmt.Printf("║  TTFT (p50)         : %-26s║\n", r.FirstTokenP50.Round(time.Microsecond))
	fmt.Printf("║  TTFT (p90)         : %-26s║\n", r.FirstTokenP90.Round(time.Microsecond))
	fmt.Printf("╚══════════════════════════════════════════════════╝\n")
}

// PrintComparison prints a comparison table of all results.
func PrintComparison(results []Result) {
	fmt.Printf("\n\n")
	fmt.Printf("┌─────────────────┬──────────┬──────────┬────────────┬────────────┬────────────┬────────────┬────────────┬────────┐\n")
	fmt.Printf("│ %-15s │ %8s │ %8s │ %10s │ %10s │ %10s │ %10s │ %10s │ %6s │\n",
		"Framework", "Req/s", "Tok/s", "Avg Lat", "P50 Lat", "P99 Lat", "TTFT Avg", "TTFT P90", "Failed")
	fmt.Printf("├─────────────────┼──────────┼──────────┼────────────┼────────────┼────────────┼────────────┼────────────┼────────┤\n")
	for _, r := range results {
		fmt.Printf("│ %-15s │ %8.1f │ %8.0f │ %10s │ %10s │ %10s │ %10s │ %10s │ %6d │\n",
			r.Framework,
			r.ReqPerSec,
			r.TokensPerSec,
			r.AvgLatency.Round(time.Microsecond),
			r.P50Latency.Round(time.Microsecond),
			r.P99Latency.Round(time.Microsecond),
			r.FirstTokenAvg.Round(time.Microsecond),
			r.FirstTokenP90.Round(time.Microsecond),
			r.FailedRequests,
		)
	}
	fmt.Printf("└─────────────────┴──────────┴──────────┴────────────┴────────────┴────────────┴────────────┴────────────┴────────┘\n")
}
