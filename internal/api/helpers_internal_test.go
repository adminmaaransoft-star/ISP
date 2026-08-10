package api

import (
	"errors"
	"net/url"
	"testing"
)

// White-box (package api, not api_test): isUniqueViolation, contains, and
// containsStr are internal implementation details of CreateSubscriber's
// 409-vs-500 error classification, not part of the package's intended
// public surface, so they stay unexported and are tested from inside the
// package rather than exported just to make them reachable from api_test.

func TestIsUniqueViolation(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"unique constraint message", errors.New(`ERROR: duplicate key value violates unique constraint "subscribers_username_key"`), true},
		{"duplicate wording without 'unique'", errors.New("duplicate entry"), true},
		{"unrelated error", errors.New("connection refused"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUniqueViolation(tc.err); got != tc.want {
				t.Errorf("isUniqueViolation(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestContains(t *testing.T) {
	cases := []struct {
		s, sub string
		want   bool
	}{
		{"", "", true},
		{"hello world", "world", true},
		{"hello world", "hello", true},
		{"hello world", "lo wo", true},
		{"hello world", "xyz", false},
		{"short", "much longer substring", false},
		{"exact", "exact", true},
	}
	for _, tc := range cases {
		if got := contains(tc.s, tc.sub); got != tc.want {
			t.Errorf("contains(%q, %q) = %v, want %v", tc.s, tc.sub, got, tc.want)
		}
	}
}

func TestContainsStr(t *testing.T) {
	cases := []struct {
		s, sub string
		want   bool
	}{
		{"hello world", "world", true},
		{"hello world", "xyz", false},
		{"", "x", false},
		{"x", "", true},
	}
	for _, tc := range cases {
		if got := containsStr(tc.s, tc.sub); got != tc.want {
			t.Errorf("containsStr(%q, %q) = %v, want %v", tc.s, tc.sub, got, tc.want)
		}
	}
}

func TestParseTimeRange(t *testing.T) {
	t.Run("both from and to parse", func(t *testing.T) {
		q := url.Values{"from": {"2026-01-01T00:00:00Z"}, "to": {"2026-01-31T23:59:59Z"}}
		from, to, err := parseTimeRange(q)
		if err != nil {
			t.Fatalf("parseTimeRange: %v", err)
		}
		if from == nil || to == nil {
			t.Fatalf("want both from and to populated, got from=%v to=%v", from, to)
		}
	})

	t.Run("neither present returns nil, nil, nil", func(t *testing.T) {
		from, to, err := parseTimeRange(url.Values{})
		if err != nil || from != nil || to != nil {
			t.Errorf("want (nil, nil, nil), got (%v, %v, %v)", from, to, err)
		}
	})

	t.Run("malformed from is rejected", func(t *testing.T) {
		_, _, err := parseTimeRange(url.Values{"from": {"not-a-date"}})
		if err == nil {
			t.Error("expected an error for a non-RFC3339 'from'")
		}
	})

	t.Run("malformed to is rejected", func(t *testing.T) {
		_, _, err := parseTimeRange(url.Values{"to": {"not-a-date"}})
		if err == nil {
			t.Error("expected an error for a non-RFC3339 'to'")
		}
	})
}

func TestParseLimit(t *testing.T) {
	const def, max = 50, 200

	cases := []struct {
		name  string
		limit string
		want  int
	}{
		{"empty falls back to default", "", def},
		{"valid value within range", "75", 75},
		{"zero falls back to default", "0", def},
		{"negative-looking (non-digit) falls back to default", "-5", def},
		{"non-numeric falls back to default", "abc", def},
		{"exceeds max is clamped", "9999", max},
		{"exactly max is kept", "200", max},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := url.Values{"limit": {tc.limit}}
			got := parseLimit(q, def, max)
			if got != tc.want {
				t.Errorf("parseLimit(%q): want %d, got %d", tc.limit, tc.want, got)
			}
		})
	}
}
