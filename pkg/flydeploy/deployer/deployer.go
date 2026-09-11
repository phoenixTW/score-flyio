// Package deployer provides Machines API primitives over the generated client.
package deployer

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/phoenixTW/score-flyio/internal"
	"github.com/phoenixTW/score-flyio/pkg/flymachines"
)

const secretTypeString = "string"

const (
	defaultPollInterval = 500 * time.Millisecond
	defaultWaitTimeout  = 2 * time.Minute
	maxWaitRequestSec   = 60
)

// RetryPolicy controls retries for safely repeatable operations. Creates
// are never retried: a lost response may still have created the resource.
type RetryPolicy struct {
	MaxAttempts int
	Backoff     time.Duration
	MaxBackoff  time.Duration
}

// WaitOptions controls readiness polling.
type WaitOptions struct {
	Timeout      time.Duration
	PollInterval time.Duration
}

type config struct {
	retry        RetryPolicy
	pollInterval time.Duration
	waitTimeout  time.Duration
	now          func() time.Time
	sleep        func(context.Context, time.Duration) error
}

// Option customizes a Deployer for deterministic tests and alternate
// bounded polling policies.
type Option func(*config)

// WithRetryPolicy configures retries for safe operations.
func WithRetryPolicy(policy RetryPolicy) Option {
	return func(c *config) { c.retry = policy }
}

// WithClock injects the clock used for polling deadlines.
func WithClock(now func() time.Time) Option {
	return func(c *config) {
		if now != nil {
			c.now = now
		}
	}
}

// WithSleep injects the sleep function used between retries and polls.
func WithSleep(sleep func(context.Context, time.Duration) error) Option {
	return func(c *config) {
		if sleep != nil {
			c.sleep = sleep
		}
	}
}

// WithPollInterval sets the default readiness polling interval.
func WithPollInterval(interval time.Duration) Option {
	return func(c *config) {
		if interval > 0 {
			c.pollInterval = interval
		}
	}
}

// WithWaitTimeout sets the default readiness timeout.
func WithWaitTimeout(timeout time.Duration) Option {
	return func(c *config) {
		if timeout > 0 {
			c.waitTimeout = timeout
		}
	}
}

// Deployer performs lifecycle operations against one Fly app. Api is kept
// exported for compatibility with callers that need the generated client.
type Deployer struct {
	Api     flymachines.ClientWithResponsesInterface
	AppName string
	config  config
}

// New creates a deployer using the default retry and polling policy.
func New(api flymachines.ClientWithResponsesInterface, appName string) *Deployer {
	return NewWithOptions(api, appName)
}

// NewWithOptions creates a deployer with injectable timing and retry policy.
func NewWithOptions(api flymachines.ClientWithResponsesInterface, appName string, options ...Option) *Deployer {
	c := config{
		retry: RetryPolicy{
			MaxAttempts: 3,
			Backoff:     100 * time.Millisecond,
			MaxBackoff:  time.Second,
		},
		pollInterval: defaultPollInterval,
		waitTimeout:  defaultWaitTimeout,
		now:          time.Now,
		sleep:        sleep,
	}
	for _, option := range options {
		if option != nil {
			option(&c)
		}
	}
	if c.retry.MaxAttempts < 1 {
		c.retry.MaxAttempts = 1
	}
	if c.retry.Backoff < 0 {
		c.retry.Backoff = 0
	}
	if c.retry.MaxBackoff <= 0 {
		c.retry.MaxBackoff = c.retry.Backoff
	}
	return &Deployer{Api: api, AppName: appName, config: c}
}

