package hotspot

import (
	"bytes"
	"embed"
	"html/template"
	"net/http"

	"github.com/rs/zerolog/log"
)

//go:embed templates
var assets embed.FS

// pages holds the two walled-garden pages. Parsed at package init so a
// template syntax error panics at process startup rather than surfacing later
// as a broken page in front of a paying customer at a counter — the same
// trade-off internal/portalui makes, and `go test ./internal/hotspot/...`
// exercises this init on every run.
var pages = template.Must(template.New("portal").ParseFS(assets, "templates/*.html"))

type landingData struct {
	Session sessionRequest
	// Ready gates the forms. False when there is no usable MAC, since a
	// submission could not succeed and offering the form would waste the
	// visitor's time on a failure they cannot fix.
	Ready  bool
	Notice string
}

type grantedData struct {
	Session sessionRequest
}

func renderLanding(w http.ResponseWriter, status int, data landingData) {
	renderPage(w, status, "landing", data)
}

func renderGranted(w http.ResponseWriter, data grantedData) {
	renderPage(w, http.StatusOK, "granted", data)
}

// renderPage buffers the page before writing it.
//
// The status code has to go out before the body, and a template that fails
// halfway would otherwise leave a 200 already committed with a truncated page
// under it. Buffering keeps a render failure recoverable into a plain error
// response. These pages are a couple of kilobytes, so the copy costs nothing.
func renderPage(w http.ResponseWriter, status int, name string, data any) {
	var buf bytes.Buffer
	if err := pages.ExecuteTemplate(&buf, name, data); err != nil {
		log.Error().Err(err).Str("page", name).Msg("hotspot: template render failed")
		http.Error(w, "sign-in page unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// A captive-portal page must never be cached: the next device to hit this
	// URL is a different device, and a cached "you're connected" page shown to
	// someone who is not would leave them staring at a success screen with no
	// network and no form to retry from.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if _, err := w.Write(buf.Bytes()); err != nil {
		log.Debug().Err(err).Msg("hotspot: client went away mid-response")
	}
}
