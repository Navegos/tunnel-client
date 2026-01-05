package internal

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestForwardingRoundTripperRoundTrip(t *testing.T) {
	makeResponse := func(status int, headers http.Header) *http.Response {
		return &http.Response{
			StatusCode: status,
			Header:     headers.Clone(),
			Body:       io.NopCloser(strings.NewReader("")),
		}
	}

	tests := []struct {
		name                string
		requestHeaders      http.Header
		expectRequestHeader http.Header
		baseResponse        *http.Response
		baseError           error
		expectedStatus      int
		expectedHeaders     http.Header
		preexistingHeaders  http.Header
	}{
		{
			name:                "injects request headers and captures response",
			requestHeaders:      http.Header{"X-Test": {"forward-me"}, "X-Other": {"one", "two"}},
			expectRequestHeader: http.Header{"X-Test": {"forward-me"}, "X-Other": {"one", "two"}},
			preexistingHeaders:  http.Header{"X-Test": {"should-be-overwritten"}},
			baseResponse:        makeResponse(http.StatusAccepted, http.Header{"X-Resp": {"ok"}}),
			expectedStatus:      http.StatusAccepted,
			expectedHeaders:     http.Header{"X-Resp": {"ok"}},
		},
		{
			name:            "captures response headers",
			requestHeaders:  nil,
			baseResponse:    makeResponse(http.StatusOK, http.Header{"X-Resp": {"ok"}, "X-Trace": {"abc"}}),
			expectedStatus:  http.StatusOK,
			expectedHeaders: http.Header{"X-Resp": {"ok"}, "X-Trace": {"abc"}},
		},
		{
			name:            "propagates error without capturing response",
			requestHeaders:  http.Header{"X-Test": {"forward-me"}},
			baseResponse:    makeResponse(http.StatusBadGateway, http.Header{"X-Resp": {"should-not-store"}}),
			baseError:       errors.New("transport failure"),
			expectedStatus:  0,
			expectedHeaders: nil,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx, carrier, err := ContextWithHeaders(context.Background(), tc.requestHeaders)
			if err != nil {
				t.Fatalf("ContextWithHeaders: %v", err)
			}

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.com", nil)
			if err != nil {
				t.Fatalf("NewRequestWithContext: %v", err)
			}
			for key, values := range tc.preexistingHeaders {
				for _, value := range values {
					req.Header.Add(key, value)
				}
			}

			rt := NewForwardingRoundTripper(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				for key, want := range tc.expectRequestHeader {
					got := req.Header.Values(key)
					if diff := cmp.Diff(want, got, cmpopts.SortSlices(func(a, b string) bool { return a < b })); diff != "" {
						t.Fatalf("request header %s mismatch (-want +got):\n%s", key, diff)
					}
				}
				return tc.baseResponse, tc.baseError
			}))

			resp, err := rt.RoundTrip(req)
			if tc.baseError != nil {
				if !errors.Is(err, tc.baseError) {
					t.Fatalf("RoundTrip error mismatch: got %v, want %v", err, tc.baseError)
				}
			} else if err != nil {
				t.Fatalf("RoundTrip: %v", err)
			}

			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}

			status, headers := carrier.ResponseStatusAndHeaders()
			if status != tc.expectedStatus {
				t.Fatalf("response status mismatch: got %d, want %d", status, tc.expectedStatus)
			}
			if diff := cmp.Diff(tc.expectedHeaders, headers, cmpopts.SortSlices(func(a, b string) bool { return a < b })); diff != "" {
				t.Fatalf("response headers mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestNewForwardingRoundTripperPanicsOnNil(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic when base RoundTripper is nil")
		}
	}()

	_ = NewForwardingRoundTripper(nil)
}
