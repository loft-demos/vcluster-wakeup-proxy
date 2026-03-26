package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func newTestClient(fn roundTripFunc) *http.Client {
	return &http.Client{Transport: fn}
}

func TestShouldDumpRequestSkipsConfiguredUserAgents(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	req.Header.Set("User-Agent", "kube-probe/1.33")

	if shouldDumpRequest(req, parseList("kube-probe")) {
		t.Fatal("expected kube-probe request to be skipped")
	}
}

func TestShouldDumpRequestAllowsOtherUserAgents(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	req.Header.Set("User-Agent", "curl/8.7.1")

	if !shouldDumpRequest(req, parseList("kube-probe")) {
		t.Fatal("expected non-probe request to be logged")
	}
}

func TestParseListTrimsAndNormalizesEntries(t *testing.T) {
	got := parseList(" kube-probe , Prometheus ")
	want := []string{"kube-probe", "prometheus"}

	if len(got) != len(want) {
		t.Fatalf("expected %d items, got %d (%v)", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected %v, got %v", want, got)
		}
	}
}

func TestWakeRequestTreatsConfiguredStatusAsAccepted(t *testing.T) {
	handler := newProxyHandler("http://upstream", newTestClient(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusGatewayTimeout,
			Status:     "504 Gateway Timeout",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("timeout")),
		}, nil
	}), parseOverrideSet("502,504"), false)

	req := httptest.NewRequest(http.MethodPost, "/kubernetes/project/demo/virtualcluster/team-a", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"accepted":true`) {
		t.Fatalf("expected accepted response body, got %q", rec.Body.String())
	}
}

func TestWakeRequestRewritesSuccessfulResponseAsAcceptedJSON(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		status     string
	}{
		{name: "200 OK", statusCode: http.StatusOK, status: "200 OK"},
		{name: "202 Accepted", statusCode: http.StatusAccepted, status: "202 Accepted"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := newProxyHandler("http://upstream", newTestClient(func(*http.Request) (*http.Response, error) {
				header := make(http.Header)
				header.Set("Content-Type", "application/json")
				header.Set("X-Upstream", "set")
				return &http.Response{
					StatusCode: tt.statusCode,
					Status:     tt.status,
					Header:     header,
					Body:       io.NopCloser(strings.NewReader("")),
				}, nil
			}), parseOverrideSet("502,504"), false)

			req := httptest.NewRequest(http.MethodPost, "/kubernetes/project/demo/virtualcluster/team-a", nil)
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected synthetic 200, got %d", rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("expected synthetic JSON content type, got %q", got)
			}
			if got := rec.Header().Get("X-Upstream"); got != "" {
				t.Fatalf("expected upstream headers to be discarded, got %q", got)
			}
			if !strings.Contains(rec.Body.String(), `"accepted":true`) {
				t.Fatalf("expected accepted response body, got %q", rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), `"note":"wake request accepted"`) {
				t.Fatalf("expected wake success note, got %q", rec.Body.String())
			}
		})
	}
}

func TestNonWakeRequestPassesConfiguredStatusThrough(t *testing.T) {
	handler := newProxyHandler("http://upstream", newTestClient(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusGatewayTimeout,
			Status:     "504 Gateway Timeout",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("timeout")),
		}, nil
	}), parseOverrideSet("502,504"), false)

	req := httptest.NewRequest(http.MethodGet, "/kubernetes/project/demo/virtualcluster/team-a", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 504, got %d", rec.Code)
	}
	if rec.Body.String() != "timeout" {
		t.Fatalf("expected upstream body to pass through, got %q", rec.Body.String())
	}
}

func TestNonWakeRequestPassesSuccessfulResponseThrough(t *testing.T) {
	handler := newProxyHandler("http://upstream", newTestClient(func(*http.Request) (*http.Response, error) {
		header := make(http.Header)
		header.Set("Content-Type", "application/json")
		header.Set("X-Upstream", "set")
		return &http.Response{
			StatusCode: http.StatusAccepted,
			Status:     "202 Accepted",
			Header:     header,
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	}), parseOverrideSet("502,504"), false)

	req := httptest.NewRequest(http.MethodGet, "/kubernetes/project/demo/virtualcluster/team-a", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", rec.Code)
	}
	if got := rec.Header().Get("X-Upstream"); got != "set" {
		t.Fatalf("expected upstream headers to pass through, got %q", got)
	}
	if rec.Body.String() != "" {
		t.Fatalf("expected empty upstream body to pass through, got %q", rec.Body.String())
	}
}

func TestWakeRequestKeepsPermanentErrorsVisible(t *testing.T) {
	handler := newProxyHandler("http://upstream", newTestClient(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Status:     "403 Forbidden",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("forbidden")),
		}, nil
	}), parseOverrideSet("502,504"), true)

	req := httptest.NewRequest(http.MethodPost, "/kubernetes/project/demo/virtualcluster/team-a", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
	if rec.Body.String() != "forbidden" {
		t.Fatalf("expected upstream body to pass through, got %q", rec.Body.String())
	}
}

func TestWakeRequestTreatsRetryableTransportErrorAsAccepted(t *testing.T) {
	handler := newProxyHandler("http://upstream", newTestClient(func(*http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Post", URL: "http://upstream", Err: io.ErrUnexpectedEOF}
	}), parseOverrideSet("502,504"), true)

	req := httptest.NewRequest(http.MethodPost, "/kubernetes/project/demo/virtualcluster/team-a", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"accepted":true`) {
		t.Fatalf("expected accepted response body, got %q", rec.Body.String())
	}
}

func TestWakeRequestDoesNotHideNonRetryableTransportError(t *testing.T) {
	handler := newProxyHandler("http://upstream", newTestClient(func(*http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Post", URL: "http://upstream", Err: errors.New("no such host")}
	}), parseOverrideSet("502,504"), true)

	req := httptest.NewRequest(http.MethodPost, "/kubernetes/project/demo/virtualcluster/team-a", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no such host") {
		t.Fatalf("expected transport error to be visible, got %q", rec.Body.String())
	}
}
