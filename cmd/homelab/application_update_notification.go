package main

import (
	"context"
	"fmt"
	"strings"

	productnotifications "github.com/kombifyio/homelab/internal/notifications"
	"github.com/kombifyio/homelab/pkg/jobs"
)

const applicationUpdateNotificationTopic = "application.update"

// observeApplicationUpdate notifies the owner who started a single-application
// update when it could not return to the prior release (rollback_failed) or
// its outcome on the node is unknown (unresolved): the application may be
// stopped or on an unverified release and needs the owner. An update that
// succeeded or rolled back cleanly is shown on the application itself.
func (n stackKitLifecycleNotifier) observeApplicationUpdate(ctx context.Context, job jobs.JobSnapshot) error {
	if job.State != jobs.JobStateCompleted && job.State != jobs.JobStateFailed {
		return nil
	}
	outcome, _ := job.Result[jobs.ApplicationUpdateResultField].(map[string]any)
	result := strings.TrimSpace(fmt.Sprint(outcome["result"]))
	if result != jobs.ApplicationUpdateRollbackFailed && result != jobs.ApplicationUpdateUnresolved {
		return nil
	}
	tenantID, ownerID := stackKitNotificationString(job, "tenant_id"), stackKitNotificationString(job, "owner_id")
	serviceID, useCase := stackKitNotificationString(job, "service_id"), stackKitNotificationString(job, "use_case")
	if tenantID == "" || ownerID == "" || serviceID == "" {
		return nil
	}
	deepLink := "/services"
	messageID, body := "services.applicationUpdateRollbackFailed", "The update could not return the application to its previous version. Open the application to see what happened."
	if result == jobs.ApplicationUpdateUnresolved {
		messageID, body = "services.applicationUpdateUnresolved", "Homelab Runtime lost the answer of the application update. Open the application and check which version runs."
	}
	payload := map[string]any{
		"subject": "Application update needs you", "message_id": messageID, "severity": "critical", "body": body,
		"service_id": serviceID, "use_case": useCase, "job_id": strings.TrimSpace(job.ID), "link_url": deepLink,
		"target_version": stackKitNotificationString(job, "application_version"), "reason_code": strings.TrimSpace(fmt.Sprint(outcome["reason_code"])),
	}
	for _, channel := range []string{"in_app", "push"} {
		if err := n.outbox.Enqueue(ctx, productnotifications.ProductEvent{
			Topic: applicationUpdateNotificationTopic, Channel: channel, Auth0UserID: ownerID, TenantID: tenantID,
			IdempotencyKey: fmt.Sprintf("homelab:application-update:%s:%s:%s", job.ID, result, channel),
			Payload:        payload, SourceApp: notificationSourceApp, EventKey: "application.update." + result,
			SubjectRef: "service:" + serviceID, DeepLink: deepLink, GroupKey: "homelab:application-update:" + serviceID, Priority: "high",
		}); err != nil {
			return err
		}
	}
	return nil
}
