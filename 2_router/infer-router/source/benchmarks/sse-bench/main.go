package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"sse-bench/bench"
	"sse-bench/servers"
)

type framework struct {
	name  string
	start func() func() // returns a stop function
	h2    bool          // whether this is an h2c test
}

func main() {
	concurrency := flag.Int("c", 500, "concurrency level")
	rounds := flag.String("rounds", "1000,10000,100000", "comma-separated request counts for each round")
	flag.Parse()

	var reqCounts []int
	for s := range strings.SplitSeq(*rounds, ",") {
		s = strings.TrimSpace(s)
		var n int
		fmt.Sscanf(s, "%d", &n)
		if n > 0 {
			reqCounts = append(reqCounts, n)
		}
	}
	if len(reqCounts) == 0 {
		fmt.Println("No valid rounds specified")
		os.Exit(1)
	}

	// Define all frameworks to test
	frameworks := []framework{
		{
			name: "net/http",
			start: func() func() {
				srv := servers.StartNetHTTP(":18081")
				return func() { srv.Shutdown(context.Background()) }
			},
		},
		{
			name: "fasthttp",
			start: func() func() {
				srv := servers.StartFastHTTP(":18082")
				return func() { srv.Shutdown() }
			},
		},
		{
			name: "fiber",
			start: func() func() {
				srv := servers.StartFiber(":18083")
				return func() { srv.Shutdown() }
			},
		},
		{
			name: "hertz",
			start: func() func() {
				srv := servers.StartHertz("127.0.0.1:18084")
				return func() { srv.Shutdown() }
			},
		},
		{
			name: "net/http+h2c",
			h2:   true,
			start: func() func() {
				srv := servers.StartH2C(":18085")
				return func() { srv.Shutdown(context.Background()) }
			},
		},
	}

	// Port mapping for each framework
	addrs := []string{
		"127.0.0.1:18081",
		"127.0.0.1:18082",
		"127.0.0.1:18083",
		"127.0.0.1:18084",
		"127.0.0.1:18085",
	}

	fmt.Println("==========================================================")
	fmt.Println("  SSE Benchmark: net/http vs fasthttp vs fiber vs hertz")
	fmt.Println("              + HTTP/2 (h2c) comparison")
	fmt.Println("==========================================================")
	fmt.Printf("  Go Version  : %s\n", runtime.Version())
	fmt.Printf("  GOMAXPROCS  : %d\n", runtime.GOMAXPROCS(0))
	fmt.Printf("  Concurrency : %d\n", *concurrency)
	fmt.Printf("  Rounds      : %v\n", reqCounts)
	fmt.Printf("  Tokens/Req  : 50\n")
	fmt.Printf("  Token Delay : 5ms\n")
	fmt.Printf("  ConnPool    : MaxConnsPerHost = %d\n", *concurrency*2)
	fmt.Println("==========================================================")

	globalStart := time.Now()

	type roundResult struct {
		reqCount int
		results  []bench.Result
	}
	var allRounds []roundResult

	for ri, n := range reqCounts {
		fmt.Printf("\n\n##########################################################\n")
		fmt.Printf("  ROUND %d/%d — %d requests, concurrency %d\n", ri+1, len(reqCounts), n, *concurrency)
		fmt.Printf("##########################################################\n")

		roundStart := time.Now()
		var results []bench.Result

		for fi, fw := range frameworks {
			fmt.Printf("\n>>> [%s] starting on %s ...\n", fw.name, addrs[fi])
			stop := fw.start()
			time.Sleep(500 * time.Millisecond)

			var r bench.Result
			if fw.h2 {
				r = bench.RunH2C(fw.name, addrs[fi], *concurrency, n)
			} else {
				r = bench.Run(fw.name, addrs[fi], *concurrency, n)
			}
			bench.PrintResult(r)
			results = append(results, r)

			stop()
			time.Sleep(500 * time.Millisecond)
		}

		bench.PrintComparison(results)
		fmt.Printf("\n  Round %d wall time: %s\n", ri+1, time.Since(roundStart).Round(time.Millisecond))
		allRounds = append(allRounds, roundResult{reqCount: n, results: results})
	}

	// Final summary
	fmt.Printf("\n\n##########################################################\n")
	fmt.Printf("  FINAL CROSS-ROUND SUMMARY\n")
	fmt.Printf("##########################################################\n")
	for _, rr := range allRounds {
		fmt.Printf("\n--- %d requests (c=%d) ---\n", rr.reqCount, *concurrency)
		bench.PrintComparison(rr.results)
	}

	fmt.Printf("\n  Total wall time: %s\n", time.Since(globalStart).Round(time.Millisecond))
	fmt.Println("\nAll benchmarks complete.")
	os.Exit(0)
}
