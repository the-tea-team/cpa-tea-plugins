package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Host seams. Tests replace these with in-memory fakes; production uses the RPC wrappers in
// abi_stream.go. Keeping them as package-level variables mirrors hostModelExecute.
var (
	hostModelExecuteStream = func(ctx context.Context, req pluginapi.HostModelExecutionRequest) (*pluginapi.HostModelStreamResponse, error) {
		return hostModelExecuteStreamRPC(ctx, req)
	}
	hostModelStreamRead = func(ctx context.Context, streamID string) (*pluginapi.HostModelStreamReadResponse, error) {
		return hostModelStreamReadRPC(ctx, streamID)
	}
	hostModelStreamClose = func(streamID string) {
		_ = hostModelStreamCloseRPC(streamID)
	}
	pluginStreamEmit = func(ctx context.Context, streamID string, payload []byte) error {
		return hostStreamEmitRPC(streamID, payload)
	}
	pluginStreamClose = func(streamID string, detail *pluginabi.Error) {
		hostStreamCloseRPC(streamID, detail)
	}
)

// comboStreamHandle is an opened host model stream. executeComboStream has already forwarded the
// first payload chunk (if any) by the time it returns a handle, so a later failure is final.
type comboStreamHandle struct {
	streamID string
	pending  string
}

// Close releases the host stream. It is safe to call more than once.
func (h *comboStreamHandle) Close() {
	if h == nil || h.streamID == "" {
		return
	}
	hostModelStreamClose(h.streamID)
	h.streamID = ""
}

// Pump forwards every remaining chunk. The first chunk was already emitted by executeComboStream,
// so an error here is after the first byte and must not trigger another member.
func (h *comboStreamHandle) Pump(ctx context.Context, streamID string) error {
	if h.pending != "" {
		return errors.New(h.pending)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		read, err := hostModelStreamRead(ctx, h.streamID)
		if err != nil {
			return err
		}
		if read == nil {
			return errors.New("host model stream read returned no response")
		}
		if read.Error != "" {
			return errors.New(read.Error)
		}
		if len(read.Payload) > 0 {
			if err := pluginStreamEmit(ctx, streamID, read.Payload); err != nil {
				return err
			}
		}
		if read.Done {
			return nil
		}
	}
}

// ExecuteStream is the Executor interface entry point. The ABI carries the host stream id beside
// the request, so the abi layer puts it in the context before calling this.
func (p *combosPlugin) executeStream(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorStreamResponse, error) {
	streamID := pluginStreamIDFromContext(ctx)
	if strings.TrimSpace(streamID) == "" {
		return pluginapi.ExecutorStreamResponse{}, &comboError{
			Status:  http.StatusInternalServerError,
			Message: "tea-combos: stream id is required for executor.execute_stream",
		}
	}
	emit := func(payload []byte) error {
		return pluginStreamEmit(ctx, streamID, payload)
	}
	// The first chunk (or the decision that every member failed before one) is resolved
	// synchronously so the returned headers really are the serving member's and a failure before
	// any byte still reaches the client as a real HTTP status via the ABI error envelope.
	_, headers, handle, err := p.executeComboStream(ctx, req, strings.TrimSpace(req.Model), -1, emit)
	if err != nil {
		return pluginapi.ExecutorStreamResponse{}, err
	}
	if handle == nil {
		pluginStreamClose(streamID, nil)
		return pluginapi.ExecutorStreamResponse{Headers: headers}, nil
	}
	go func() {
		errPump := handle.Pump(ctx, streamID)
		// Release the host stream before telling the client the stream ended, so nothing in this
		// goroutine touches host seams after the client-visible close.
		handle.Close()
		if errPump != nil {
			pluginStreamClose(streamID, &pluginabi.Error{
				Code:    "combo_stream_failed",
				Message: errPump.Error(),
			})
			return
		}
		pluginStreamClose(streamID, nil)
	}()
	return pluginapi.ExecutorStreamResponse{Headers: headers}, nil
}

