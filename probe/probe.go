//go:build ignore

// Probe: the control tower for the terminal. It sends a few GET /api/whoami per
// second, writes one CSV line per request and prints what changes: a new
// process, a process that stops answering, every error. Ctrl-C prints the
// summary the README numbers come from.
//
//	go run probe/probe.go -url https://app-xxx.cleverapps.io -rps 2 -out run.csv
//	go run probe/probe.go -url https://app-xxx.cleverapps.io -rps 1 -fresh -out fresh.csv
package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type answer struct {
	Version      string `json:"version"`
	Name         string `json:"name"`
	Boot         string `json:"boot"`
	Instance     string `json:"instance"`
	InstanceName string `json:"instance_name"`
	Commit       string `json:"commit"`
	BuiltFrom    string `json:"built_from"`
	Deployment   string `json:"deployment"`
}

type proc struct {
	answer
	first, last time.Time
	n           int
	gone        bool
}

type errorSpan struct {
	label       string
	first, last time.Time
	n           int
}

func main() {
	base := flag.String("url", "", "app URL, e.g. https://app-xxx.cleverapps.io")
	rps := flag.Float64("rps", 2, "requests per second (keep it light)")
	out := flag.String("out", "probe.csv", "CSV file, one line per request")
	dur := flag.Duration("for", 0, "stop after this duration (0 = until Ctrl-C)")
	fresh := flag.Bool("fresh", false, "open a new connection for every request (no keep-alive), like a new visitor")
	flag.Parse()
	if *base == "" || *rps <= 0 || *rps > 10 {
		fmt.Fprintln(os.Stderr, "usage: go run probe/probe.go -url https://app-xxx.cleverapps.io [-rps 2] [-fresh] [-out probe.csv] [-for 10m]")
		os.Exit(2)
	}
	target := strings.TrimRight(*base, "/") + "/api/whoami"

	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	_ = w.Write([]string{"sent_utc", "latency_ms", "status", "error", "version", "name", "boot", "instance", "instance_name", "commit", "deployment"})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *dur > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *dur)
		defer cancel()
	}

	// By default the probe keeps its connection open, like a browser tab: the
	// load balancer then keeps sending it to the same instance. -fresh shows
	// where a new visitor lands.
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{DisableKeepAlives: *fresh}}
	var (
		mu     sync.Mutex
		wg     sync.WaitGroup
		procs  = map[string]*proc{}
		order  []string
		sent   int
		ok     int
		spans  []*errorSpan
		lastOK time.Time
	)
	say := func(t time.Time, format string, a ...any) {
		fmt.Printf("%s  %s\n", t.UTC().Format("15:04:05.000"), fmt.Sprintf(format, a...))
	}

	record := func(t0 time.Time, lat time.Duration, status int, errLabel string, a answer) {
		mu.Lock()
		defer mu.Unlock()
		now := time.Now()
		_ = w.Write([]string{t0.UTC().Format(time.RFC3339Nano), strconv.FormatInt(lat.Milliseconds(), 10), strconv.Itoa(status), errLabel,
			a.Version, a.Name, a.Boot, a.Instance, a.InstanceName, a.Commit, a.Deployment})
		if errLabel != "" {
			if n := len(spans); n > 0 && spans[n-1].label == errLabel && now.Sub(spans[n-1].last) < 2*time.Second {
				spans[n-1].last, spans[n-1].n = now, spans[n-1].n+1
			} else {
				spans = append(spans, &errorSpan{label: errLabel, first: now, last: now, n: 1})
			}
			say(now, "ERROR %s (request sent at %s)", errLabel, t0.UTC().Format("15:04:05.000"))
			return
		}
		ok++
		lastOK = now
		p := procs[a.Boot]
		if p == nil {
			p = &proc{answer: a, first: now}
			procs[a.Boot] = p
			order = append(order, a.Boot)
			say(now, "NEW   process %s answers: %s %s, instance #%s %s, commit %.7s", a.Boot, a.Version, a.Name, a.Instance, a.InstanceName, firstOf(a.Commit, a.BuiltFrom))
		}
		p.last, p.n = now, p.n+1
		if p.gone {
			p.gone = false
			say(now, "BACK  process %s (%s) answers again", p.Boot, p.Name)
		}
	}

	sweep := func() {
		mu.Lock()
		defer mu.Unlock()
		now := time.Now()
		for _, b := range order {
			p := procs[b]
			if !p.gone && lastOK.Sub(p.last) > 15*time.Second {
				p.gone = true
				say(now, "GONE  process %s (%s %s) stopped answering; last answer at %s", p.Boot, p.Version, p.Name, p.last.UTC().Format("15:04:05.000"))
			}
		}
	}

	tick := time.NewTicker(time.Duration(float64(time.Second) / *rps))
	defer tick.Stop()
	sec := time.NewTicker(time.Second)
	defer sec.Stop()
	mode := "keep-alive connection"
	if *fresh {
		mode = "new connection per request"
	}
	fmt.Printf("probe: GET %s, %.1f req/s, %s, CSV in %s (times are UTC)\n", target, *rps, mode, *out)
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-sec.C:
			sweep()
		case <-tick.C:
			mu.Lock()
			sent++
			mu.Unlock()
			wg.Add(1)
			go func(t0 time.Time) {
				defer wg.Done()
				req, _ := http.NewRequest(http.MethodGet, target, nil)
				req.Header.Set("Cache-Control", "no-cache")
				resp, err := client.Do(req)
				lat := time.Since(t0)
				if err != nil {
					label := "network error"
					if strings.Contains(err.Error(), "Timeout") || strings.Contains(err.Error(), "deadline") {
						label = "no answer in 3 s"
					}
					record(t0, lat, 0, label, answer{})
					return
				}
				defer resp.Body.Close()
				var a answer
				if resp.StatusCode/100 != 2 {
					record(t0, lat, resp.StatusCode, "HTTP "+strconv.Itoa(resp.StatusCode), a)
					return
				}
				if json.NewDecoder(resp.Body).Decode(&a) != nil || a.Boot == "" {
					record(t0, lat, resp.StatusCode, "unreadable answer", a)
					return
				}
				record(t0, lat, resp.StatusCode, "", a)
			}(time.Now())
		}
	}
	wg.Wait()
	w.Flush()

	// Summary
	errs := 0
	for _, s := range spans {
		errs += s.n
	}
	fmt.Printf("\nsummary: %d sent, %d ok, %d errors\n", sent, ok, errs)
	sort.SliceStable(order, func(i, j int) bool { return procs[order[i]].first.Before(procs[order[j]].first) })
	for _, b := range order {
		p := procs[b]
		fmt.Printf("  process %s  %-3s %-8s instance #%s %-24s commit %.7s  %5d answers  %s → %s\n", p.Boot, p.Version, p.Name, p.Instance, p.InstanceName,
			firstOf(p.Commit, p.BuiltFrom), p.n, p.first.UTC().Format("15:04:05.000"), p.last.UTC().Format("15:04:05.000"))
	}
	for _, s := range spans {
		fmt.Printf("  errors: %d × %s between %s and %s\n", s.n, s.label, s.first.UTC().Format("15:04:05.000"), s.last.UTC().Format("15:04:05.000"))
	}
}

func firstOf(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return "-"
}