// LookupApp returns found=false for a missing app. A missing app is not an
// error because this is the normal first step of idempotent reconciliation.
func (d *Deployer) LookupApp(ctx context.Context, appName string) (*flymachines.App, bool, error) {
	app, err := retryValue(ctx, d.config, "apps lookup", func(requestContext context.Context) (*flymachines.App, int, error) {
		resp, err := d.Api.AppsShowWithResponse(requestContext, appName)
		if err != nil {
			return nil, 0, requestFailure(err)
		}
		if resp.StatusCode() == http.StatusNotFound {
			return nil, http.StatusNotFound, nil
		}
		if resp.StatusCode() != http.StatusOK || resp.JSON200 == nil {
			return nil, resp.StatusCode(), responseFailure("apps lookup", resp.StatusCode())
		}
		return resp.JSON200, resp.StatusCode(), nil
	})
	if err != nil {
		return nil, false, err
	}
	return app, app != nil, nil
}

// CreateApp creates an app. It does not retry because a lost response could
// leave the app created even when the client sees an error.
func (d *Deployer) CreateApp(ctx context.Context, request flymachines.CreateAppRequest) error {
	_, err := d.createApp(ctx, request)
	return err
}

func (d *Deployer) createApp(ctx context.Context, request flymachines.CreateAppRequest) (int, error) {
	resp, err := d.Api.AppsCreateWithResponse(ctx, request)
	if err != nil {
		return 0, requestFailure(err)
	}
	if resp.StatusCode() != http.StatusCreated {
		return resp.StatusCode(), responseFailure("apps create", resp.StatusCode())
	}
	return resp.StatusCode(), nil
}

// EnsureApp looks up an app before creating it. A concurrent create that
// returns a conflict is reconciled by looking the app up again.
func (d *Deployer) EnsureApp(ctx context.Context, request flymachines.CreateAppRequest) (*flymachines.App, error) {
	if request.AppName == nil || *request.AppName == "" {
		return nil, fmt.Errorf("apps ensure: app name is required")
	}
	app, found, err := d.LookupApp(ctx, *request.AppName)
	if err != nil {
		return nil, err
	}
	if found {
		return app, nil
	}
	status, err := d.createApp(ctx, request)
	if err != nil && status != http.StatusConflict {
		return nil, err
	}
	app, found, err = d.LookupApp(ctx, *request.AppName)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("apps ensure: app was not found after create")
	}
	return app, nil
}

// SetSecrets creates or updates each app secret with one Machines API call
// per key in sorted order, keeping values out of returned errors.
func (d *Deployer) SetSecrets(ctx context.Context, secrets map[string]string) error {
	for _, key := range slices.Sorted(maps.Keys(secrets)) {
		if err := d.createSecret(ctx, key, secrets[key]); err != nil {
			return err
		}
	}
	return nil
}

func (d *Deployer) createSecret(ctx context.Context, key, secretValue string) error {
	valueBytes := make([]int, len(secretValue))
	for i := 0; i < len(secretValue); i++ {
		valueBytes[i] = int(secretValue[i])
	}
	resp, err := d.Api.SecretCreateWithResponse(ctx, d.AppName, key, secretTypeString, flymachines.CreateSecretRequest{Value: &valueBytes})
	if err != nil {
		return fmt.Errorf("secrets create %q: %w", key, err)
	}
	if resp == nil {
		return fmt.Errorf("secrets create %q failed: empty response", key)
	}
	if resp.StatusCode() != http.StatusCreated {
		return fmt.Errorf("secrets create %q failed with status %d", key, resp.StatusCode())
	}
	return nil
}

// DeleteApp deletes an app and treats an already missing app as success.
func (d *Deployer) DeleteApp(ctx context.Context, appName string) error {
	return d.retry(ctx, "apps delete", func(requestContext context.Context) (int, error) {
		resp, err := d.Api.AppsDeleteWithResponse(requestContext, appName)
		if err != nil {
			return 0, requestFailure(err)
		}
		if resp.StatusCode() == http.StatusNotFound || resp.StatusCode() == http.StatusAccepted {
			return resp.StatusCode(), nil
		}
		return resp.StatusCode(), responseFailure("apps delete", resp.StatusCode())
	})
}

// ListMachines lists machines in the configured app.
func (d *Deployer) ListMachines(ctx context.Context) ([]flymachines.Machine, error) {
	return d.ListMachinesWithParams(ctx, nil)
}