// executeComboStream walks the selector order like executeCombo, but only moves to the next member
// while nothing has been sent to the client. It returns the index of the member that served, that
// member's response headers, and an open handle when more chunks remain.
//
// preferredIndex is the slice B affinity seam. An empty member list, a disabled combo or an unknown
// strategy produces the same comboError the non-stream path does.
func (p *combosPlugin) executeComboStream(ctx context.Context, req pluginapi.ExecutorRequest, comboName string, preferredIndex int, emit func([]byte) error) (int, http.Header, *comboStreamHandle, error) {
	combo, ok := p.store.Get(comboName)
	if !ok {
		return -1, nil, nil, &comboError{Status: http.StatusNotFound, Message: fmt.Sprintf("combo %q is not configured", comboName)}
	}
	if !combo.IsEnabled() {
		return -1, nil, nil, &comboError{Status: http.StatusNotFound, Message: fmt.Sprintf("combo %q is disabled", comboName)}
	}
	if len(combo.Members) == 0 {
		return -1, nil, nil, &comboError{Status: http.StatusNotFound, Message: fmt.Sprintf("combo %q has no members", comboName)}
	}
	selector, err := p.memberSelector(combo.Strategy)
	if err != nil {
		return -1, nil, nil, &comboError{Status: http.StatusInternalServerError, Message: err.Error()}
	}
	// Session affinity, same rules as the non-stream path: the bound member goes first, a binding
	// is recorded only once a member has served, and a non-retryable stop or an exhausted chain
	// leaves it untouched. An explicit preferredIndex wins over a resolved binding.
	affinityKey, boundIndex, bound := p.affinity.Resolve(combo, req)
	if preferredIndex < 0 && bound {
		preferredIndex = boundIndex
	}
	order := p.selectOrder(combo, selector, affinityKey, preferredIndex)

	trace := make([]attemptTrace, 0, len(order))
	lastStatus := 0
	lastMessage := ""

	for _, index := range order {
		if err := ctx.Err(); err != nil {
			return -1, nil, nil, &comboError{Status: http.StatusRequestTimeout, Message: err.Error()}
		}
		member := combo.Members[index]
		if member.ExplicitAccountStrategy(combo) {
			outcome := p.runAccountMemberStream(ctx, req, combo, member, emit, &trace)
			if outcome.fatal != nil {
				return -1, nil, nil, outcome.fatal
			}
			if outcome.served {
				p.affinity.Bind(affinityKey, index, combo)
				return index, outcome.headers, outcome.handle, nil
			}
			lastStatus, lastMessage = outcome.status, outcome.message
			continue
		}
		primed := primeStream(ctx, pluginapi.HostModelExecutionRequest{
			EntryProtocol:  req.SourceFormat,
			ExitProtocol:   req.Format,
			Model:          member.Model,
			Stream:         true,
			Body:           rewriteModel(req.Payload, member.Model),
			Headers:        req.Headers,
			Query:          req.Query,
			ForcedProvider: member.Provider,
		})
		if !primed.ok {
			trace = append(trace, attemptTrace{Member: member.Model, Status: primed.status, Error: primed.message, Retryable: primed.retryable})
			if !memberFailure(primed.status, primed.message) {
				return -1, nil, nil, &comboError{Status: primed.status, Message: primed.message}
			}
			p.cooldown.Cool(combo.Name, member, p.coolDuration(combo, primed.headers, primed.message))
			lastStatus = primed.status
			lastMessage = primed.message
			continue
		}
		trace = append(trace, attemptTrace{Member: member.Model, Status: primed.status, Retryable: false})
		p.cooldown.Clear(combo.Name, member)
		handle := &comboStreamHandle{streamID: primed.opened.StreamID}
		if len(primed.first) == 0 && primed.done {
			// A clean empty stream is a successful member: nothing to forward, no fallback.
			handle.Close()
			p.affinity.Bind(affinityKey, index, combo)
			return index, primed.opened.Headers, nil, nil
		}
		if len(primed.first) > 0 {
			if errEmit := emit(primed.first); errEmit != nil {
				handle.Close()
				return -1, nil, nil, &comboError{Status: http.StatusBadGateway, Message: errEmit.Error()}
			}
		}
		p.affinity.Bind(affinityKey, index, combo)
		return index, primed.opened.Headers, handle, nil
	}

	if lastStatus == 0 {
		lastStatus = http.StatusBadGateway
	}
	if lastMessage == "" {
		lastMessage = "every member failed retryably"
	}
	encodedTrace, _ := json.Marshal(trace)
	return -1, nil, nil, &comboError{
		Status:    lastStatus,
		Retryable: true,
		Message:   fmt.Sprintf("combo %s exhausted: %s | attempts=%s | cooling=%v", combo.Name, lastMessage, string(encodedTrace), p.cooldown.Cooling(combo.Name, combo.Members)),
	}
}

// readUntilFirstPayload reads until a non-empty payload, a clean end, or an error. A member only
// counts as opened once one of those three outcomes is known; the returned payload is the single
// chunk held back for the fallback decision, never a buffered stream.
func readUntilFirstPayload(ctx context.Context, streamID string) ([]byte, bool, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		read, err := hostModelStreamRead(ctx, streamID)
		if err != nil {
			return nil, false, err
		}
		if read == nil {
			return nil, false, errors.New("host model stream read returned no response")
		}
		if read.Error != "" {
			return nil, false, errors.New(read.Error)
		}
		if len(read.Payload) > 0 {
			return read.Payload, read.Done, nil
		}
		if read.Done {
			return nil, true, nil
		}
	}
}

// streamErrorStatus turns a host failure into the status/retryable pair the non-stream loop uses.
// A mid-stream read error carries no status from the host, and since a stream only opens on a
// non-4xx member status, any pre-payload read error is treated as retryable.
func streamErrorStatus(err error) (int, bool) {
	status := 0
	retryable := false
	var hostErr *hostError
	if errors.As(err, &hostErr) {
		status = hostErr.Status
		retryable = hostErr.Retryable
	}
	if status == 0 {
		status = http.StatusBadGateway
	}
	return status, retryable || retryableStatus(status)
}
