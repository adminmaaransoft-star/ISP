// Package validate provides small, dependency-free input validators shared
// across request DTOs and dispatch boundaries — currently just E.164 phone
// number format, which was previously documented in field comments
// ("MobileNumber string // E.164") but never actually enforced anywhere.
package validate

import "regexp"

// e164Pattern is ITU-T E.164: a leading '+', then 2-15 digits total, the
// first of which is never 0 (a country code cannot start with 0).
var e164Pattern = regexp.MustCompile(`^\+[1-9]\d{1,14}$`)

// E164 reports whether s is a validly formatted E.164 phone number, e.g.
// "+919876543210".
func E164(s string) bool {
	return e164Pattern.MatchString(s)
}