// ListMachinesWithParams lists machines with generated-client filters.
func (d *Deployer) ListMachinesWithParams(ctx context.Context, params *flymachines.MachinesListParams) ([]flymachines.Machine, error) {
	machines, err := retryValue(ctx, d.config, "machines list", func(requestContext context.Context) ([]flymachines.Machine, int, error) {
		resp, err := d.Api.MachinesListWithResponse(requestContext, d.AppName, params)
		if err != nil {
			return nil, 0, requestFailure(err)
		}
		if resp.StatusCode() != http.StatusOK || resp.JSON200 == nil {
			return nil, resp.StatusCode(), responseFailure("machines list", resp.StatusCode())
		}
		return *resp.JSON200, resp.StatusCode(), nil
	})
	return machines, err
}

// GetMachine returns found=false for a missing machine.
func (d *Deployer) GetMachine(ctx context.Context, machineID string) (*flymachines.Machine, bool, error) {
	var machine *flymachines.Machine
	err := d.retry(ctx, "machines show", func(requestContext context.Context) (int, error) {
		resp, err := d.Api.MachinesShowWithResponse(requestContext, d.AppName, machineID)
		if err != nil {
			return 0, requestFailure(err)
		}
		if resp.StatusCode() == http.StatusNotFound {
			machine = nil
			return http.StatusNotFound, nil
		}
		if resp.StatusCode() != http.StatusOK || resp.JSON200 == nil {
			return resp.StatusCode(), responseFailure("machines show", resp.StatusCode())
		}
		machine = resp.JSON200
		return resp.StatusCode(), nil
	})
	return machine, machine != nil, err
}

// ShowMachine returns an error when the machine does not exist.
func (d *Deployer) ShowMachine(ctx context.Context, machineID string) (*flymachines.Machine, error) {
	machine, found, err := d.GetMachine(ctx, machineID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("machines show: machine not found")
	}
	return machine, nil
}

// CreateMachine creates a machine. It is intentionally not retried.
func (d *Deployer) CreateMachine(ctx context.Context, name string, region string, config flymachines.FlyMachineConfig) (*flymachines.Machine, error) {
	request := flymachines.CreateMachineRequest{Config: &config}
	if name != "" {
		request.Name = &name
	}
	if region != "" {
		request.Region = &region
	}
	resp, err := d.Api.MachinesCreateWithResponse(ctx, d.AppName, request)
	if err != nil {
		return nil, requestFailure(err)
	}
	if resp.StatusCode() != http.StatusOK || resp.JSON200 == nil {
		return nil, responseFailure("machines create", resp.StatusCode())
	}
	return resp.JSON200, nil
}

// UpdateMachine updates a machine in place. It is not retried because the
// current version may intentionally make the update fail after a race.
func (d *Deployer) UpdateMachine(ctx context.Context, machineID string, currentVersion string, name string, config flymachines.FlyMachineConfig) (*flymachines.Machine, error) {
	request := flymachines.UpdateMachineRequest{Config: &config}
	if currentVersion != "" {
		request.CurrentVersion = &currentVersion
	}
	if name != "" {
		request.Name = &name
	}
	resp, err := d.Api.MachinesUpdateWithResponse(ctx, d.AppName, machineID, request)
	if err != nil {
		return nil, requestFailure(err)
	}
	if resp.StatusCode() != http.StatusOK || resp.JSON200 == nil {
		return nil, responseFailure("machines update", resp.StatusCode())
	}
	return resp.JSON200, nil
}

