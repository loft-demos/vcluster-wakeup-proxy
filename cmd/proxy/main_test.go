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
