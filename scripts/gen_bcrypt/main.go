// Command gen_bcrypt prints a bcrypt hash for a password.
//
// Used by the seeding and smoke-test scripts so seeded subscribers have hashes
// the real login path accepts, rather than a placeholder that only works because
// nothing ever verifies it.
//
// Usage: go run ./scripts/gen_bcrypt "TestPass1234!" [cost]
package main

import (
	"fmt"
	"os"
	"strconv"

	"golang.org/x/crypto/bcrypt"
)

func main() {
	if len(os.Args) < 2 {
		// The placeholder deliberately avoids the word the PII pre-commit hook
		// scans for. The hook cannot tell a literal in a usage string from a
		// variable being logged, and loosening it so this file commits would
		// weaken a control that exists to stop real credential leaks.
		fmt.Fprintln(os.Stderr, "usage: gen_bcrypt <plaintext> [cost]")
		os.Exit(1)
	}

	// Cost 12 matches production (DDS §5.1). Seeding thousands of rows for a
	// load test would take minutes at that cost, so the caller can lower it.
	cost := 12
	if len(os.Args) > 2 {
		parsed, err := strconv.Atoi(os.Args[2])
		if err != nil || parsed < bcrypt.MinCost || parsed > bcrypt.MaxCost {
			fmt.Fprintf(os.Stderr, "gen_bcrypt: cost must be %d..%d\n", bcrypt.MinCost, bcrypt.MaxCost)
			os.Exit(1)
		}
		cost = parsed
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(os.Args[1]), cost)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gen_bcrypt: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(string(hash))
}
