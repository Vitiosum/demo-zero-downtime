package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"unicode/utf8"
)

// A release is what changes from one deployment to the next. It is compiled into
// the binary, so each git commit carries exactly one release: rolling back to a
// commit brings its release back. The files in releases/ are the demo's three
// releases; copy one onto release.json, commit, deploy.
type release struct {
	Version string `json:"version"` // "v2"
	Name    string `json:"name"`    // "Bravo", from the aviation alphabet
	Color   string `json:"color"`   // "#11bea9", the release's color in the tower
	Health  string `json:"health"`  // "ok", or "fail" to make /health answer 503
	Note    string `json:"note"`    // one line shown on the pages
}

//go:embed release.json
var releaseJSON []byte

var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// parseRelease decodes and checks a release file. A typo must fail the build's
// tests, not show up as a broken page in production.
func parseRelease(b []byte) (release, error) {
	var r release
	if err := json.Unmarshal(b, &r); err != nil {
		return r, fmt.Errorf("release.json: %w", err)
	}
	switch {
	case r.Version == "" || utf8.RuneCountInString(r.Version) > 12:
		return r, fmt.Errorf("release.json: version must be 1 to 12 characters")
	case r.Name == "" || utf8.RuneCountInString(r.Name) > 24:
		return r, fmt.Errorf("release.json: name must be 1 to 24 characters")
	case !hexColor.MatchString(r.Color):
		return r, fmt.Errorf("release.json: color must look like #11bea9")
	case r.Health != "ok" && r.Health != "fail":
		return r, fmt.Errorf(`release.json: health must be "ok" or "fail"`)
	case utf8.RuneCountInString(r.Note) > 160:
		return r, fmt.Errorf("release.json: note must be 160 characters at most")
	}
	return r, nil
}

func mustParseRelease(b []byte) release {
	r, err := parseRelease(b)
	if err != nil {
		panic(err)
	}
	return r
}

// Healthy tells whether /health answers 200.
func (r release) Healthy() bool { return r.Health == "ok" }

// Label is "v2 Bravo", used in logs.
func (r release) Label() string { return r.Version + " " + r.Name }
