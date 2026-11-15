package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// accountPrep is the invocation-local result of resolving one explicit-strategy member. Nothing
// here is shared between requests: callback ids, caller scope and credential lists live and die
// with the call.
type accountPrep struct {
	plan        accountPlan
	bindingKey  string
	bound       string
	callerScope string
}

// accountMemberResult is how an explicit member reports back to the member loop.
type accountMemberResult struct {
	response pluginapi.ExecutorResponse
	served   bool
	// fatal stops the whole combo chain (caller-side or configuration fault).
	fatal error
	// status/message describe a member-side failure the next member may escape.
	status  int
	message string
}

// classifyExecute turns one non-stream host execution into the status triple the member loop
// uses. A host response can carry a failure without an error: a nil response is a bad gateway,
// and a status >= 400 is a failure whose message is taken from the body so quota/credential
// wording is still recognised by memberFailure.
func classifyExecute(result *pluginapi.HostModelExecutionResponse, err error) (status int, retryable bool, message string) {
	if err != nil {
		var hostErr *hostError
		if errors.As(err, &hostErr) {
			status = hostErr.Status
			if status == 0 {
				status = http.StatusBadGateway
			}
			retryable = hostErr.Retryable || retryableStatus(status)
		} else {
			status = http.StatusBadGateway
			retryable = true
		}
		return status, retryable, err.Error()
	}
	if result == nil {
		return http.StatusBadGateway, true, "host model returned no response"
	}
	status = result.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	if status >= http.StatusBadRequest {
		return status, retryableStatus(status), statusOnlyMessage(status, result.Body)
	}
	return status, false, ""
}

// statusOnlyMessage describes a failed host response that arrived without an error. The body is
// bounded: it only feeds classification and the combo's error text.
func statusOnlyMessage(status int, body []byte) string {
	const limit = 512
	if len(body) > limit {
		body = body[:limit]
	}
	if text := strings.TrimSpace(string(body)); text != "" {
		return fmt.Sprintf("host model status %d: %s", status, text)
	}
	return fmt.Sprintf("host model status %d", status)
}

// fatalExecute reports whether a classified outcome ends the whole combo: a transport error or a
// status >= 400 that memberFailure does not attribute to the member. Only a clean < 400 outcome
// may be served or bound.
func fatalExecute(err error, status int) bool {
	return err != nil || status >= http.StatusBadRequest
}

// pinnedHostRequest builds one pinned attempt. The caller scope is forwarded exactly as the host
// handed it to us and is never synthesized; headers and query are the originals.
func pinnedHostRequest(req pluginapi.ExecutorRequest, prep accountPrep, member Member, credential string, stream bool, body []byte) pluginapi.HostModelExecutionRequest {
	return pluginapi.HostModelExecutionRequest{
		EntryProtocol:  req.SourceFormat,
		ExitProtocol:   req.Format,
		Model:          member.Model,
		Stream:         stream,
		Body:           body,
		Headers:        req.Headers,
		Query:          req.Query,
		ForcedProvider: prep.plan.Provider,
		AuthID:         credential,
		CallerScope:    prep.callerScope,
	}
}

// prepareAccountMember validates and resolves an explicit-strategy member. A returned error with
// Retryable=false is fatal for the whole combo; Retryable=true means this member cannot run but a
// configured fallback member may. It never produces an unpinned plan.
func (p *combosPlugin) prepareAccountMember(ctx context.Context, req pluginapi.ExecutorRequest, combo Combo, member Member) (accountPrep, *comboError) {
	if strings.TrimSpace(hostCallbackIDFromContext(ctx)) == "" {
		return accountPrep{}, &comboError{Status: http.StatusInternalServerError, Message: "tea-combos: explicit accountStrategy requires a host callback id"}
	}
	if member.Model == combo.Name {
		return accountPrep{}, &comboError{Status: http.StatusBadRequest, Message: fmt.Sprintf("combo %q member %q references itself", combo.Name, member.Model)}
	}
	if _, nested := p.store.Get(member.Model); nested {
		return accountPrep{}, &comboError{Status: http.StatusBadRequest, Message: fmt.Sprintf("combo %q member %q is another combo; explicit accountStrategy members must be provider models", combo.Name, member.Model)}
	}
	strategy := member.EffectiveAccountStrategy(combo)
	prep := accountPrep{callerScope: metadataString(req.Metadata, pluginapi.CallerScopeMetadataKey)}
	if comboAffinityEnabled(combo) {
		prep.bindingKey = accountBindingKey(prep.callerScope, sessionIdentity(req), combo, member, strategy)
		prep.bound, _ = p.accountBindings.Lookup(prep.bindingKey)
	}
	plan, cerr := p.accounts.plan(accountPlanRequest{Combo: combo, Member: member, Bound: prep.bound})
	if cerr != nil {
		if prep.bound != "" {
			p.accountBindings.Remove(prep.bindingKey)
		}
		return accountPrep{}, cerr
	}
	if prep.bound != "" && !plan.Bound {
		// The pool no longer offers the bound credential (disabled, cooling, exhausted).
		p.accountBindings.Remove(prep.bindingKey)
		prep.bound = ""
	}
	prep.plan = plan
	return prep, nil
}

