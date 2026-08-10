// Command gen_jwt mints a signed JWT for local testing and load runs.
//
// It builds middleware.Claims directly, so a token it produces is exactly what
// the real JWT middleware expects — including the role and franchise bindings
// that row-level scoping depends on.
//
// Usage:
//
//	go run ./scripts/gen_jwt -secret "$JWT_SECRET" -role billing_admin
//	go run ./scripts/gen_jwt -secret "$JWT_SECRET" -role lco -franchise 1
//	go run ./scripts/gen_jwt -secret "$PORTAL_JWT_SECRET" -role subscriber -subscriber 1
//	go run ./scripts/gen_jwt -secret "$JWT_SECRET" -role noc_engineer -lea
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
)

func main() {
	var (
		secret     = flag.String("secret", "", "HMAC signing secret (required)")
		role       = flag.String("role", "billing_admin", "role claim")
		subject    = flag.String("subject", "loadtest", "subject claim")
		subscriber = flag.Int("subscriber", 0, "subscriber_id claim, for portal tokens")
		franchise  = flag.Int("franchise", 0, "franchise_id claim, for LCO tokens")
		ttl        = flag.Duration("ttl", time.Hour, "token lifetime")
		// Deliberately its own flag rather than something -role implies. SecD
		// §9.3 makes lea_access a claim independent of role precisely so that
		// LEA reach can never be granted as a side effect of a role
		// assignment, and a generator that inferred it from -role would
		// undermine the property it is meant to demonstrate.
		lea = flag.Bool("lea", false, "set the lea_access claim, required by /api/v1/lea/*")
	)
	flag.Parse()

	if *secret == "" {
		fmt.Fprintln(os.Stderr, "gen_jwt: -secret is required")
		os.Exit(1)
	}

	claims := middleware.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   *subject,
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(*ttl)),
		},
		Role:         *role,
		SubscriberID: *subscriber,
		FranchiseID:  *franchise,
		LeaAccess:    *lea,
	}

	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(*secret))
	if err != nil {
		fmt.Fprintf(os.Stderr, "gen_jwt: sign: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(token)
}
