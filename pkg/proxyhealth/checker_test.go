package proxyhealth

import (
	"bufio"
	"encoding/base64"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.openai.org/api/tunnel-client/pkg/config"
	"go.openai.org/api/tunnel-client/pkg/proxy"
)

func TestRecordResultHistoryRetention(t *testing.T) {
	checker, route := newTestChecker(t)
	for i := 0; i < maxHistoryEntries+2; i++ {
		record := CheckRecord{Timestamp: time.Now().Add(time.Duration(i) * time.Second)}
		checker.recordResult(route, record, i%2 == 0)
	}
	summaries := checker.HealthSummaries()
	if len(summaries) != 1 {
		t.Fatalf("expected 1 summary, got %d", len(summaries))
	}
	if len(summaries[0].History) != maxHistoryEntries {
		t.Fatalf("expected %d history entries, got %d", maxHistoryEntries, len(summaries[0].History))
	}
}

func TestRecordResultStateTransitions(t *testing.T) {
	checker, route := newTestChecker(t)
	checker.recordResult(route, CheckRecord{Timestamp: time.Now()}, false)
	state := checker.HealthSummaries()[0].HealthState
	if state != string(HealthStateUnhealthy) {
		t.Fatalf("expected unhealthy, got %s", state)
	}
	checker.recordResult(route, CheckRecord{Timestamp: time.Now()}, true)
	state = checker.HealthSummaries()[0].HealthState
	if state != string(HealthStateHealthy) {
		t.Fatalf("expected healthy, got %s", state)
	}
}

func TestConnectThroughProxyIncludesProxyAuthorization(t *testing.T) {
	t.Parallel()

	clientConn, proxyConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = proxyConn.Close()
	})

	requestLines := make(chan string, 1)
	go func() {
		reader := bufio.NewReader(proxyConn)
		var builder strings.Builder
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			builder.WriteString(line)
			if line == "\r\n" {
				break
			}
		}
		requestLines <- builder.String()
		_, _ = proxyConn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	}()

	proxyURL := mustParseURL(t, "http://alice:wonderland@proxy.example:8080")
	duration, category, err := connectThroughProxy(clientConn, proxyURL, "api.example.com:443", time.Second)
	if err != nil {
		t.Fatalf("connectThroughProxy returned error: %v", err)
	}
	if category != "2xx" {
		t.Fatalf("status category = %q, want %q", category, "2xx")
	}
	if duration <= 0 {
		t.Fatalf("duration = %v, want > 0", duration)
	}

	rawRequest := <-requestLines
	if !strings.Contains(rawRequest, "CONNECT api.example.com:443 HTTP/1.1\r\n") {
		t.Fatalf("missing CONNECT request line: %q", rawRequest)
	}
	encodedCreds := base64.StdEncoding.EncodeToString([]byte("alice:wonderland"))
	wantHeader := "Proxy-Authorization: Basic " + encodedCreds + "\r\n"
	if !strings.Contains(rawRequest, wantHeader) {
		t.Fatalf("missing proxy authorization header: got %q want to contain %q", rawRequest, wantHeader)
	}
}

func TestConnectThroughProxyReturnsStatusCategoryForErrors(t *testing.T) {
	t.Parallel()

	clientConn, proxyConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = proxyConn.Close()
	})

	go func() {
		reader := bufio.NewReader(proxyConn)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			if line == "\r\n" {
				break
			}
		}
		_, _ = proxyConn.Write([]byte("HTTP/1.1 407 Proxy Authentication Required\r\n\r\n"))
	}()

	_, category, err := connectThroughProxy(clientConn, nil, "api.example.com:443", time.Second)
	if err == nil {
		t.Fatal("expected error for 4xx CONNECT response")
	}
	if category != "4xx" {
		t.Fatalf("status category = %q, want %q", category, "4xx")
	}
}

func newTestChecker(t *testing.T) (*Checker, proxy.Route) {
	t.Helper()
	proxyURL := mustParseURL(t, "http://proxy.example:8080")
	targetURL := mustParseURL(t, "https://example.com")
	route := proxy.ResolveRoute(proxy.RouteKindControlPlane, "control-plane", targetURL, proxyURL, config.ProxySource("flag"), lookupEnvMap(nil))
	checker := &Checker{
		routes:      []proxy.Route{route},
		routeStatus: map[string]*routeStatus{routeKey(route): {route: route, healthState: HealthStateUnhealthy}},
	}
	return checker, route
}

func lookupEnvMap(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		if values == nil {
			return "", false
		}
		val, ok := values[key]
		return val, ok
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	return parsed
}
