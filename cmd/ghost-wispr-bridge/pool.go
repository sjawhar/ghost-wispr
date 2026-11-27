package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"google.golang.org/api/googleapi"
	runapi "google.golang.org/api/run/v2"
)

// poolAPI sets and reads the call pool's instance count.
type poolAPI interface {
	// SetCount changes the count and waits, within its bound, for Cloud
	// Run's long-running operation to end. A refused patch, a failed
	// operation or a refused read of it is a *poolError; a change still
	// pending at the bound is errPoolChangePending.
	SetCount(ctx context.Context, n int64) error
	Count(ctx context.Context) (int64, error)
}

// countMask is the one field a count change patches.
const countMask = "scaling.manualInstanceCount"

const (
	// poolChangeWait bounds a count change, its patch and its operation, so
	// the pass that makes it can still read the count back within
	// watchPassTimeout.
	poolChangeWait = 15 * time.Second
	// poolPollEvery is how often a count change reads its pending operation.
	poolPollEvery = time.Second
)

// errPoolChangePending is a count change whose operation had not ended at
// its bound: Cloud Run has not confirmed it.
var errPoolChangePending = errors.New("the count change's operation had not ended")

// runPool is the poolAPI of one Cloud Run worker pool, through the Admin API
// v2 (plan D17).
type runPool struct {
	svc  *runapi.Service
	name string // projects/<project>/locations/<region>/workerPools/<pool>
	// wait and poll are poolChangeWait and poolPollEvery; a test shortens
	// them.
	wait, poll time.Duration
}

func newRunPool(svc *runapi.Service, name string) *runPool {
	return &runPool{svc: svc, name: name, wait: poolChangeWait, poll: poolPollEvery}
}

// SetCount patches the pool's manual instance count, and only it (update
// mask scaling.manualInstanceCount). The field is int64 with omitempty, so
// ForceSendFields sends a 0. The patch starts a long-running operation, and
// Cloud Run keeps the old count until it ends: SetCount reads it every
// p.poll until it ends or p.wait passes.
func (p *runPool) SetCount(ctx context.Context, n int64) error {
	ctx, cancel := context.WithTimeout(ctx, p.wait)
	defer cancel()
	pool := &runapi.GoogleCloudRunV2WorkerPool{Scaling: &runapi.GoogleCloudRunV2WorkerPoolScaling{
		ManualInstanceCount: n,
		ForceSendFields:     []string{"ManualInstanceCount"},
	}}
	op, err := p.svc.Projects.Locations.WorkerPools.Patch(p.name, pool).UpdateMask(countMask).Context(ctx).Do()
	if err != nil {
		return refusal("set the pool's count", err)
	}
	poll := time.NewTimer(p.poll)
	defer poll.Stop()
	for !op.Done {
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %w", errPoolChangePending, ctx.Err())
		case <-poll.C:
		}
		op, err = p.svc.Projects.Locations.Operations.Get(op.Name).Context(ctx).Do()
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("%w: %w", errPoolChangePending, ctx.Err())
			}
			return refusal("read the count change's operation", err)
		}
		poll.Reset(p.poll)
	}
	if op.Error != nil {
		return operationError(op.Error)
	}
	return nil
}

// Count reads the pool's manual instance count; Cloud Run leaves a 0 out.
func (p *runPool) Count(ctx context.Context) (int64, error) {
	pool, err := p.svc.Projects.Locations.WorkerPools.Get(p.name).Context(ctx).Do()
	if err != nil {
		return 0, refusal("read the pool's count", err)
	}
	if pool.Scaling == nil {
		return 0, nil
	}
	return pool.Scaling.ManualInstanceCount, nil
}

// poolError is Cloud Run's refusal of a call, or the failure of a count
// change's operation: its HTTP status and, when Google names one, the
// permission the caller lacks.
type poolError struct {
	Status     int
	Permission string
	err        error
}

func (e *poolError) Error() string {
	return fmt.Sprintf("status %d, permission %q: %v", e.Status, e.Permission, e.err)
}

func (e *poolError) Unwrap() error { return e.err }

// refusal gives err as a *poolError when Cloud Run answered it.
func refusal(what string, err error) error {
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) {
		return fmt.Errorf("%s: %w", what, err)
	}
	return &poolError{Status: gerr.Code, Permission: deniedPermission(gerr.Details, gerr.Message), err: fmt.Errorf("%s: %w", what, err)}
}

// operationError gives the google.rpc.Status a count change's operation
// ended with as a *poolError: the HTTP status of its gRPC code and the
// permission it names. The error keeps none of the status's message.
func operationError(s *runapi.GoogleRpcStatus) error {
	details := make([]any, 0, len(s.Details))
	for _, raw := range s.Details {
		var d map[string]any
		if json.Unmarshal(raw, &d) == nil {
			details = append(details, d)
		}
	}
	return &poolError{
		Status:     rpcHTTPStatus(s.Code),
		Permission: deniedPermission(details, s.Message),
		err:        fmt.Errorf("the count change's operation failed with code %d", s.Code),
	}
}

// rpcHTTPStatus gives the HTTP status of a google.rpc.Code, as
// google/rpc/code.proto maps each; 500 for a code it does not name.
func rpcHTTPStatus(code int64) int {
	statuses := [...]int{1: 499, 2: 500, 3: 400, 4: 504, 5: 404, 6: 409, 7: 403, 8: 429, 9: 400, 10: 409, 11: 400, 12: 501, 13: 500, 14: 503, 15: 500, 16: 401}
	if code < 1 || code >= int64(len(statuses)) {
		return 500
	}
	return statuses[code]
}

// deniedPermissionText is how an IAM refusal's message names the permission.
var deniedPermissionText = regexp.MustCompile(`Permission '([^']+)' denied`)

// permissionName is the form of an IAM permission's name,
// <service>.<resource>.<verb>, such as iam.serviceAccounts.actAs. A log line
// carries a permission only in this form, so no other text of an answer
// reaches it.
var permissionName = regexp.MustCompile(`^[a-z][a-zA-Z0-9]{0,62}(\.[a-zA-Z][a-zA-Z0-9]{0,62}){2,3}$`)

// deniedPermission gives the permission a refusal names: the "permission"
// metadata of its google.rpc.ErrorInfo detail (Google's error model,
// AIP-193), else the permission its message names, else "". Either must have
// a permission's form.
func deniedPermission(details []any, message string) string {
	for _, d := range details {
		detail, ok := d.(map[string]any)
		if !ok || detail["@type"] != "type.googleapis.com/google.rpc.ErrorInfo" {
			continue
		}
		if metadata, ok := detail["metadata"].(map[string]any); ok {
			if permission, ok := metadata["permission"].(string); ok && permissionName.MatchString(permission) {
				return permission
			}
		}
	}
	if m := deniedPermissionText.FindStringSubmatch(message); len(m) == 2 && permissionName.MatchString(m[1]) {
		return m[1]
	}
	return ""
}
