package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// abiExecutorStreamRequest mirrors CPA's rpcExecutorRequest: the host-owned stream id and the
// callback id ride beside the executor request.
type abiExecutorStreamRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// rpcExecutorStreamResponse is CPA's decoding shape for executor.execute_stream results.
type rpcExecutorStreamResponse struct {
	Headers map[string][]string             `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

type pluginStreamIDKey struct{}

func withPluginStreamID(ctx context.Context, id string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, pluginStreamIDKey{}, id)
}

func pluginStreamIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(pluginStreamIDKey{}).(string)
	return id
}

// handleAbiExecuteStream runs one executor.execute_stream call. The executor returns as soon as
// the serving member's first payload is in flight; the remaining chunks are emitted from the
// goroutine opened by executeStream. Errors before the first byte become a combo_failed envelope
// so CPA can surface the real status.
func handleAbiExecuteStream(request []byte) ([]byte, error) {
	plugin := currentPlugin.Load()
	if plugin == nil {
		return abiErrorEnvelope("not_ready", "plugin not registered"), nil
	}
	var req abiExecutorStreamRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	ctx := withHostCallbackID(context.Background(), req.HostCallbackID)
	ctx = withPluginStreamID(ctx, req.StreamID)
	resp, err := plugin.executeStream(ctx, req.ExecutorRequest)
	if err != nil {
		var comboErr *comboError
		if errors.As(err, &comboErr) {
			raw, errMarshal := json.Marshal(abiEnvelope{OK: false, Error: &abiError{
				Code:       "combo_failed",
				Message:    comboErr.Message,
				Retryable:  comboErr.Retryable,
				HTTPStatus: comboErr.Status,
			}})
			return raw, errMarshal
		}
		return nil, err
	}
	headers := map[string][]string(nil)
	if len(resp.Headers) > 0 {
		headers = resp.Headers
	}
	return abiOKEnvelope(rpcExecutorStreamResponse{Headers: headers})
}

type rpcStreamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
	Error    string `json:"error,omitempty"`
	// ErrorDetail carries status/retry metadata that a plain message cannot.
	ErrorDetail *pluginabi.Error `json:"error_detail,omitempty"`
}

type rpcStreamCloseRequest struct {
	StreamID    string           `json:"stream_id"`
	Error       string           `json:"error,omitempty"`
	ErrorDetail *pluginabi.Error `json:"error_detail,omitempty"`
}

func hostModelExecuteStreamRPC(ctx context.Context, req pluginapi.HostModelExecutionRequest) (*pluginapi.HostModelStreamResponse, error) {
	raw, err := hostCall(pluginabi.MethodHostModelExecuteStream, rpcHostModelExecutionRequest{
		HostModelExecutionRequest: req,
		HostCallbackID:            hostCallbackIDFromContext(ctx),
	})
	if err != nil {
		return nil, err
	}
	var resp pluginapi.HostModelStreamResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode host model stream response: %w", err)
	}
	return &resp, nil
}

func hostModelStreamReadRPC(ctx context.Context, streamID string) (*pluginapi.HostModelStreamReadResponse, error) {
	raw, err := hostCall(pluginabi.MethodHostModelStreamRead, pluginapi.HostModelStreamReadRequest{StreamID: streamID})
	if err != nil {
		return nil, err
	}
	var resp pluginapi.HostModelStreamReadResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode host model stream read response: %w", err)
	}
	return &resp, nil
}

func hostModelStreamCloseRPC(streamID string) error {
	_, err := hostCall(pluginabi.MethodHostModelStreamClose, pluginapi.HostModelStreamCloseRequest{StreamID: streamID})
	return err
}

func hostStreamEmitRPC(streamID string, payload []byte) error {
	_, err := hostCall(pluginabi.MethodHostStreamEmit, rpcStreamEmitRequest{
		StreamID: streamID,
		Payload:  payload,
	})
	return err
}

func hostStreamCloseRPC(streamID string, detail *pluginabi.Error) {
	_, _ = hostCall(pluginabi.MethodHostStreamClose, rpcStreamCloseRequest{
		StreamID:    streamID,
		ErrorDetail: detail,
	})
}
