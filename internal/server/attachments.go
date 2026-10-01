package server

import (
	"net/http"
	"strings"

	"github.com/marcioapm/lux/internal/spec"
)

// takesAttachments: the adapter has somewhere to put an image (every agent
// adapter; a generic workload's stdin is not one).
func takesAttachments(adapter string) bool {
	return adapter != "" && adapter != "generic"
}

func errAttachmentsUnsupported(adapter string) error {
	return errf(http.StatusBadRequest, "attachments_unsupported",
		"the Run's adapter is %s: a plain process has nowhere to put an image", adapter)
}

// checkAttachments is a 400 invalid_attachment naming every problem (by
// index), or nil.
func checkAttachments(at string, list []spec.Attachment) error {
	problems := spec.CheckAttachments(at, list)
	if len(problems) == 0 {
		return nil
	}
	he := errf(http.StatusBadRequest, "invalid_attachment", "%s", strings.Join(problems, "; "))
	he.Details = problems
	return he
}

// withoutAttachmentData is a spec as Run views show it: its attachments'
// names and types, not their bytes, which went to the agent once.
func withoutAttachmentData(sp spec.RunSpec) spec.RunSpec {
	if len(sp.Workload.Attachments) == 0 {
		return sp
	}
	list := make([]spec.Attachment, len(sp.Workload.Attachments))
	for i, a := range sp.Workload.Attachments {
		list[i] = spec.Attachment{Name: a.Name, ContentType: a.ContentType}
	}
	sp.Workload.Attachments = list
	return sp
}