func (p *combosPlugin) bindAccount(combo Combo, prep accountPrep, credential string) {
	p.accountBindings.Bind(prep.bindingKey, credential, comboAffinityTTL(combo))
}

func (p *combosPlugin) dropFailedBinding(prep accountPrep, credential string) {
	if prep.bindingKey != "" && prep.bound != "" && credential == prep.bound {
		p.accountBindings.Remove(prep.bindingKey)
	}
}

func contextStopped(ctx context.Context) *comboError {
	if err := ctx.Err(); err != nil {
		return &comboError{Status: http.StatusRequestTimeout, Message: err.Error()}
	}
	return nil
}

// runAccountMember executes one explicit-strategy member over its planned credentials. Each
// listed credential is tried at most once; only member-side failures advance. The member cools
// only after every permitted credential failed.
func (p *combosPlugin) runAccountMember(ctx context.Context, req pluginapi.ExecutorRequest, combo Combo, member Member, trace *[]attemptTrace) accountMemberResult {
	prep, cerr := p.prepareAccountMember(ctx, req, combo, member)
	if cerr != nil {
		if !cerr.Retryable {
			return accountMemberResult{fatal: cerr}
		}
		*trace = append(*trace, attemptTrace{Member: member.Model, Status: cerr.Status, Error: cerr.Message, Retryable: true})
		return accountMemberResult{status: cerr.Status, message: cerr.Message}
	}
	payload := rewriteModel(req.Payload, member.Model)
	lastStatus, lastMessage := 0, ""
	var lastHeaders map[string][]string
	for _, credential := range prep.plan.Credentials {
		if stop := contextStopped(ctx); stop != nil {
			return accountMemberResult{fatal: stop}
		}
		result, err := hostModelExecute(ctx, pinnedHostRequest(req, prep, member, credential, false, payload))
		status, retryable, message := classifyExecute(result, err)
		*trace = append(*trace, attemptTrace{Member: member.Model, Status: status, Error: message, Retryable: retryable})
		if !memberFailure(status, message) {
			p.cooldown.Clear(combo.Name, member)
			if fatalExecute(err, status) {
				return accountMemberResult{fatal: &comboError{Status: status, Message: message}}
			}
			p.bindAccount(combo, prep, credential)
			return accountMemberResult{response: pluginapi.ExecutorResponse{Payload: result.Body, Headers: result.Headers}, served: true}
		}
		p.dropFailedBinding(prep, credential)
		lastHeaders = nil
		if result != nil {
			lastHeaders = result.Headers
		}
		lastStatus, lastMessage = status, message
	}
	p.cooldown.Cool(combo.Name, member, p.coolDuration(combo, lastHeaders, lastMessage))
	return accountMemberResult{status: lastStatus, message: lastMessage}
}

// primedStream is one opened host stream held at its first payload (or clean end).
type primedStream struct {
	opened    *pluginapi.HostModelStreamResponse
	first     []byte
	done      bool
	status    int
	retryable bool
	message   string
	headers   map[string][]string
	ok        bool
}

