package httpguard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	providerexecutor "github.com/kombifyio/stackkits/contracts/providerexecutor/v1beta1"
)

type WorkerExecutor interface {
	ExecuteWorker(context.Context, providerexecutor.WorkerExecution) providerexecutor.WorkerExecutionResult
}

type providerControlRequest struct {
	controlRequest
	InvocationID   string                                  `json:"invocation_id,omitempty"`
	ProviderResult *providerexecutor.WorkerExecutionResult `json:"provider_result,omitempty"`
}

// workerIdlePause is the pause after the control plane answered that no
// provider command waits. The control plane holds that answer open for a
// while (a long poll), so this only spaces the polls.
const workerIdlePause = time.Second

func (c *Client) runWorkerLoop(ctx context.Context) {
	backoff := newRetryBackoff(time.Second, 30*time.Second)
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case c.workerGate <- struct{}{}:
		}
		outcome, err := c.runWorkerCycle(ctx)
		<-c.workerGate
		var delay time.Duration
		switch outcome {
		case workerCycleRan:
			backoff.Reset()
			continue
		case workerCycleIdle:
			backoff.Reset()
			delay = workerIdlePause
		default:
			// Refused, unavailable or unreachable: back off, never below a
			// pause the control plane asked for.
			delay = max(backoff.Next(), retryAfterOf(err))
			c.log.Warn("substrate_poll_failed", "error", err.Error(), "retry_in", delay.String())
		}
		if waitContext(ctx, delay) != nil {
			return
		}
	}
}

type workerCycleOutcome int

const (
	workerCycleRan workerCycleOutcome = iota
	workerCycleIdle
	workerCycleFailed
)

// HoldWorker waits until no provider command is in flight and claims no further
// one until release is called. A command's result lives only in this process, so
// a Guard that is about to restart holds the worker first.
func (c *Client) HoldWorker(ctx context.Context) (release func(), err error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case c.workerGate <- struct{}{}:
		return func() { <-c.workerGate }, nil
	}
}

// runWorkerCycle claims and completes at most one provider command, its result
// included. It reports whether one ran, none was waiting, or the poll failed
// (with the error, which may carry the control plane's Retry-After), so the
// caller paces the next poll.
func (c *Client) runWorkerCycle(ctx context.Context) (workerCycleOutcome, error) {
	identity := c.controlIdentity()
	identity.Capabilities = []string{"proxmox-substrate-v1"}
	status, body, retryAfter, err := c.postJSONExchange(ctx, c.cfg.WorkerCommandURL, providerControlRequest{controlRequest: identity}, 256<<10)
	if err != nil {
		return workerCycleFailed, err
	}
	if status == http.StatusNoContent {
		return workerCycleIdle, nil
	}
	if status != http.StatusOK {
		return workerCycleFailed, withRetryAfter(unexpectedStatusError(status, body), retryAfter)
	}
	var response struct {
		Data struct {
			Command *providerexecutor.WorkerExecution `json:"command"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &response) != nil || response.Data.Command == nil {
		return workerCycleFailed, errors.New("substrate command response is empty")
	}
	var result providerexecutor.WorkerExecutionResult
	if executor, ok := c.cfg.WorkerExecutor.(interface {
		ExecuteWorkerWithBootstrap(context.Context, providerexecutor.WorkerExecution, func(context.Context) ([]byte, error)) providerexecutor.WorkerExecutionResult
	}); ok {
		result = executor.ExecuteWorkerWithBootstrap(ctx, *response.Data.Command, func(fetchCtx context.Context) ([]byte, error) {
			status, body, err := c.postJSONResponseLimit(fetchCtx, strings.TrimSuffix(c.cfg.WorkerCommandURL, "/next")+"/bootstrap", providerControlRequest{controlRequest: identity, InvocationID: response.Data.Command.InvocationID}, 300<<10)
			if err != nil || status != http.StatusOK {
				return nil, errors.New("substrate bootstrap unavailable")
			}
			var bootstrap struct {
				Data struct {
					CloudInit string `json:"cloud_init"`
				} `json:"data"`
			}
			if json.Unmarshal(body, &bootstrap) != nil || bootstrap.Data.CloudInit == "" {
				return nil, errors.New("substrate bootstrap invalid")
			}
			return []byte(bootstrap.Data.CloudInit), nil
		})
	} else {
		result = c.cfg.WorkerExecutor.ExecuteWorker(ctx, *response.Data.Command)
	}
	// Retain the exact result until accepted; never re-execute the provider
	// action merely because its result response was lost.
	backoff := newRetryBackoff(time.Second, 30*time.Second)
	for ctx.Err() == nil {
		status, _, retryAfter, err = c.postJSONExchange(ctx, c.cfg.WorkerResultURL, providerControlRequest{controlRequest: identity, ProviderResult: &result}, maxResponseBody)
		if err == nil && status >= 200 && status < 300 {
			break
		}
		// A 429 is load, not a refusal of the result: it is kept and sent again.
		if err == nil && status >= 400 && status < 500 && status != http.StatusTooManyRequests {
			c.log.Warn("substrate_result_rejected", "status", status)
			break
		}
		if waitContext(ctx, max(backoff.Next(), retryAfter)) != nil {
			break
		}
	}
	return workerCycleRan, nil
}
