// Command demo-zero-downtime shows what a Clever Cloud deployment does to live
// traffic. The home page tells which release and which instance answered; the
// control tower (/tower) sends a few requests per second from the browser and
// counts successes and errors while you deploy, break, roll back and scale.
// Standard library only.
package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

//go:embed static
var staticFS embed.FS

var (
	startTime  = time.Now()
	bootID     = newBootID()
	served     atomic.Int64 // answers of /api/whoami since start
	lastServed atomic.Int64 // unix ms of the last one
	healthHits atomic.Int64
	rel        = mustParseRelease(releaseJSON)
	pages      = template.Must(template.ParseFS(staticFS, "static/brand.tmpl", "static/index.html", "static/tower.html"))
)

// whoami is the answer the tower polls. Every field identifies who answered:
// the release compiled into the binary, the instance, the process, the commit.
type whoami struct {
	Version      string `json:"version"`
	Name         string `json:"name"`
	Color        string `json:"color"`
	Health       string `json:"health"`
	Boot         string `json:"boot"`
	Instance     string `json:"instance"`
	InstanceName string `json:"instance_name"`
	Commit       string `json:"commit"`
	BuiltFrom    string `json:"built_from"`
	Deployment   string `json:"deployment"`
	UptimeMs     int64  `json:"uptime_ms"`
	Served       int64  `json:"served"`
}

type pageData struct {
	Release   release
	Boot      string
	Uptime    string
	CC        platform
	BuiltFrom string
}

func main() {
	log.SetOutput(os.Stdout) // Clever Cloud collects stdout; one stream is enough
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.LUTC)

	cc := loadPlatform(os.Getenv)
	port := listenPort()
	srv := &http.Server{
		Addr:              "0.0.0.0:" + port,
		Handler:           newHandler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		// Never close an idle connection before the load balancer does (the doc gives
		// 180 s for Sōzu's backend operations). With 60 s we saw rare 502s answered by
		// the load balancer in 0 to 3 ms; with 10 min we saw none. A precaution, not a
		// proven cause: see "Rare 502s outside the switch-overs" in the README.
		IdleTimeout: 10 * time.Minute,
	}

	// SIGTERM is what Clever Cloud sends to the old instance once the new one
	// serves the traffic: finish the in-flight requests (10 s max), then exit.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("start: release %s (health %s), process %s, instance %q, listening on :%s",
			rel.Label(), rel.Health, bootID, cc.Instance(), port)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("server error: %v", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		stop()
		log.Printf("SIGTERM: release %s, process %s, served %d tower answers, the last one %s ago; draining (10 s max)",
			rel.Label(), bootID, served.Load(), sinceLast())
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown error: %v", err)
			os.Exit(1)
		}
		log.Printf("stopped: process %s", bootID)
	}
}

func sinceLast() string {
	ms := lastServed.Load()
	if ms == 0 {
		return "never"
	}
	return time.Since(time.UnixMilli(ms)).Round(10 * time.Millisecond).String()
}

// newHandler wires the routes. Method patterns answer 405 to other verbs and
// "/{$}" matches the root only, so any other path gets a 404.
func newHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", page("index.html"))
	mux.HandleFunc("GET /tower", page("tower.html"))
	mux.HandleFunc("GET /api/whoami", whoamiAPI)
	mux.HandleFunc("GET /health", health)
	mux.HandleFunc("GET /cc-brand.css", staticFile("cc-brand.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /app.css", staticFile("app.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /tower.js", staticFile("tower.js", "text/javascript; charset=utf-8"))
	return secure(mux)
}

// contentSecurityPolicy: same-origin scripts and fetches, Google Fonts, inline
// styles (the release color is set with style="--v:#…").
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; " +
	"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src https://fonts.gstatic.com; " +
	"img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'"

func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		next.ServeHTTP(w, r)
	})
}

func page(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Render into a buffer: a template error must give a clean 500.
		var buf bytes.Buffer
		err := pages.ExecuteTemplate(&buf, name, pageData{
			Release:   rel,
			Boot:      bootID,
			Uptime:    fmtUptime(time.Since(startTime)),
			CC:        loadPlatform(os.Getenv),
			BuiltFrom: cut(buildRevision(), 7),
		})
		if err != nil {
			log.Printf("template %s: %v", name, err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = buf.WriteTo(w)
	}
}

func whoamiAPI(w http.ResponseWriter, r *http.Request) {
	// ?fresh=1 ("new visitor" in the tower): close the connection after this
	// answer, so the load balancer picks an instance again for the next request.
	if r.URL.Query().Get("fresh") == "1" {
		w.Header().Set("Connection", "close")
	}
	n := served.Add(1)
	lastServed.Store(time.Now().UnixMilli())
	cc := loadPlatform(os.Getenv)
	writeJSON(w, http.StatusOK, whoami{
		Version:      rel.Version,
		Name:         rel.Name,
		Color:        rel.Color,
		Health:       rel.Health,
		Boot:         bootID,
		Instance:     cc.InstanceNumber,
		InstanceName: cc.InstanceName,
		Commit:       cc.Commit,
		BuiltFrom:    buildRevision(),
		Deployment:   cc.Deployment,
		UptimeMs:     time.Since(startTime).Milliseconds(),
		Served:       n,
	})
}

// health is the path set in CC_HEALTH_CHECK_PATH. Clever Cloud calls it during
// the deployment and switches traffic only on a 2xx. Each call is logged, so
// the deployment logs show how often the platform asked.
func health(w http.ResponseWriter, r *http.Request) {
	n := healthHits.Add(1)
	if !rel.Healthy() {
		log.Printf("health check #%d: 503, release %s fails its self-check on purpose", n, rel.Label())
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "fail", "release": rel.Label(), "reason": "self-check failed on purpose (release file: health=fail)",
		})
		return
	}
	log.Printf("health check #%d: 200, release %s", n, rel.Label())
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "release": rel.Label()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func staticFile(name, contentType string) http.HandlerFunc {
	body, err := staticFS.ReadFile("static/" + name)
	if err != nil {
		panic(err)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		// no-cache: the browser revalidates, so a rollback never mixes files
		// from two releases.
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(body)
	}
}

// fmtUptime: "42 s", "3 min 07 s", "2 h 05 min".
func fmtUptime(d time.Duration) string {
	sec := int(d.Seconds())
	switch {
	case sec < 60:
		return fmt.Sprintf("%d s", sec)
	case sec < 3600:
		return fmt.Sprintf("%d min %02d s", sec/60, sec%60)
	default:
		return fmt.Sprintf("%d h %02d min", sec/3600, sec%3600/60)
	}
}
