package wiretypes

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestRawJSONRPCPolledCommandMarshalFieldNames(t *testing.T) {
	createdAt := time.Date(2024, time.September, 4, 12, 30, 0, 0, time.UTC)
	cmd := RawJSONRPCPolledCommand{
		BaseRawPolledCommand: BaseRawPolledCommand{
			RequestID:   "req-123",
			ShardToken:  "shard-456",
			CommandType: CommandTypeJSONRPC,
			CreatedAt:   createdAt,
			Headers: http.Header{
				"X-Trace-ID": []string{"trace-789"},
			},
		},
		JSONRPC: json.RawMessage(`{"jsonrpc":"2.0","id":"rpc-99","method":"tools/list","params":{"needle":"hay"}}`),
	}

	payload, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshal raw jsonrpc polled command: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal marshaled payload: %v", err)
	}

	if got["request_id"] != "req-123" {
		t.Fatalf("expected request_id to be req-123, got %v", got["request_id"])
	}
	if got["shard_token"] != "shard-456" {
		t.Fatalf("expected shard_token to be shard-456, got %v", got["shard_token"])
	}
	if got["command_type"] != string(CommandTypeJSONRPC) {
		t.Fatalf("expected command_type to be %q, got %v", CommandTypeJSONRPC, got["command_type"])
	}
	if _, ok := got["created_at"]; !ok {
		t.Fatalf("expected created_at to be present")
	}
	if _, ok := got["jsonrpc"]; !ok {
		t.Fatalf("expected jsonrpc to be present")
	}

	headersValue, ok := got["headers"].(map[string]any)
	if !ok {
		t.Fatalf("expected headers to be a map, got %T", got["headers"])
	}
	traceHeaders, ok := headersValue["X-Trace-ID"].([]any)
	if !ok || len(traceHeaders) != 1 || traceHeaders[0] != "trace-789" {
		t.Fatalf("expected headers to include X-Trace-ID trace-789, got %v", headersValue)
	}
}

func TestTunnelResponsePayloadOmitemptyAndHeaders(t *testing.T) {
	payload := TunnelResponsePayload{RequestID: "req-omit"}
	marshaled, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(marshaled, &got); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	if len(got) != 1 || got["request_id"] != "req-omit" {
		t.Fatalf("expected only request_id to be present, got %v", got)
	}

	payloadWithHeaders := TunnelResponsePayload{
		RequestID: "req-headers",
		ResponseHeaders: http.Header{
			"X-Resp-ID": []string{"resp-abc"},
		},
	}
	marshaledWithHeaders, err := json.Marshal(payloadWithHeaders)
	if err != nil {
		t.Fatalf("marshal payload with headers: %v", err)
	}

	var gotWithHeaders map[string]any
	if err := json.Unmarshal(marshaledWithHeaders, &gotWithHeaders); err != nil {
		t.Fatalf("unmarshal payload with headers: %v", err)
	}

	headersValue, ok := gotWithHeaders["resp_headers"].(map[string]any)
	if !ok {
		t.Fatalf("expected resp_headers to be a map, got %T", gotWithHeaders["resp_headers"])
	}
	respHeaders, ok := headersValue["X-Resp-ID"].([]any)
	if !ok || len(respHeaders) != 1 || respHeaders[0] != "resp-abc" {
		t.Fatalf("expected resp_headers to include X-Resp-ID resp-abc, got %v", headersValue)
	}
}

func TestPolledCommandEnvelopeUnmarshalCommands(t *testing.T) {
	fixture := []byte(`{"commands":[{"request_id":"req-777","shard_token":"shard-888","command_type":"jsonrpc","created_at":"2024-10-11T12:13:14Z","headers":{"X-Test":["alpha"]},"jsonrpc":{"jsonrpc":"2.0","id":"rpc-1","method":"tools/list","params":{"needle":"hay"}}},{"request_id":"req-888","shard_token":"shard-999","command_type":"oauth_discovery","created_at":"2024-10-11T12:14:15Z","headers":{}}]}`)

	var envelope PolledCommandEnvelope
	if err := json.Unmarshal(fixture, &envelope); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}

	if len(envelope.Commands) != 2 {
		t.Fatalf("expected 2 commands, got %d", len(envelope.Commands))
	}

	var first map[string]any
	if err := json.Unmarshal(envelope.Commands[0], &first); err != nil {
		t.Fatalf("unmarshal first command: %v", err)
	}
	if first["request_id"] != "req-777" {
		t.Fatalf("expected first request_id to be req-777, got %v", first["request_id"])
	}
	if first["command_type"] != string(CommandTypeJSONRPC) {
		t.Fatalf("expected first command_type to be %q, got %v", CommandTypeJSONRPC, first["command_type"])
	}

	var second map[string]any
	if err := json.Unmarshal(envelope.Commands[1], &second); err != nil {
		t.Fatalf("unmarshal second command: %v", err)
	}
	if second["request_id"] != "req-888" {
		t.Fatalf("expected second request_id to be req-888, got %v", second["request_id"])
	}
	if second["command_type"] != string(CommandTypeOAuthDiscovery) {
		t.Fatalf("expected second command_type to be %q, got %v", CommandTypeOAuthDiscovery, second["command_type"])
	}
}
