// cmd/proxy/main.go
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"syscall"
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

func isWakeRequest(r *http.Request) bool {
	return r.Method == http.MethodPost && isWakePath(r.URL.Path)
}

func isWakePath(path string) bool {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 5 {
		return false
	}

	return parts[0] == "kubernetes" &&
		parts[1] == "project" &&
		parts[2] != "" &&
		parts[3] == "virtualcluster" &&
		parts[4] != ""
}

func isRetryableWakeError(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return isRetryableWakeError(urlErr.Err)
	}

	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return isRetryableWakeError(opErr.Err)
	}

	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno == syscall.ECONNRESET
	}

	return strings.Contains(strings.ToLower(err.Error()), "server closed idle connection")
}

func writeAcceptedWakeResponse(w http.ResponseWriter, note string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":       true,
		"accepted": true,
		"note":     note,
	})
}

func newProxyHandler(upstream string, client *http.Client, successOn map[int]struct{}, successOnError bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
			log.Printf("upstream %s %s error: %v", r.Method, targetURL, err)
			if successOnError && isWakeRequest(r) && isRetryableWakeError(err) {
				log.Printf("wake request %s error (%v) treated as accepted", targetURL, err)
				writeAcceptedWakeResponse(w, "wake request likely initiated; retryable upstream transport error treated as accepted")
				return
			}

			// default: propagate as 502 so caller may retry
			http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		log.Printf("upstream %s %s -> %s", r.Method, targetURL, resp.Status)

		if _, ok := successOn[resp.StatusCode]; ok && isWakeRequest(r) {
			log.Printf("wake request %s → %d (treated as accepted)", targetURL, resp.StatusCode)
			_, _ = io.Copy(io.Discard, resp.Body)
			writeAcceptedWakeResponse(w, "wake request likely initiated; retryable upstream status treated as accepted")
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
	}
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
	successOnError := mustEnv("SUCCESS_ON_ERROR", "false") == "true"

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

	http.HandleFunc("/", newProxyHandler(upstream, client, successOn, successOnError))

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

	log.Printf("proxy listening on %s → upstream %s (wake success on: %v, wake success on transport error: %v, timeout: %s)",
		addr, upstream, successOn, successOnError, timeout)
	log.Fatal(http.ListenAndServe(addr, nil))
}