// DeleteMachine deletes a machine and treats an already missing machine as
// success, making cleanup safe to repeat after a partial deployment.
func (d *Deployer) DeleteMachine(ctx context.Context, machineID string, force bool) error {
	return d.retry(ctx, "machines delete", func(requestContext context.Context) (int, error) {
		resp, err := d.Api.MachinesDeleteWithResponse(requestContext, d.AppName, machineID, &flymachines.MachinesDeleteParams{Force: &force})
		if err != nil {
			return 0, requestFailure(err)
		}
		if resp.StatusCode() == http.StatusNotFound || resp.StatusCode() == http.StatusOK || resp.StatusCode() == http.StatusAccepted {
			return resp.StatusCode(), nil
		}
		return resp.StatusCode(), responseFailure("machines delete", resp.StatusCode())
	})
}

// StopMachine stops a machine.
func (d *Deployer) StopMachine(ctx context.Context, machineID string) error {
	resp, err := d.Api.MachinesStopWithResponse(ctx, d.AppName, machineID, flymachines.StopRequest{})
	return actionResult("machines stop", resp, err)
}

// SuspendMachine suspends a machine.
func (d *Deployer) SuspendMachine(ctx context.Context, machineID string) error {
	resp, err := d.Api.MachinesSuspendWithResponse(ctx, d.AppName, machineID)
	return actionResult("machines suspend", resp, err)
}

// ResumeMachine resumes a machine using the generated start operation.
func (d *Deployer) ResumeMachine(ctx context.Context, machineID string) error {
	return d.StartMachine(ctx, machineID)
}

// StartMachine starts a machine.
func (d *Deployer) StartMachine(ctx context.Context, machineID string) error {
	resp, err := d.Api.MachinesStartWithResponse(ctx, d.AppName, machineID)
	return actionResult("machines start", resp, err)
}

// RestartMachine restarts a machine.
func (d *Deployer) RestartMachine(ctx context.Context, machineID string) error {
	return d.RestartMachineWithParams(ctx, machineID, nil)
}

// RestartMachineWithParams restarts a machine with generated-client options.
func (d *Deployer) RestartMachineWithParams(ctx context.Context, machineID string, params *flymachines.MachinesRestartParams) error {
	resp, err := d.Api.MachinesRestartWithResponse(ctx, d.AppName, machineID, params)
	return actionResult("machines restart", resp, err)
}

// WaitForState waits via the wait endpoint and verifies state by inspecting
// the machine; a successful endpoint response alone is not readiness proof.
func (d *Deployer) WaitForState(ctx context.Context, machineID string, state flymachines.MachinesWaitParamsState, timeout time.Duration) error {
	if timeout <= 0 {
		return fmt.Errorf("machines wait: timeout must be positive")
	}
	_, err := d.waitForState(ctx, machineID, state, d.config.now().Add(timeout))
	return err
}

// WaitReady waits for started state and then polls all machine checks until
// every reported check is passing.
func (d *Deployer) WaitReady(ctx context.Context, machineID string, options WaitOptions) (*flymachines.Machine, error) {
	if options.Timeout <= 0 {
		options.Timeout = d.config.waitTimeout
	}
	if options.PollInterval <= 0 {
		options.PollInterval = d.config.pollInterval
	}
	if options.Timeout <= 0 {
		return nil, fmt.Errorf("machines ready: timeout must be positive")
	}
	deadline := d.config.now().Add(options.Timeout)
	machine, err := d.waitForState(ctx, machineID, flymachines.MachinesWaitParamsStateStarted, deadline)
	if err != nil {
		return nil, err
	}
	return d.waitForHealthyMachine(ctx, machineID, machine, deadline, options.PollInterval)
}

// WaitForHealthy polls a machine until it is started and all checks pass.
func (d *Deployer) WaitForHealthy(ctx context.Context, machineID string, timeout time.Duration, interval time.Duration) error {
	if timeout <= 0 {
		return fmt.Errorf("machines healthy: timeout must be positive")
	}
	if interval <= 0 {
		interval = d.config.pollInterval
	}
	_, err := d.waitForHealthyMachine(ctx, machineID, nil, d.config.now().Add(timeout), interval)
	return err
}

