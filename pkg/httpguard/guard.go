package httpguard

import (
	"context"
	"net"
	"net/http"
)

const defaultLoopbackMessage = "access is restricted to loopback; set --allow-remote-ui to override"

type connectionNetworkKey struct{}

// GuardedMux wraps an http.ServeMux with loopback gating.
type GuardedMux struct {
	mux         *http.ServeMux
	allowRemote bool
	message     string
}

// NewGuardedMux constructs a GuardedMux using the supplied loopback policy.
func NewGuardedMux(mux *http.ServeMux, allowRemote bool, message string) GuardedMux {
	if message == "" {
		message = defaultLoopbackMessage
	}
	return GuardedMux{
		mux:         mux,
		allowRemote: allowRemote,
		message:     message,
	}
}

// Handle registers a pattern with loopback enforcement applied.
func (g GuardedMux) Handle(pattern string, h http.Handler) {
	if g.mux == nil {
		return
	}
	g.mux.Handle(pattern, g.guard(h))
}

// HandleFunc registers a pattern with loopback enforcement applied.
func (g GuardedMux) HandleFunc(pattern string, fn func(http.ResponseWriter, *http.Request)) {
	g.Handle(pattern, http.HandlerFunc(fn))
}

func (g GuardedMux) guard(next http.Handler) http.Handler {
	if g.allowRemote {
		return next
	}
	return LocalOnly(next, g.message)
}

// LocalOnly blocks non-loopback traffic with a configurable error message.
func LocalOnly(next http.Handler, message string) http.Handler {
	if message == "" {
		message = defaultLoopbackMessage
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !IsLoopbackRequest(r) {
			http.Error(w, message, http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// IsLoopbackRequest reports whether a request originates from loopback.
func IsLoopbackRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	if ConnectionNetwork(r.Context()) == "unix" {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

func WithConnectionNetwork(ctx context.Context, network string) context.Context {
	return context.WithValue(ctx, connectionNetworkKey{}, network)
}

func ConnectionNetwork(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	network, _ := ctx.Value(connectionNetworkKey{}).(string)
	return network
}

// WithShutdownContext merges the request context with a shutdown context.
// The handler will observe cancellation from either context.
func WithShutdownContext(next http.Handler, shutdownCtx context.Context) http.Handler {
	if shutdownCtx == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r == nil {
			next.ServeHTTP(w, r)
			return
		}
		ctx := MergeContexts(r.Context(), shutdownCtx)
		if ctx == r.Context() {
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// MergeContexts returns a context that is canceled when either input context is done.
func MergeContexts(primary context.Context, secondary context.Context) context.Context {
	if primary == nil {
		primary = context.Background()
	}
	if secondary == nil {
		return primary
	}
	ctx, cancel := context.WithCancel(primary)
	go func() {
		select {
		case <-secondary.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx
}
