package validate_test

import (
	"testing"

	"github.com/maaransoft/isp-bss-oss/pkg/validate"
)

func TestE164_Valid(t *testing.T) {
	valid := []string{
		"+919876543210",    // India, 10-digit subscriber number
		"+14155552671",     // US
		"+447911123456",    // UK
		"+8613800138000",   // China
		"+12",              // minimum length: 2 digits total
		"+123456789012345", // maximum length: 15 digits total
	}
	for _, s := range valid {
		if !validate.E164(s) {
			t.Errorf("E164(%q) = false, want true", s)
		}
	}
}

func TestE164_Invalid(t *testing.T) {
	invalid := []string{
		"",                   // empty
		"919876543210",       // missing leading +
		"+0123456789",        // leading zero after + (no valid country code starts with 0)
		"+91 9876543210",     // space
		"+91-9876543210",     // dash
		"+91.9876543210",     // dot
		"+1",                 // only 1 digit after + (below the 2-digit minimum)
		"+",                  // no digits at all
		"abc",                // not a phone number
		"9876543210",         // looks like a valid national number but has no country code
		"+91987654321012345", // 17 digits — over the 15-digit maximum
		"+ 919876543210",     // space between + and digits
		"++919876543210",     // doubled +
		"+91987654321O",      // letter O instead of digit 0
	}
	for _, s := range invalid {
		if validate.E164(s) {
			t.Errorf("E164(%q) = true, want false", s)
		}
	}
}
