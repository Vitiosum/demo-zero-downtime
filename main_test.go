package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	newHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func withRelease(t *testing.T, r release) {
	t.Helper()
	old := rel
	rel = r
	t.Cleanup(func() { rel = old })
}

func TestReleaseFiles(t *testing.T) {
	files, _ := filepath.Glob("releases/*.json")
	if len(files) != 3 {
		t.Fatalf("want 3 release files, got %d", len(files))
	}
	broken := 0
	for _, f := range append(files, "release.json") {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		r, err := parseRelease(b)
		if err != nil {
			t.Errorf("%s: %v", f, err)
		}
		if f != "release.json" && !r.Healthy() {
			broken++
		}
	}
	if broken != 1 {
		t.Errorf("want exactly one broken release in releases/, got %d", broken)
	}
}

func TestParseReleaseRejects(t *testing.T) {
	for _, s := range []string{
		`{`,
		`{"version":"","name":"A","color":"#112233","health":"ok"}`,
		`{"version":"v1","name":"A","color":"red","health":"ok"}`,
		`{"version":"v1","name":"A","color":"#112233","health":"maybe"}`,
	} {
		if _, err := parseRelease([]byte(s)); err == nil {
			t.Errorf("accepted %s", s)
		}
	}
}

func TestHealthFollowsRelease(t *testing.T) {
	withRelease(t, release{Version: "v2", Name: "Bravo", Color: "#11bea9", Health: "ok"})
	if rec := get(t, "/health"); rec.Code != http.StatusOK {
		t.Fatalf("healthy release: /health = %d", rec.Code)
	}
	withRelease(t, release{Version: "v3", Name: "Charlie", Color: "#fcb900", Health: "fail"})
	rec := get(t, "/health")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("broken release: /health = %d, want 503", rec.Code)
	}
	// The broken release still serves its pages: only the health check fails,
	// so a 503 seen by the tower can only come from the platform, not from it.
	if rec := get(t, "/api/whoami"); rec.Code != http.StatusOK {
		t.Fatalf("broken release: /api/whoami = %d, want 200", rec.Code)
	}
}

func TestWhoami(t *testing.T) {
	t.Setenv("INSTANCE_NUMBER", "1")
	t.Setenv("CC_PRETTY_INSTANCE_NAME", "Brave Otter")
	t.Setenv("CC_COMMIT_ID", "0123456789abcdef0123456789abcdef01234567")
	withRelease(t, release{Version: "v2", Name: "Bravo", Color: "#11bea9", Health: "ok"})
	rec := get(t, "/api/whoami")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	var w whoami
	if err := json.Unmarshal(rec.Body.Bytes(), &w); err != nil {
		t.Fatal(err)
	}
	if w.Name != "Bravo" || w.Version != "v2" || w.Color != "#11bea9" || w.Instance != "1" ||
		w.InstanceName != "Brave Otter" || len(w.Commit) != 40 || w.Boot != bootID || w.Served < 1 {
		t.Errorf("unexpected whoami: %+v", w)
	}
}

func TestFreshClosesConnection(t *testing.T) {
	if rec := get(t, "/api/whoami?fresh=1"); rec.Header().Get("Connection") != "close" {
		t.Errorf("?fresh=1 must answer Connection: close")
	}
	if rec := get(t, "/api/whoami"); rec.Header().Get("Connection") != "" {
		t.Errorf("default must keep the connection")
	}
}

func TestPages(t *testing.T) {
	for path, want := range map[string]string{
		"/":      "Ouvrir la tour de contrôle",
		"/tower": "Quatre scènes, la tour ouverte",
	} {
		rec := get(t, path)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s: %d, missing %q", path, rec.Code, want)
		}
		if !strings.Contains(rec.Body.String(), rel.Name) && path == "/" {
			t.Errorf("%s does not show the release name", path)
		}
		if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
			t.Errorf("%s: CSP = %q", path, csp)
		}
	}
	for _, p := range []string{"/cc-brand.css", "/app.css", "/tower.js"} {
		if rec := get(t, p); rec.Code != http.StatusOK {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
}

func TestUnknownRoutes(t *testing.T) {
	if rec := get(t, "/nope"); rec.Code != http.StatusNotFound {
		t.Errorf("/nope: %d", rec.Code)
	}
	rec := httptest.NewRecorder()
	newHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/whoami", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/whoami: %d", rec.Code)
	}
	if rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("security headers missing on 405")
	}
}