// WaitExit polls a one-off machine until it stops and returns its exit code.
func (d *Deployer) WaitExit(ctx context.Context, machineID string, timeout time.Duration, interval time.Duration) (int, error) {
	if timeout <= 0 {
		timeout = d.config.waitTimeout
	}
	if interval <= 0 {
		interval = d.config.pollInterval
	}
	if timeout <= 0 {
		return -1, fmt.Errorf("machines exit: timeout must be positive")
	}
	deadline := d.config.now().Add(timeout)
	for {
		machine, found, err := d.GetMachine(ctx, machineID)
		if err != nil {
			return -1, err
		}
		if found && machine != nil {
			state := internal.DerefOr(machine.State, "")
			if state == "destroyed" || state == "stopped" {
				return d.exitCode(ctx, machineID)
			}
		}
		if err := d.pause(ctx, deadline, interval); err != nil {
			return -1, err
		}
	}
}

func (d *Deployer) exitCode(ctx context.Context, machineID string) (int, error) {
	events, err := d.ListEvents(ctx, machineID)
	if err != nil {
		return -1, err
	}
	best := -1
	bestStamp := -1
	for _, event := range events {
		if internal.DerefOr(event.Type, "") != "exit" || event.Request == nil {
			continue
		}
		raw, ok := (*event.Request)["exit_code"]
		if !ok {
			continue
		}
		var code int
		switch value := raw.(type) {
		case float64:
			code = int(value)
		case int:
			code = value
		default:
			continue
		}
		stamp := -1
		if event.Timestamp != nil {
			stamp = *event.Timestamp
		}
		if stamp >= bestStamp {
			best = code
			bestStamp = stamp
		}
	}
	if best < 0 {
		return -1, fmt.Errorf("machines exit: no exit event for machine %s", machineID)
	}
	return best, nil
}

func (d *Deployer) waitForState(ctx context.Context, machineID string, state flymachines.MachinesWaitParamsState, deadline time.Time) (*flymachines.Machine, error) {
	for {
		remaining := deadline.Sub(d.config.now())
		if remaining <= 0 {
			return nil, fmt.Errorf("machines wait: timed out waiting for state %s", state)
		}
		seconds := int((remaining + time.Second - 1) / time.Second)
		if seconds < 1 {
			seconds = 1
		}
		if seconds > maxWaitRequestSec {
			seconds = maxWaitRequestSec
		}
		err := d.retry(ctx, "machines wait", func(requestContext context.Context) (int, error) {
			callContext, cancel := context.WithTimeout(requestContext, minDuration(remaining, time.Duration(seconds)*time.Second))
			defer cancel()
			resp, err := d.Api.MachinesWaitWithResponse(callContext, d.AppName, machineID, &flymachines.MachinesWaitParams{State: &state, Timeout: &seconds})
			if err != nil {
				return 0, requestFailure(err)
			}
			if resp.StatusCode() != http.StatusOK {
				return resp.StatusCode(), responseFailure("machines wait", resp.StatusCode())
			}
			return resp.StatusCode(), nil
		})
		if err != nil {
			return nil, err
		}
		machine, found, err := d.GetMachine(ctx, machineID)
		if err != nil {
			return nil, err
		}
		if found && machine != nil && internal.DerefOr(machine.State, "") == string(state) {
			return machine, nil
		}
		if err := d.pause(ctx, deadline, d.config.pollInterval); err != nil {
			return nil, err
		}
	}
}

func (d *Deployer) waitForHealthyMachine(ctx context.Context, machineID string, initial *flymachines.Machine, deadline time.Time, interval time.Duration) (*flymachines.Machine, error) {
	machine := initial
	for {
		if machine == nil {
			var found bool
			var err error
			machine, found, err = d.GetMachine(ctx, machineID)
			if err != nil {
				return nil, err
			}
			if !found {
				machine = nil
			}
		}
		if machine != nil && internal.DerefOr(machine.State, "") == "started" && checksPassing(machine) {
			return machine, nil
		}
		if d.config.now().After(deadline) {
			return nil, fmt.Errorf("machines healthy: timed out")
		}
		if err := d.pause(ctx, deadline, interval); err != nil {
			return nil, err
		}
		machine = nil
	}
}

