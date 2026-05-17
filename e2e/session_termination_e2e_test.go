package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"go.openai.org/api/tunnel-client/pkg/controlplane/wiretypes"
	harnesspkg "go.openai.org/api/tunnel-client/testsupport/e2e"
	"go.openai.org/api/tunnel-client/testsupport/mockmcpserver"
	"go.openai.org/api/tunnel-client/testsupport/mocktunnelservice"
)

func TestHarnessHandlesSessionTerminationCommand(t *testing.T) {
	const requestID = "cmd-session-termination"

	sessionTermination := mocktunnelservice.CommandResponse{
		Command: mocktunnelservice.NewSessionTerminationCommand(requestID, nil),
		ExpectedResponses: []mocktunnelservice.ExpectedResponse{{
			RequestID: requestID,
			Assert: func(tb testing.TB, resp mocktunnelservice.ReceivedResponse) {
				if tb != nil {
					tb.Helper()
				}
				target := tb
				if target == nil {
					target = t
				}
				if resp.ResponseType != string(wiretypes.ResponsePayloadSessionTermination) {
					target.Fatalf("session termination response type mismatch: got %q", resp.ResponseType)
				}
				if resp.ResponseCode != http.StatusNoContent {
					target.Fatalf("session termination response code mismatch: %d", resp.ResponseCode)
				}
				if len(resp.JSONResponse) != 0 {
					target.Fatalf("session termination response should not include resp_json payload")
				}
			},
		}},
	}

	h := harnesspkg.NewHarness(
		t,
		harnesspkg.WithControlPlaneOptions(
			mocktunnelservice.WithSessionHeaderPropagation(),
			mocktunnelservice.WithInitializationPhaseCommands(),
			mocktunnelservice.WithCommandResponses(sessionTermination),
		),
	)
	h.ExecuteScenarious(t)
}

func TestHarnessRejectsSessionTerminationForStdioAndKeepsServing(t *testing.T) {
	commandArgs := mockmcpserver.StdioServerCommand(t)

	const (
		sessionTerminationRequestID = "cmd-session-termination"
		toolRequestID               = "cmd-tool-after-session-termination"
		callID                      = "tool-after-session-termination"
		userName                    = "Ada"
		sessionID                   = "stdio-session"
	)

	sessionTermination := mocktunnelservice.CommandResponse{
		Command: mocktunnelservice.NewSessionTerminationCommand(
			sessionTerminationRequestID,
			http.Header{"Mcp-Session-Id": {sessionID}},
		),
		ExpectedResponses: []mocktunnelservice.ExpectedResponse{{
			RequestID: sessionTerminationRequestID,
			Assert: func(tb testing.TB, resp mocktunnelservice.ReceivedResponse) {
				if tb != nil {
					tb.Helper()
				}
				target := tb
				if target == nil {
					target = t
				}
				if resp.ResponseType != string(wiretypes.ResponsePayloadSessionTermination) {
					target.Fatalf("session termination response type mismatch: got %q", resp.ResponseType)
				}
				if resp.ResponseCode != http.StatusMethodNotAllowed {
					target.Fatalf("session termination response code mismatch: got %d want %d", resp.ResponseCode, http.StatusMethodNotAllowed)
				}
			},
		}},
	}
	toolCommand := mocktunnelservice.CommandResponse{
		Command: mocktunnelservice.NewCommand(
			toolRequestID,
			json.RawMessage(`{
				"jsonrpc":"2.0",
				"id":"`+callID+`",
				"method":"tools/call",
				"params":{
					"name":"echo",
					"arguments":{
						"name":"`+userName+`"
					}
				}
			}`),
			nil,
		),
		ExpectedResponses: []mocktunnelservice.ExpectedResponse{{
			RequestID: toolRequestID,
			Assert: func(tb testing.TB, resp mocktunnelservice.ReceivedResponse) {
				if tb != nil {
					tb.Helper()
				}
				target := tb
				if target == nil {
					target = t
				}
				if resp.ResponseType != string(wiretypes.ResponsePayloadJSONRPC) {
					target.Fatalf("tool call response type mismatch: got %q", resp.ResponseType)
				}
				if resp.ResponseCode != http.StatusOK {
					target.Fatalf("tool call response code mismatch: %d", resp.ResponseCode)
				}
				if len(resp.JSONResponse) == 0 {
					target.Fatalf("tool call missing resp_json payload")
				}
			},
		}},
	}

	h := harnesspkg.NewHarness(
		t,
		harnesspkg.WithMCPCommand(commandArgs),
		harnesspkg.WithControlPlaneOptions(
			mocktunnelservice.WithInitializationPhaseCommandsWithoutSessionHeaders(),
			mocktunnelservice.WithCommandResponses(sessionTermination, toolCommand),
		),
	)
	h.ExecuteScenarious(t)

	matched := h.ControlPlane.ReceivedResponses(mocktunnelservice.ResponseMatchMatched)
	if len(matched) != 4 {
		t.Fatalf("expected four matched responses (initialize, initialized, delete, tool); got %d", len(matched))
	}
	var toolResponse mocktunnelservice.ReceivedResponse
	for _, resp := range matched {
		if resp.RequestID == toolRequestID {
			toolResponse = resp
			break
		}
	}
	if toolResponse.RequestID == "" {
		t.Fatalf("tool response for %s not recorded", toolRequestID)
	}
	var rpcPayload struct {
		Result struct {
			StructuredContent map[string]any `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(toolResponse.JSONResponse, &rpcPayload); err != nil {
		t.Fatalf("decode tool response payload: %v", err)
	}
	msg, _ := rpcPayload.Result.StructuredContent["message"].(string)
	expectedMessage := fmt.Sprintf("hello %s", userName)
	if msg != expectedMessage {
		t.Fatalf("unexpected tool response message: got %q want %q", msg, expectedMessage)
	}
}
