package httpguard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIsLoopbackRequest(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name       string
		remoteAddr string
		want       bool
	}{
		{name: "ipv4 loopback", remoteAddr: "127.0.0.1:1234", want: true},
		{name: "ipv6 loopback", remoteAddr: "[::1]:443", want: true},
		{name: "non loopback", remoteAddr: "10.1.2.3:443", want: false},
		{name: "invalid remote addr", remoteAddr: "malformed", want: false},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, "http://example.test", nil)
			req.RemoteAddr = tc.remoteAddr
			if got := IsLoopbackRequest(req); got != tc.want {
				t.Fatalf("IsLoopbackRequest() = %v, want %v", got, tc.want)
			}
		})
	}

	if got := IsLoopbackRequest(nil); got {
		t.Fatalf("IsLoopbackRequest(nil) = true, want false")
	}
}

func TestLocalOnly(t *testing.T) {
	t.Parallel()

	t.Run("rejects non loopback with default message", func(t *testing.T) {
		t.Parallel()

		called := false
		handler := LocalOnly(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			called = true
			w.WriteHeader(http.StatusNoContent)
		}), "")

		req := httptest.NewRequest(http.MethodGet, "http://example.test", nil)
		req.RemoteAddr = "192.168.1.10:5555"
		resp := httptest.NewRecorder()

		handler.ServeHTTP(resp, req)

		if called {
			t.Fatalf("next handler should not be called for remote request")
		}
		if resp.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want %d", resp.Code, http.StatusForbidden)
		}
		if got := resp.Body.String(); got != defaultLoopbackMessage+"\n" {
			t.Fatalf("body = %q, want %q", got, defaultLoopbackMessage+"\n")
		}
	})

	t.Run("allows loopback", func(t *testing.T) {
		t.Parallel()

		called := false
		handler := LocalOnly(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			called = true
			w.WriteHeader(http.StatusNoContent)
		}), "custom")

		req := httptest.NewRequest(http.MethodGet, "http://example.test", nil)
		req.RemoteAddr = "127.0.0.1:9999"
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)

		if !called {
			t.Fatalf("next handler should be called for loopback request")
		}
		if resp.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want %d", resp.Code, http.StatusNoContent)
		}
	})
}

func TestGuardedMuxHandleFunc(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	guarded := NewGuardedMux(mux, false, "loopback only")
	guarded.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	remoteReq := httptest.NewRequest(http.MethodGet, "http://example.test/healthz", nil)
	remoteReq.RemoteAddr = "10.0.0.5:1234"
	remoteResp := httptest.NewRecorder()
	mux.ServeHTTP(remoteResp, remoteReq)
	if remoteResp.Code != http.StatusForbidden {
		t.Fatalf("remote status = %d, want %d", remoteResp.Code, http.StatusForbidden)
	}

	loopbackReq := httptest.NewRequest(http.MethodGet, "http://example.test/healthz", nil)
	loopbackReq.RemoteAddr = "127.0.0.1:1234"
	loopbackResp := httptest.NewRecorder()
	mux.ServeHTTP(loopbackResp, loopbackReq)
	if loopbackResp.Code != http.StatusNoContent {
		t.Fatalf("loopback status = %d, want %d", loopbackResp.Code, http.StatusNoContent)
	}
}

func TestWithShutdownContextCancelsRequestContext(t *testing.T) {
	t.Parallel()

	shutdownCtx, cancelShutdown := context.WithCancel(context.Background())
	defer cancelShutdown()

	requestObservedCancel := make(chan struct{}, 1)
	handlerDone := make(chan struct{}, 1)

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		requestObservedCancel <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
		handlerDone <- struct{}{}
	})

	wrapped := WithShutdownContext(next, shutdownCtx)

	req := httptest.NewRequest(http.MethodGet, "http://example.test", nil)
	req.RemoteAddr = "127.0.0.1:2222"
	resp := httptest.NewRecorder()

	go wrapped.ServeHTTP(resp, req)
	cancelShutdown()

	select {
	case <-requestObservedCancel:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("request context was not canceled by shutdown context")
	}

	select {
	case <-handlerDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("wrapped handler did not complete")
	}
}