func checksPassing(machine *flymachines.Machine) bool {
	if machine.Checks == nil {
		return true
	}
	for _, check := range *machine.Checks {
		if internal.DerefOr(check.Status, "") != "passing" {
			return false
		}
	}
	return true
}

func actionResult[T interface{ StatusCode() int }](operation string, response *T, err error) error {
	if err != nil {
		return requestFailure(err)
	}
	if response == nil {
		return fmt.Errorf("%s failed", operation)
	}
	status := (*response).StatusCode()
	if status != http.StatusOK {
		return responseFailure(operation, status)
	}
	return nil
}

func (d *Deployer) retry(ctx context.Context, operation string, call func(context.Context) (int, error)) error {
	_, err := retryValue(ctx, d.config, operation, func(requestContext context.Context) (struct{}, int, error) {
		status, err := call(requestContext)
		return struct{}{}, status, err
	})
	return err
}

func retryValue[T any](ctx context.Context, c config, operation string, call func(context.Context) (T, int, error)) (T, error) {
	var zero T
	policy := c.retry
	for attempt := 1; attempt <= policy.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		value, status, err := call(ctx)
		if err == nil {
			return value, nil
		}
		if !retryable(status) || attempt == policy.MaxAttempts {
			return zero, fmt.Errorf("%s: %w", operation, err)
		}
		backoff := policy.Backoff
		for retryAttempt := 1; retryAttempt < attempt; retryAttempt++ {
			backoff *= 2
		}
		if policy.MaxBackoff > 0 && backoff > policy.MaxBackoff {
			backoff = policy.MaxBackoff
		}
		if err := c.sleep(ctx, backoff); err != nil {
			return zero, err
		}
	}
	return zero, fmt.Errorf("%s: request failed", operation)
}

func (d *Deployer) pause(ctx context.Context, deadline time.Time, interval time.Duration) error {
	remaining := deadline.Sub(d.config.now())
	if remaining <= 0 {
		return fmt.Errorf("machines wait: timed out")
	}
	if interval > remaining {
		interval = remaining
	}
	return d.sleep(ctx, interval)
}

func (d *Deployer) sleep(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	return d.config.sleep(ctx, delay)
}

func sleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func retryable(status int) bool {
	switch status {
	case 0, http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func requestFailure(err error) error {
	return fmt.Errorf("request failed: %w", err)
}

func responseFailure(operation string, status int) error {
	return fmt.Errorf("%s failed with status %d", operation, status)
}

func minDuration(first, second time.Duration) time.Duration {
	if first < second {
		return first
	}
	return second
}

// ListEvents lists machine events for compatibility with existing callers.
func (d *Deployer) ListEvents(ctx context.Context, machineID string) ([]flymachines.MachineEvent, error) {
	events, err := retryValue(ctx, d.config, "machines events", func(requestContext context.Context) ([]flymachines.MachineEvent, int, error) {
		resp, err := d.Api.MachinesListEventsWithResponse(requestContext, d.AppName, machineID)
		if err != nil {
			return nil, 0, requestFailure(err)
		}
		if resp.StatusCode() != http.StatusOK || resp.JSON200 == nil {
			return nil, resp.StatusCode(), responseFailure("machines events", resp.StatusCode())
		}
		return *resp.JSON200, resp.StatusCode(), nil
	})
	return events, err
}

// MachinesByGroup filters machines by a metadata key/value pair.
func MachinesByGroup(machines []flymachines.Machine, groupMetaKey string, group string) []flymachines.Machine {
	var out []flymachines.Machine
	for _, machine := range machines {
		if machine.Config == nil || machine.Config.Metadata == nil {
			continue
		}
		if value, ok := (*machine.Config.Metadata)[groupMetaKey]; ok && value == group {
			out = append(out, machine)
		}
	}
	return out
}
