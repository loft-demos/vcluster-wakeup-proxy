// cmd/proxy/main.go
package main

import (
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"os"
	"strings"
	"time"
)

func mustEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseOverrideSet(v string) map[int]struct{} {
	set := map[int]struct{}{}
	if v == "" {
		return set
	}
	for _, s := range strings.Split(v, ",") {
		switch strings.TrimSpace(s) {
		case "502":
			set[502] = struct{}{}
		case "504":
			set[504] = struct{}{}
		case "500":
			set[500] = struct{}{}
		case "429":
			set[429] = struct{}{}
		}
	}
	return set
}

func main() {
	upstream := os.Getenv("UPSTREAM_BASE") // e.g. https://romeo.us.demo.dev
	if upstream == "" {
		log.Fatal("UPSTREAM_BASE is required")
	}
	successOn := parseOverrideSet(mustEnv("SUCCESS_ON", "502,504"))
	addr := mustEnv("LISTEN_ADDR", ":8080")
	timeout := time.Duration(10) * time.Second
	if t := os.Getenv("UPSTREAM_TIMEOUT"); t != "" {
		if d, err := time.ParseDuration(t); err == nil {
			timeout = d
		}
	}

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		IdleConnTimeout:       30 * time.Second,
	}
	client := &http.Client{Transport: transport, Timeout: timeout}

	// Basic health endpoints
	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	http.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })

	// Catch-all proxy
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		targetURL := strings.TrimRight(upstream, "/") + r.URL.Path
		if r.URL.RawQuery != "" {
			targetURL += "?" + r.URL.RawQuery
		}

		req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
		if err != nil {
			http.Error(w, "bad upstream request", http.StatusBadGateway)
			return
		}

		// Copy headers (minus hop-by-hop)
		hopByHop := map[string]struct{}{
			"Connection":          {},
			"Proxy-Connection":    {},
			"Keep-Alive":          {},
			"Proxy-Authenticate":  {},
			"Proxy-Authorization": {},
			"Te":                  {},
			"Trailer":             {},
			"Transfer-Encoding":   {},
			"Upgrade":             {},
		}
		for k, vv := range r.Header {
			if _, skip := hopByHop[http.CanonicalHeaderKey(k)]; skip {
				continue
			}
			for _, v := range vv {
				req.Header.Add(k, v)
			}
		}

		resp, err := client.Do(req)
		if err != nil {
			// Upstream unreachable/network error → propagate as 502 (so Argo may retry)
			http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		// If status is in SUCCESS_ON, swallow it and return 200
		if _, ok := successOn[resp.StatusCode]; ok {
      log.Printf("upstream %s → %d (treated as success)", targetURL, resp.StatusCode)
			// drain body (optional)
			_, _ = io.Copy(io.Discard, resp.Body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true,"note":"status ` + resp.Status + ` treated as success"}`))
			return
		}

		// Otherwise, pass through
		for k, vv := range resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		// stream body
		if _, err := io.Copy(w, resp.Body); err != nil {
			// non-fatal
			log.Printf("stream error: %v", err)
		}
	})

	dump := mustEnv("LOG_REQUESTS", "false") == "true"
	if dump {
		orig := http.DefaultServeMux
		http.DefaultServeMux = http.NewServeMux()
		http.DefaultServeMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if b, err := httputil.DumpRequest(r, true); err == nil {
				log.Printf("REQ:\n%s\n", string(b))
			}
			orig.ServeHTTP(w, r)
		})
	}

	log.Printf("proxy listening on %s → upstream %s (success on: %v, timeout: %s)",
		addr, upstream, successOn, timeout)
	log.Fatal(http.ListenAndServe(addr, nil))
}
