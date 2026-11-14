package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	productnotifications "github.com/kombifyio/techstack/internal/notifications"
	"github.com/kombifyio/techstack/pkg/jobs"
	"github.com/kombifyio/techstack/pkg/logger"
)

type destroyJobFailureNotifier struct {
	outbox stackKitLifecycleNotificationOutbox
}

func (n destroyJobFailureNotifier) ObserveTerminalJob(ctx context.Context, job jobs.JobSnapshot) error {
	if n.outbox == nil {
		return nil
	}
	if job.State != jobs.JobStateFailed {
		return nil
	}
	switch job.Type {
	case jobs.JobTypeDestroy, jobs.JobTypeReconcileLease:
	default:
		return nil
	}
	ownerID := stackKitNotificationString(job, "owner_id")
	tenantID := stackKitNotificationString(job, "tenant_id")
	if ownerID == "" {
		logger.Get().Warn("destroy_notification_context_incomplete",
			"job_id", job.ID, "tenant_id", tenantID)
		return nil
	}
	stackID := strings.TrimSpace(job.TargetID)
	// A failure without a stack has no deployment thread: it stands alone
	// instead of folding into every other unbound failure of the recipient.
	deepLink, groupKey := "/dashboard", ""
	if stackID != "" {
		groupKey = "kit_deployment:" + stackID
	}
	body := strings.TrimSpace(job.Error)
	if body == "" {
		body = strings.TrimSpace(job.Message)
	}
	if body == "" {
		body = "Decommissioning the managed runtime did not finish."
	}
	completedAt := time.Now().UTC()
	if job.CompletedAt != nil {
		completedAt = job.CompletedAt.UTC()
	}
	payload := map[string]any{
		"subject":     "Runtime decommission failed",
		"body":        body,
		"job_id":      job.ID,
		"job_type":    string(job.Type),
		"stack_id":    stackID,
		"occurred_at": completedAt.Format(time.RFC3339Nano),
		"link_url":    deepLink,
	}
	for _, channel := range []string{"in_app", "push"} {
		if err := n.outbox.Enqueue(ctx, productnotifications.ProductEvent{
			Topic:          "jobs.completed",
			Channel:        channel,
			Auth0UserID:    ownerID,
			TenantID:       tenantID,
			IdempotencyKey: fmt.Sprintf("techstack:destroy:%s:%s", job.ID, channel),
			Payload:        payload,
			SourceApp:      notificationSourceApp,
			EventKey:       "runtime.destroy.failed",
			SubjectRef:     "job:" + job.ID,
			DeepLink:       deepLink,
			GroupKey:       groupKey,
			Priority:       "high",
		}); err != nil {
			return err
		}
	}
	return nil
}
