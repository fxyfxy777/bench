package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"sync/atomic"
	"time"

	"sse-bench/bench"
	"sse-bench/proxy"
	"sse-bench/servers"
)

// dataProfile defines a token-count / delay combination.
type dataProfile struct {
	name         string
	tokensPerReq int
	tokenDelay   time.Duration
	// estimated single-request duration for display
	estDuration string
}

// protoScenario defines a frontend/backend protocol combination.
type protoScenario struct {
	name         string
	frontendMode string // "standard" or "hijack"
	frontendH2C  bool   // client uses h2c to connect to proxy
	backendH2C   bool
	// caveat is printed alongside results if non-empty
	caveat string
}

func main() {
	concurrency := flag.Int("c", 500, "concurrency level")
	numBackends := flag.Int("backends", 4, "number of GPU backend instances")
	quick := flag.Bool("quick", false, "quick mode: only 1K requests, light+heavy data")
	flag.Parse()

	profiles := []dataProfile{
		{"light(50tok)", 50, 5 * time.Millisecond, "~250ms"},
		{"medium(200tok)", 200, 2 * time.Millisecond, "~400ms"},
		{"heavy(500tok)", 500, 1 * time.Millisecond, "~500ms"},
		{"xlarge(1Ktok)", 1000, 1 * time.Millisecond, "~1s"},
		{"xxlarge(10Ktok)", 10000, 100 * time.Microsecond, "~1s"},
	}

	reqRounds := []int{1000, 10000}

	if *quick {
		profiles = []dataProfile{profiles[0], profiles[2]}
		reqRounds = []int{1000}
	}

	// NOTE on hijack scenarios:
	// Hijack mode takes over the raw TCP connection, which disables HTTP Keep-Alive.
	// This means each request opens a new TCP connection (no reuse).
	// For short-lived SSE streams this adds connection-setup overhead; for long-lived
	// streams (real LLM inference) the overhead is amortized and hijack's lower
	// per-flush overhead dominates. Interpret hijack results with this caveat.
	hijackCaveat := "* hijack disables Keep-Alive"

	scenarios := []protoScenario{
		{"h1→h1", "standard", false, false, ""},
		{"h1→h2c", "standard", false, true, ""},
		{"hijack→h1", "hijack", false, false, hijackCaveat},
		{"hijack→h2c", "hijack", false, true, hijackCaveat},
		{"h2c→h1", "standard", true, false, ""},
		{"h2c→h2c", "standard", true, true, ""},
	}

	fmt.Println("=================================================================")
	fmt.Println("  Integrated SSE Proxy Benchmark (v2 — methodology fixes)")
	fmt.Println("  Client → Router(proxy) → GPU Backends")
	fmt.Println("=================================================================")
	fmt.Printf("  Go Version   : %s\n", runtime.Version())
	fmt.Printf("  GOMAXPROCS   : %d\n", runtime.GOMAXPROCS(0))
	fmt.Printf("  Concurrency  : %d\n", *concurrency)
	fmt.Printf("  Backends     : %d\n", *numBackends)
	fmt.Printf("  Data profiles: %d\n", len(profiles))
	fmt.Printf("  Req rounds   : %v\n", reqRounds)
	fmt.Printf("  Scenarios    : %d\n", len(scenarios))
	fmt.Println("-----------------------------------------------------------------")
	fmt.Println("  Fixes applied:")
	fmt.Println("    1. Client timeout: 120s (was 30s, avoids false failures)")
	fmt.Println("    2. Memory: HeapInuse (live) instead of TotalAlloc (cumulative)")
	fmt.Println("    3. GR tracking: peak during bench, not just delta after")
	fmt.Println("    4. Cooldown: 2s + GC between scenarios to reduce state leakage")
	fmt.Println("    5. Proxy active-conn tracking for peak concurrency")
	fmt.Println("    6. Hijack caveat clearly labeled in output")
	fmt.Println("=================================================================")

	globalStart := time.Now()

	// For each data profile, start backends, run all scenarios x all request rounds.
	for _, dp := range profiles {
		fmt.Printf("\n\n###############################################################\n")
		fmt.Printf("  DATA PROFILE: %s  (tokens=%d, delay=%v, est=%s)\n",
			dp.name, dp.tokensPerReq, dp.tokenDelay, dp.estDuration)
		fmt.Printf("###############################################################\n")

		// Start h1 backends
		var h1Addrs []string
		var h1Srvs []*servers.BackendServer
		for i := 0; i < *numBackends; i++ {
			addr := fmt.Sprintf("127.0.0.1:%d", 17001+i)
			srv := servers.StartBackend(addr, false, dp.tokensPerReq, dp.tokenDelay)
			h1Addrs = append(h1Addrs, addr)
			h1Srvs = append(h1Srvs, srv)
		}
		// Start h2c backends
		var h2Addrs []string
		var h2Srvs []*servers.BackendServer
		for i := 0; i < *numBackends; i++ {
			addr := fmt.Sprintf("127.0.0.1:%d", 17101+i)
			srv := servers.StartBackend(addr, true, dp.tokensPerReq, dp.tokenDelay)
			h2Addrs = append(h2Addrs, addr)
			h2Srvs = append(h2Srvs, srv)
		}
		time.Sleep(500 * time.Millisecond)

		for _, nReq := range reqRounds {
			fmt.Printf("\n  ─── %s | %d requests (c=%d) ───\n", dp.name, nReq, *concurrency)

			var results []bench.Result

			for si, sc := range scenarios {
				proxyAddr := fmt.Sprintf("127.0.0.1:%d", 16001+si)
				backendAddrs := h1Addrs
				if sc.backendH2C {
					backendAddrs = h2Addrs
				}

				p := proxy.New(proxy.Config{
					ListenAddr:   proxyAddr,
					FrontendMode: sc.frontendMode,
					EnableH2C:    sc.frontendH2C,
					Backends:     backendAddrs,
					BackendH2C:   sc.backendH2C,
					BackendPool:  *concurrency * 2,
				})
				p.Start()
				time.Sleep(300 * time.Millisecond)

				// --- FIX #3: Stabilize baseline with GC before measurement ---
				runtime.GC()
				runtime.GC() // double GC to reclaim finalizer-dependent objects
				time.Sleep(100 * time.Millisecond)

				// Memory snapshot: use HeapInuse (live heap), not TotalAlloc (cumulative)
				var memBefore runtime.MemStats
				runtime.ReadMemStats(&memBefore)
				grBefore := runtime.NumGoroutine()

				// Track peak goroutines via sampling goroutine
				peakGR := int64(grBefore)
				peakConn := int64(0)
				stopPeakCh := make(chan struct{})
				go func() {
					ticker := time.NewTicker(50 * time.Millisecond)
					defer ticker.Stop()
					for {
						select {
						case <-stopPeakCh:
							return
						case <-ticker.C:
							gr := int64(runtime.NumGoroutine())
							for {
								old := atomic.LoadInt64(&peakGR)
								if gr <= old {
									break
								}
								if atomic.CompareAndSwapInt64(&peakGR, old, gr) {
									break
								}
							}
							conn := p.ActiveConns()
							for {
								old := atomic.LoadInt64(&peakConn)
								if conn <= old {
									break
								}
								if atomic.CompareAndSwapInt64(&peakConn, old, conn) {
									break
								}
							}
						}
					}
				}()

				// Run benchmark
				var r bench.Result
				if sc.frontendH2C {
					r = bench.RunH2C(sc.name, proxyAddr, *concurrency, nReq)
				} else {
					r = bench.Run(sc.name, proxyAddr, *concurrency, nReq)
				}

				close(stopPeakCh)

				// Memory snapshot after
				var memAfter runtime.MemStats
				runtime.ReadMemStats(&memAfter)
				grAfter := runtime.NumGoroutine()

				results = append(results, r)

				// FIX #2: HeapInuse = live heap pages (not cumulative)
				heapBeforeMB := float64(memBefore.HeapInuse) / (1 << 20)
				heapAfterMB := float64(memAfter.HeapInuse) / (1 << 20)
				heapDeltaMB := heapAfterMB - heapBeforeMB
				grDelta := grAfter - grBefore

				cav := ""
				if sc.caveat != "" {
					cav = " " + sc.caveat
				}

				fmt.Printf("    %-14s | Req/s %7.1f | P99 %10s | TTFT %8s | Fail %5d | PeakGR %5d | GRΔ %+5d | Heap %+.1fMB (%.1f→%.1fMB) | PeakConn %5d%s\n",
					sc.name, r.ReqPerSec,
					r.P99Latency.Round(time.Microsecond),
					r.FirstTokenAvg.Round(time.Microsecond),
					r.FailedRequests,
					atomic.LoadInt64(&peakGR), grDelta,
					heapDeltaMB, heapBeforeMB, heapAfterMB,
					atomic.LoadInt64(&peakConn),
					cav,
				)

				p.Stop()

				// FIX #4: Cooldown between scenarios to let TIME_WAIT sockets expire
				// and goroutines settle
				runtime.GC()
				time.Sleep(2 * time.Second)
			}

			// Comparison table for this round
			fmt.Printf("\n")
			bench.PrintComparison(results)
		}

		// Stop backends for this data profile
		for _, s := range h1Srvs {
			s.Stop()
		}
		for _, s := range h2Srvs {
			s.Stop()
		}
		time.Sleep(1 * time.Second)
	}

	fmt.Printf("\n\n  Total wall time: %s\n", time.Since(globalStart).Round(time.Millisecond))
	fmt.Println("\n  Methodology notes:")
	fmt.Println("  - Memory: HeapInuse measures live heap pages (not cumulative allocs)")
	fmt.Println("  - GR: PeakGR sampled every 50ms during benchmark run")
	fmt.Println("  - Cooldown: 2s + GC between each scenario to reduce cross-contamination")
	fmt.Println("  - Hijack: disables Keep-Alive, results marked with caveat")
	fmt.Println("  - Client timeout: 120s to accommodate xxlarge data profiles")
	fmt.Println("  - Limitation: client/proxy/backend share process; heap includes all three")
	fmt.Println("\nAll benchmarks complete.")
	os.Exit(0)
}
