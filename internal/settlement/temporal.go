package settlement

import (
	"context"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
)

// Starter kicks off settlement for a withdrawal. The API calls it right
// after the hold is posted, and again on every idempotent retry, so it must
// tolerate being called many times for the same transfer.
type Starter interface {
	StartWithdrawal(ctx context.Context, transferID string) error
}

// TemporalStarter starts WithdrawalWorkflow on a Temporal cluster.
type TemporalStarter struct {
	Client    client.Client
	TaskQueue string
}

func (s TemporalStarter) StartWithdrawal(ctx context.Context, transferID string) error {
	q := s.TaskQueue
	if q == "" {
		q = TaskQueue
	}
	_, err := s.Client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        WorkflowID(transferID),
		TaskQueue: q,
		// A finished workflow for this transfer means it is already settled
		// or failed; never run it a second time.
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
		// A running one: just attach to it.
		WorkflowIDConflictPolicy: enumspb.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
	}, WithdrawalWorkflow, WithdrawalInput{TransferID: transferID})
	if temporal.IsWorkflowExecutionAlreadyStartedError(err) {
		return nil
	}
	return err
}

// NewWorker registers the workflow and activities on a task queue (empty
// means TaskQueue).
func NewWorker(c client.Client, queue string, acts *Activities, opts worker.Options) worker.Worker {
	if queue == "" {
		queue = TaskQueue
	}
	w := worker.New(c, queue, opts)
	w.RegisterWorkflow(WithdrawalWorkflow)
	w.RegisterActivity(acts)
	return w
}
