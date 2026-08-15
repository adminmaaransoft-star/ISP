package partner

import "net/http"

// SetHTTPClientForTest swaps the sender's SSRF-guarded client for a plain one.
//
// httptest servers bind to 127.0.0.1, which the guard blocks by design, so a
// delivery test would otherwise be unable to reach its own receiver. The guard
// is covered separately against real blocked addresses — keeping the two apart
// means each is tested for what it actually does rather than one masking the
// other.
func SetHTTPClientForTest(s *Sender, c *http.Client) { s.client = c }
