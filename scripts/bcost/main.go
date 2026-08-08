// Command bcost measures bcrypt comparison cost.
//
// DDS §5.1 mandates cost 12 for password hashes, while NFR-PERF-001 demands a
// 15ms p99 at 5,000 authentications per second. Those two requirements interact,
// and this reports the per-core ceiling each cost implies so the trade-off is
// based on a measurement rather than an assumption.
//
// Usage: go run ./scripts/bcost
package main

import (
	"fmt"
	"runtime"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func main() {
	const sample = "TestPass1234!"
	fmt.Printf("GOMAXPROCS: %d\n\n", runtime.GOMAXPROCS(0))
	fmt.Printf("%-6s %-14s %-18s %s\n", "cost", "compare", "per core", "at 8 cores")

	for _, cost := range []int{4, 8, 10, 12} {
		hash, err := bcrypt.GenerateFromPassword([]byte(sample), cost)
		if err != nil {
			fmt.Printf("cost %2d: %v\n", cost, err)
			continue
		}

		const iterations = 5
		start := time.Now()
		for i := 0; i < iterations; i++ {
			_ = bcrypt.CompareHashAndPassword(hash, []byte(sample)) //nolint:errcheck
		}
		per := time.Since(start) / iterations
		perCore := 1.0 / per.Seconds()

		fmt.Printf("%-6d %-14v %-18.0f %.0f auth/s\n",
			cost, per.Round(time.Microsecond), perCore, perCore*8)
	}
}
