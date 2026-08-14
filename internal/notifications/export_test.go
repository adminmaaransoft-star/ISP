package notifications

// BuildMessageForTest exposes buildMessage to the external notifications_test
// package.
//
// Header assembly stays unexported — it is an implementation detail of the
// SMTP client, not part of the package's API — but the CRLF-stripping in the
// subject line is a header-injection defense, and a defense with no test is
// one nobody notices losing.
func BuildMessageForTest(from, to, subject, body string) string {
	return buildMessage(from, to, subject, body)
}
