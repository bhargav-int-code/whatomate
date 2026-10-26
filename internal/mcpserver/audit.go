package mcpserver

import (
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/audit"
	"github.com/shridarpatil/whatomate/internal/models"
)

// Audit log resource types for MCP write tools.
const (
	auditContacts  = models.ResourceContacts
	auditCampaigns = models.ResourceCampaigns
	auditMessages  = "messages"
)

// audit records a write tool's action in the organization's audit log, so it
// is clear which changes an AI assistant made and through which connection.
func (c *call) audit(resourceType, resourceID string, action models.AuditAction, toolName string, details map[string]any) {
	id, err := uuid.Parse(resourceID)
	if err != nil {
		return
	}
	changes := []map[string]any{
		{"field": "via", "old_value": nil, "new_value": "AI assistant (MCP), " + c.p.Client},
		{"field": "tool", "old_value": nil, "new_value": "whatomate_" + toolName},
	}
	keys := make([]string, 0, len(details))
	for k := range details {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		changes = append(changes, map[string]any{"field": k, "old_value": nil, "new_value": fmt.Sprint(details[k])})
	}
	audit.LogAudit(c.s.app.DB, c.p.OrgID, c.p.UserID, audit.GetUserName(c.s.app.DB, c.p.UserID),
		resourceType, id, action, nil, nil, changes...)
}