// primeStream opens a host stream and reads until the first payload. Every failure path closes
// the host stream it opened, so a failed attempt never leaks one.
func primeStream(ctx context.Context, hostReq pluginapi.HostModelExecutionRequest) primedStream {
	opened, err := hostModelExecuteStream(ctx, hostReq)
	out := primedStream{opened: opened}
	if err != nil {
		out.status, out.retryable = streamErrorStatus(err)
		out.message = err.Error()
		return out
	}
	if opened == nil {
		out.status, out.retryable = http.StatusBadGateway, true
		out.message = "host model stream returned no response"
		return out
	}
	out.status = opened.StatusCode
	if out.status == 0 {
		out.status = http.StatusOK
	}
	out.retryable = retryableStatus(out.status)
	out.headers = opened.Headers
	if out.status >= http.StatusBadRequest {
		hostModelStreamClose(opened.StreamID)
		out.message = fmt.Sprintf("host model status %d", out.status)
		return out
	}
	if strings.TrimSpace(opened.StreamID) == "" {
		out.status, out.retryable = http.StatusBadGateway, true
		out.message = "host model stream returned an empty stream id"
		return out
	}
	first, done, readErr := readUntilFirstPayload(ctx, opened.StreamID)
	if readErr != nil {
		hostModelStreamClose(opened.StreamID)
		out.status, out.retryable = streamErrorStatus(readErr)
		out.message = readErr.Error()
		return out
	}
	out.first, out.done, out.ok = first, done, true
	return out
}

// accountStreamResult is runAccountMemberStream's outcome.
type accountStreamResult struct {
	served  bool
	headers http.Header
	handle  *comboStreamHandle
	fatal   error
	status  int
	message string
}

// runAccountMemberStream is the streaming twin of runAccountMember. Credentials advance only
// before the first payload is emitted; after it the returned handle is final and never replayed.
func (p *combosPlugin) runAccountMemberStream(ctx context.Context, req pluginapi.ExecutorRequest, combo Combo, member Member, emit func([]byte) error, trace *[]attemptTrace) accountStreamResult {
	prep, cerr := p.prepareAccountMember(ctx, req, combo, member)
	if cerr != nil {
		if !cerr.Retryable {
			return accountStreamResult{fatal: cerr}
		}
		*trace = append(*trace, attemptTrace{Member: member.Model, Status: cerr.Status, Error: cerr.Message, Retryable: true})
		return accountStreamResult{status: cerr.Status, message: cerr.Message}
	}
	payload := rewriteModel(req.Payload, member.Model)
	lastStatus, lastMessage := 0, ""
	var lastHeaders map[string][]string
	for _, credential := range prep.plan.Credentials {
		if stop := contextStopped(ctx); stop != nil {
			return accountStreamResult{fatal: stop}
		}
		primed := primeStream(ctx, pinnedHostRequest(req, prep, member, credential, true, payload))
		if !primed.ok {
			*trace = append(*trace, attemptTrace{Member: member.Model, Status: primed.status, Error: primed.message, Retryable: primed.retryable})
			if !memberFailure(primed.status, primed.message) {
				return accountStreamResult{fatal: &comboError{Status: primed.status, Message: primed.message}}
			}
			p.dropFailedBinding(prep, credential)
			lastStatus, lastMessage, lastHeaders = primed.status, primed.message, primed.headers
			continue
		}
		handle := &comboStreamHandle{streamID: primed.opened.StreamID}
		if len(primed.first) == 0 && primed.done {
			// An explicit-account stream that ends before any payload gave the client nothing:
			// close it, never bind it, and let the next permitted credential answer. (Inherited
			// members keep treating a clean empty stream as success; that path is untouched.)
			handle.Close()
			message := "host model stream ended before the first payload"
			*trace = append(*trace, attemptTrace{Member: member.Model, Status: http.StatusBadGateway, Error: message, Retryable: true})
			p.dropFailedBinding(prep, credential)
			lastStatus, lastMessage, lastHeaders = http.StatusBadGateway, message, primed.headers
			continue
		}
		*trace = append(*trace, attemptTrace{Member: member.Model, Status: primed.status})
		p.cooldown.Clear(combo.Name, member)
		if len(primed.first) > 0 {
			if errEmit := emit(primed.first); errEmit != nil {
				handle.Close()
				return accountStreamResult{fatal: &comboError{Status: http.StatusBadGateway, Message: errEmit.Error()}}
			}
		}
		p.bindAccount(combo, prep, credential)
		return accountStreamResult{served: true, headers: primed.opened.Headers, handle: handle}
	}
	p.cooldown.Cool(combo.Name, member, p.coolDuration(combo, lastHeaders, lastMessage))
	return accountStreamResult{status: lastStatus, message: lastMessage}
}
