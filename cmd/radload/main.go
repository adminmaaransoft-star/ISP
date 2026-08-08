// Command radload is a RADIUS authentication load generator.
//
// It exists because the tracker's NFR-PERF-001 row calls for "radperf", which is
// not a package that can actually be installed. This covers the same ground: a
// fixed request rate against the daemon, latency percentiles, and a pass/fail
// verdict against a p99 threshold.
//
// Single request:
//
//	radload -addr 127.0.0.1:1812 -secret testing123 -username u -password p
//
// Load test (NFR-PERF-001: p99 <= 15ms, error rate < 0.01%):
//
//	radload -addr 127.0.0.1:1812 -secret testing123 -users users.csv \
//	        -rate 5000 -duration 60s -p99 15ms
//
// The users file is one "username,password" per line.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"layeh.com/radius"
	"layeh.com/radius/rfc2865"
)

type credential struct {
	username string
	password string
}

type result struct {
	latency time.Duration
	code    radius.Code
	err     error
}

func main() {
	var (
		addr        = flag.String("addr", "127.0.0.1:1812", "RADIUS server address")
		secret      = flag.String("secret", "testing123", "RADIUS shared secret")
		username    = flag.String("username", "", "username for a single request")
		password    = flag.String("password", "", "password for a single request")
		usersFile   = flag.String("users", "", "CSV file of username,password for load mode")
		rate        = flag.Int("rate", 100, "requests per second")
		duration    = flag.Duration("duration", 10*time.Second, "test duration")
		concurrency = flag.Int("concurrency", 128, "in-flight requests (matches the daemon worker pool)")
		timeout     = flag.Duration("timeout", 5*time.Second, "per-request timeout")
		p99Limit    = flag.Duration("p99", 0, "fail if p99 exceeds this (0 disables the check)")
		errLimit    = flag.Float64("max-error-rate", 0.0001, "fail above this error rate (default 0.01%)")
	)
	flag.Parse()

	if *username != "" {
		if err := singleRequest(*addr, *secret, *username, *password, *timeout); err != nil {
			fmt.Fprintf(os.Stderr, "radload: %v\n", err)
			os.Exit(1)
		}
		return
	}

	creds, err := loadUsers(*usersFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "radload: %v\n", err)
		os.Exit(1)
	}

	if err := loadTest(*addr, *secret, creds, *rate, *duration, *concurrency, *timeout, *p99Limit, *errLimit); err != nil {
		fmt.Fprintf(os.Stderr, "radload: %v\n", err)
		os.Exit(1)
	}
}

// singleRequest sends one Access-Request and reports the response code.
func singleRequest(addr, secret, username, password string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	start := time.Now()
	code, err := auth(ctx, addr, []byte(secret), credential{username, password})
	elapsed := time.Since(start)
	if err != nil {
		return fmt.Errorf("request failed after %v: %w", elapsed, err)
	}

	fmt.Printf("response: %v  latency: %v\n", code, elapsed.Round(time.Microsecond))
	if code != radius.CodeAccessAccept {
		return fmt.Errorf("expected Access-Accept, got %v", code)
	}
	return nil
}

// auth performs one Access-Request/Response exchange.
func auth(ctx context.Context, addr string, secret []byte, c credential) (radius.Code, error) {
	packet := radius.New(radius.CodeAccessRequest, secret)
	if err := rfc2865.UserName_SetString(packet, c.username); err != nil {
		return 0, fmt.Errorf("set User-Name: %w", err)
	}
	if err := rfc2865.UserPassword_SetString(packet, c.password); err != nil {
		return 0, fmt.Errorf("set User-Password: %w", err)
	}

	response, err := radius.Exchange(ctx, packet, addr)
	if err != nil {
		return 0, err
	}
	return response.Code, nil
}

