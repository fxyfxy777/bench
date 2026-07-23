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
	estDuration  string
}

// fwScenario defines a backend-framework × proxy-framework × frontend-protocol combination.
type fwScenario struct {
	name         string
	proxyType    string // "nethttp" or "hertz"
	backendType  string // "nethttp" or "hertz"
	backendH2C   bool
	frontendMode string // "standard" (always)
	frontendH2C  bool   // client uses h2c to connect to proxy
}

func main() {
	concurrency := flag.Int("c", 500, "concurrency level")
	numBackends := flag.Int("backends", 4, "number of GPU backend instances per type")
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

	// Scenarios: proxy framework × backend framework × frontend protocol
	// Naming: "frontend→proxy(framework)→backend(protocol)"
	scenarios := []fwScenario{
		// net/http proxy — net/http backend h1
		{"h1→nethttp(h1)", "nethttp", "nethttp", false, "standard", false},
		{"h2c→nethttp(h1)", "nethttp", "nethttp", false, "standard", true},
		// net/http proxy — net/http backend h2c
		{"h1→nethttp(h2c)", "nethttp", "nethttp", true, "standard", false},
		{"h2c→nethttp(h2c)", "nethttp", "nethttp", true, "standard", true},
		// net/http proxy — hertz backend h1
		{"h1→hertzBE(h1)", "nethttp", "hertz", false, "standard", false},
		{"h2c→hertzBE(h1)", "nethttp", "hertz", false, "standard", true},
		// hertz proxy — net/http backend h1
		{"h1→hertzPx→(h1)", "hertz", "nethttp", false, "standard", false},
		{"h2c→hertzPx→(h1)", "hertz", "nethttp", false, "standard", true},
		// hertz proxy — net/http backend h2c
		{"h1→hertzPx→(h2c)", "hertz", "nethttp", true, "standard", false},
		{"h2c→hertzPx→(h2c)", "hertz", "nethttp", true, "standard", true},
	}

	fmt.Println("=================================================================")
	fmt.Println("  Framework Benchmark: hertz vs net/http (Proxy & Backend)")
	fmt.Println("  Client → Proxy(hertz|net/http) → Backend(hertz|net/http)")
	fmt.Println("=================================================================")
	fmt.Printf("  Go Version   : %s\n", runtime.Version())
	fmt.Printf("  GOMAXPROCS   : %d\n", runtime.GOMAXPROCS(0))
	fmt.Printf("  Concurrency  : %d\n", *concurrency)
	fmt.Printf("  Backends     : %d per type\n", *numBackends)
	fmt.Printf("  Data profiles: %d\n", len(profiles))
	fmt.Printf("  Req rounds   : %v\n", reqRounds)
	fmt.Printf("  Scenarios    : %d\n", len(scenarios))
	fmt.Println("-----------------------------------------------------------------")
	fmt.Println("  net/http proxy: supports h1 and h2c frontend (native)")
	fmt.Println("  hertz proxy: supports h1 and h2c frontend (hertz-contrib/http2)")
	fmt.Println("  hertz backend: HTTP/1.1 only (no HTTP/2 support)")
	fmt.Println("  net/http backend: HTTP/1.1 and h2c (HTTP/2 cleartext)")
	fmt.Println("-----------------------------------------------------------------")
	fmt.Println("  Methodology: same as proxybench v2")
	fmt.Println("    - Client timeout: 120s")
	fmt.Println("    - Memory: HeapInuse (live heap)")
	fmt.Println("    - GR tracking: 50ms peak sampling")
	fmt.Println("    - Cooldown: 2s + GC between scenarios")
	fmt.Println("=================================================================")

	globalStart := time.Now()

	// Port allocation:
	//   net/http h1 backends:  127.0.0.1:18001-18004
	//   net/http h2c backends: 127.0.0.1:18101-18104
	//   hertz h1 backends:     127.0.0.1:18201-18204
	//   proxies:               127.0.0.1:19001-19010

	for _, dp := range profiles {
		fmt.Printf("\n\n###############################################################\n")
		fmt.Printf("  DATA PROFILE: %s  (tokens=%d, delay=%v, est=%s)\n",
			dp.name, dp.tokensPerReq, dp.tokenDelay, dp.estDuration)
		fmt.Printf("###############################################################\n")

		// Start net/http h1 backends
		var nethttpH1Addrs []string
		var nethttpH1Srvs []*servers.BackendServer
		for i := 0; i < *numBackends; i++ {
			addr := fmt.Sprintf("127.0.0.1:%d", 18001+i)
			srv := servers.StartBackend(addr, false, dp.tokensPerReq, dp.tokenDelay)
			nethttpH1Addrs = append(nethttpH1Addrs, addr)
			nethttpH1Srvs = append(nethttpH1Srvs, srv)
		}

		// Start net/http h2c backends
		var nethttpH2Addrs []string
		var nethttpH2Srvs []*servers.BackendServer
		for i := 0; i < *numBackends; i++ {
			addr := fmt.Sprintf("127.0.0.1:%d", 18101+i)
			srv := servers.StartBackend(addr, true, dp.tokensPerReq, dp.tokenDelay)
			nethttpH2Addrs = append(nethttpH2Addrs, addr)
			nethttpH2Srvs = append(nethttpH2Srvs, srv)
		}

		// Start hertz h1 backends
		var hertzAddrs []string
		var hertzSrvs []*servers.HertzBackendServer
		for i := 0; i < *numBackends; i++ {
			addr := fmt.Sprintf("127.0.0.1:%d", 18201+i)
			srv := servers.StartHertzBackend(addr, dp.tokensPerReq, dp.tokenDelay)
			hertzAddrs = append(hertzAddrs, addr)
			hertzSrvs = append(hertzSrvs, srv)
		}

		time.Sleep(800 * time.Millisecond) // wait for all servers to start

		for _, nReq := range reqRounds {
			fmt.Printf("\n  ─── %s | %d requests (c=%d) ───\n", dp.name, nReq, *concurrency)

			var results []bench.Result

			for si, sc := range scenarios {
				proxyAddr := fmt.Sprintf("127.0.0.1:%d", 19001+si)

				// Pick backend addrs based on scenario
				var backendAddrs []string
				var backendH2C bool
				switch {
				case sc.backendType == "nethttp" && sc.backendH2C:
					backendAddrs = nethttpH2Addrs
					backendH2C = true
				case sc.backendType == "nethttp" && !sc.backendH2C:
					backendAddrs = nethttpH1Addrs
					backendH2C = false
				case sc.backendType == "hertz":
					backendAddrs = hertzAddrs
					backendH2C = false
				}

				proxyCfg := proxy.Config{
					ListenAddr:   proxyAddr,
					FrontendMode: sc.frontendMode,
					EnableH2C:    sc.frontendH2C,
					Backends:     backendAddrs,
					BackendH2C:   backendH2C,
					BackendPool:  *concurrency * 2,
				}
				var p proxy.SSEProxy
				if sc.proxyType == "hertz" {
					p = proxy.NewHertz(proxyCfg)
				} else {
					p = proxy.New(proxyCfg)
				}
				p.Start()
				time.Sleep(300 * time.Millisecond)

				// Stabilize baseline
				runtime.GC()
				runtime.GC()
				time.Sleep(100 * time.Millisecond)

				var memBefore runtime.MemStats
				runtime.ReadMemStats(&memBefore)
				grBefore := runtime.NumGoroutine()

				// Track peak goroutines
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

				var memAfter runtime.MemStats
				runtime.ReadMemStats(&memAfter)
				grAfter := runtime.NumGoroutine()

				results = append(results, r)

				heapBeforeMB := float64(memBefore.HeapInuse) / (1 << 20)
				heapAfterMB := float64(memAfter.HeapInuse) / (1 << 20)
				heapDeltaMB := heapAfterMB - heapBeforeMB
				grDelta := grAfter - grBefore

				fmt.Printf("    %-20s | Req/s %7.1f | P99 %10s | TTFT %8s | Fail %5d | PeakGR %5d | GRΔ %+5d | Heap %+.1fMB | PeakConn %5d\n",
					sc.name, r.ReqPerSec,
					r.P99Latency.Round(time.Microsecond),
					r.FirstTokenAvg.Round(time.Microsecond),
					r.FailedRequests,
					atomic.LoadInt64(&peakGR), grDelta,
					heapDeltaMB,
					atomic.LoadInt64(&peakConn),
				)

				p.Stop()

				// Cooldown
				runtime.GC()
				time.Sleep(2 * time.Second)
			}

			// Comparison table
			fmt.Printf("\n")
			bench.PrintComparison(results)
		}

		// Stop all backends
		for _, s := range nethttpH1Srvs {
			s.Stop()
		}
		for _, s := range nethttpH2Srvs {
			s.Stop()
		}
		for _, s := range hertzSrvs {
			s.Stop()
		}
		time.Sleep(1 * time.Second)
	}

	fmt.Printf("\n\n  Total wall time: %s\n", time.Since(globalStart).Round(time.Millisecond))
	fmt.Println("\n  Key comparison points:")
	fmt.Println("  - hertz proxy vs nethttp proxy: framework overhead at proxy layer")
	fmt.Println("  - h1 frontend vs h2c frontend: protocol upgrade benefit")
	fmt.Println("  - hertz backend vs nethttp backend: framework overhead at backend layer")
	fmt.Println("  - h1 backend vs h2c backend: proxy-to-backend protocol impact")
	fmt.Println("\nAll benchmarks complete.")
	os.Exit(0)
}
