package sybra

import (
	"fmt"
	"log/slog"
	"slices"

	"github.com/Automaat/sybra/internal/attachment"
	"github.com/Automaat/sybra/internal/task"
)

// ClusterAttachmentService exposes task attachment blob transfer over the
// HTTP control plane only. It is intentionally not registered as a Wails
// desktop service.
type ClusterAttachmentService struct {
	tasks       *task.Manager
	attachments *attachment.Store
	logger      *slog.Logger
}

func (s *ClusterAttachmentService) ExportAttachment(taskID, attachmentID string) ([]byte, error) {
	if s == nil || s.tasks == nil || s.attachments == nil {
		return nil, validationError("attachments are unavailable")
	}
	t, err := s.tasks.Get(taskID)
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(t.Attachments, func(att task.Attachment) bool { return att.ID == attachmentID }) {
		return nil, validationError(fmt.Sprintf("attachment %q not found", attachmentID))
	}
	data, _, err := s.attachments.Content(taskID, attachmentID)
	if err != nil {
		return nil, validationError(err.Error())
	}
	return data, nil
}

func (s *ClusterAttachmentService) ImportAttachment(taskID string, meta task.Attachment, data []byte) (task.Attachment, error) {
	if s == nil || s.attachments == nil {
		return task.Attachment{}, validationError("attachments are unavailable")
	}
	local, err := s.attachments.Import(taskID, meta, data)
	if err != nil {
		return task.Attachment{}, validationError(err.Error())
	}
	if s.logger != nil {
		s.logger.Debug("cluster.attachment.imported", "task_id", taskID, "attachment_id", local.ID, "size", local.SizeBytes)
	}
	return local, nil
}