// loadTest drives a fixed request rate and reports latency percentiles.
func loadTest(addr, secret string, creds []credential, rate int, duration time.Duration,
	concurrency int, timeout, p99Limit time.Duration, errLimit float64,
) error {
	if rate <= 0 {
		return fmt.Errorf("rate must be positive")
	}
	if concurrency <= 0 {
		concurrency = 1
	}

	fmt.Printf("=== RADIUS load test (NFR-PERF-001) ===\n")
	fmt.Printf("target      : %s\n", addr)
	fmt.Printf("rate        : %d req/s\n", rate)
	fmt.Printf("duration    : %v\n", duration)
	fmt.Printf("concurrency : %d\n", concurrency)
	fmt.Printf("users       : %d\n", len(creds))
	if p99Limit > 0 {
		fmt.Printf("p99 limit   : %v\n", p99Limit)
	}
	fmt.Println()

	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	// Buffered generously so a slow consumer cannot distort the offered rate.
	jobs := make(chan credential, concurrency*2)
	results := make(chan result, rate*2)

	var (
		wg      sync.WaitGroup
		sent    atomic.Int64
		dropped atomic.Int64
	)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range jobs {
				reqCtx, reqCancel := context.WithTimeout(context.Background(), timeout)
				start := time.Now()
				code, err := auth(reqCtx, addr, []byte(secret), c)
				results <- result{latency: time.Since(start), code: code, err: err}
				reqCancel()
			}
		}()
	}

	// Producer: paces requests in small batches on a coarse tick.
	//
	// One tick per request does not work above roughly 2,000 req/s: a sub-500µs
	// Go ticker cannot be serviced at that granularity, so it silently coalesces
	// and delivers bursts. That caps the achieved rate well below the requested
	// one and inflates p99 by slamming every worker at once — measuring the
	// generator rather than the server. Releasing a batch every 10ms keeps the
	// tick well inside what the runtime can service.
	go func() {
		defer close(jobs)

		const tickInterval = 10 * time.Millisecond
		ticksPerSecond := int(time.Second / tickInterval)
		perTick := rate / ticksPerSecond
		if perTick < 1 {
			perTick = 1
		}
		// Whatever the integer division dropped is spread back across ticks so
		// the achieved rate matches the requested one.
		remainder := rate - perTick*ticksPerSecond

		ticker := time.NewTicker(tickInterval)
		defer ticker.Stop()

		next := 0
		tick := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				batch := perTick
				if tick%ticksPerSecond < remainder {
					batch++
				}
				tick++

				for i := 0; i < batch; i++ {
					c := creds[next%len(creds)]
					next++
					select {
					case jobs <- c:
						sent.Add(1)
					case <-ctx.Done():
						return
					default:
						// Every worker is busy. Counting this rather than
						// blocking keeps the offered rate honest: a blocked
						// producer would silently reduce load and flatter the
						// latency numbers.
						dropped.Add(1)
					}
				}
			}
		}
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	latencies := make([]time.Duration, 0, rate*int(duration.Seconds()+1))
	var accepts, rejects, errs int64
	for r := range results {
		if r.err != nil {
			errs++
			continue
		}
		latencies = append(latencies, r.latency)
		switch r.code {
		case radius.CodeAccessAccept:
			accepts++
		default:
			rejects++
		}
	}

	total := accepts + rejects + errs
	if total == 0 {
		return fmt.Errorf("no responses received — is the daemon listening on %s?", addr)
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

	errorRate := float64(errs) / float64(total)

	fmt.Printf("requests    : %d (accept %d, reject %d, error %d)\n", total, accepts, rejects, errs)
	fmt.Printf("offered     : %d, dropped by saturation: %d\n", sent.Load(), dropped.Load())
	fmt.Printf("throughput  : %.0f req/s\n", float64(total)/duration.Seconds())
	fmt.Printf("error rate  : %.4f%%\n", errorRate*100)
	if len(latencies) > 0 {
		fmt.Printf("latency p50 : %v\n", percentile(latencies, 50).Round(time.Microsecond))
		fmt.Printf("latency p95 : %v\n", percentile(latencies, 95).Round(time.Microsecond))
		fmt.Printf("latency p99 : %v\n", percentile(latencies, 99).Round(time.Microsecond))
		fmt.Printf("latency max : %v\n", latencies[len(latencies)-1].Round(time.Microsecond))
	}
	fmt.Println()

	failed := false
	if p99Limit > 0 && len(latencies) > 0 {
		p99 := percentile(latencies, 99)
		if p99 > p99Limit {
			fmt.Printf("FAIL: p99 %v exceeds the %v threshold\n", p99.Round(time.Microsecond), p99Limit)
			failed = true
		} else {
			fmt.Printf("PASS: p99 %v is within the %v threshold\n", p99.Round(time.Microsecond), p99Limit)
		}
	}
	if errorRate > errLimit {
		fmt.Printf("FAIL: error rate %.4f%% exceeds the %.4f%% threshold\n", errorRate*100, errLimit*100)
		failed = true
	}
	if failed {
		return fmt.Errorf("NFR-PERF-001 thresholds not met")
	}
	return nil
}

// percentile returns the p-th percentile of a sorted slice using nearest-rank.
func percentile(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := (p * len(sorted)) / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func loadUsers(path string) ([]credential, error) {
	if path == "" {
		return nil, fmt.Errorf("-users is required in load mode (or use -username for a single request)")
	}
	f, err := os.Open(path) //nolint:gosec // operator-supplied path
	if err != nil {
		return nil, fmt.Errorf("open users file: %w", err)
	}
	defer f.Close() //nolint:errcheck

	var creds []credential
	scanner := bufio.NewScanner(f)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		parts := strings.SplitN(text, ",", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("users file line %d: want \"username,password\", got %q", line, text)
		}
		creds = append(creds, credential{
			username: strings.TrimSpace(parts[0]),
			password: strings.TrimSpace(parts[1]),
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read users file: %w", err)
	}
	if len(creds) == 0 {
		return nil, fmt.Errorf("users file %s is empty", path)
	}
	return creds, nil
}
