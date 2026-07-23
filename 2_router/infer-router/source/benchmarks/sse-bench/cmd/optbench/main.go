package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"time"

	"sse-bench/bench"
	"sse-bench/servers"
)

type testCase struct {
	name  string
	addr  string
	start func() func()
}

func main() {
	concurrency := flag.Int("c", 500, "concurrency level")
	totalReqs := flag.Int("n", 10000, "total requests per test")
	flag.Parse()

	cases := []testCase{
		{
			name: "baseline",
			addr: "127.0.0.1:19001",
			start: func() func() {
				srv := servers.StartNetHTTP(":19001")
				return func() { srv.Shutdown(context.Background()) }
			},
		},
		{
			name: "batch-5",
			addr: "127.0.0.1:19002",
			start: func() func() {
				srv := servers.StartNetHTTPBatchFlush(":19002", 5)
				return func() { srv.Shutdown(context.Background()) }
			},
		},
		{
			name: "batch-10",
			addr: "127.0.0.1:19003",
			start: func() func() {
				srv := servers.StartNetHTTPBatchFlush(":19003", 10)
				return func() { srv.Shutdown(context.Background()) }
			},
		},
		{
			name: "hijack+nodelay",
			addr: "127.0.0.1:19004",
			start: func() func() {
				srv := servers.StartNetHTTPHijack(":19004")
				return func() { srv.Shutdown(context.Background()) }
			},
		},
		{
			name: "hijack+batch5",
			addr: "127.0.0.1:19005",
			start: func() func() {
				srv := servers.StartNetHTTPHijackBatch(":19005", 5)
				return func() { srv.Shutdown(context.Background()) }
			},
		},
		{
			name: "prealloc",
			addr: "127.0.0.1:19006",
			start: func() func() {
				srv := servers.StartNetHTTPPrealloc(":19006")
				return func() { srv.Shutdown(context.Background()) }
			},
		},
		{
			name: "ultimate",
			addr: "127.0.0.1:19007",
			start: func() func() {
				srv := servers.StartNetHTTPUltimate(":19007", 5)
				return func() { srv.Shutdown(context.Background()) }
			},
		},
	}

	fmt.Println("==========================================================")
	fmt.Println("  SSE Optimization Benchmark")
	fmt.Println("  All based on net/http — testing optimization techniques")
	fmt.Println("==========================================================")
	fmt.Printf("  Go Version  : %s\n", runtime.Version())
	fmt.Printf("  GOMAXPROCS  : %d\n", runtime.GOMAXPROCS(0))
	fmt.Printf("  Concurrency : %d\n", *concurrency)
	fmt.Printf("  Total Reqs  : %d\n", *totalReqs)
	fmt.Printf("  Tokens/Req  : 50\n")
	fmt.Printf("  Token Delay : 5ms\n")
	fmt.Println("==========================================================")

	globalStart := time.Now()
	var results []bench.Result

	for _, tc := range cases {
		fmt.Printf("\n>>> [%s] starting on %s ...\n", tc.name, tc.addr)
		stop := tc.start()
		time.Sleep(500 * time.Millisecond)

		r := bench.Run(tc.name, tc.addr, *concurrency, *totalReqs)
		bench.PrintResult(r)
		results = append(results, r)

		stop()
		time.Sleep(500 * time.Millisecond)
	}

	bench.PrintComparison(results)
	fmt.Printf("\n  Total wall time: %s\n", time.Since(globalStart).Round(time.Millisecond))
	fmt.Println("\nDone.")
	os.Exit(0)
}
