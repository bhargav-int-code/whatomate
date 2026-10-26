package mcpserver

// Scope limits what a connection may do. A tool is available only when the
// connection holds its scope, and even then the user's role must allow the
// underlying action: effective access is role permissions ∩ scopes.
type Scope string

const (
	ScopeMessagesRead   Scope = "messages:read"   // contacts, conversations, templates, message logs, stats
	ScopeMessagesSend   Scope = "messages:send"   // send messages, create and tag contacts
	ScopeCampaignsRead  Scope = "campaigns:read"  // campaigns and their recipients
	ScopeCampaignsWrite Scope = "campaigns:write" // create, fill, start, pause and cancel campaigns
	ScopeWebhooksRead   Scope = "webhooks:read"   // outbound webhook delivery logs
)

// allScopes is every scope, in the order they are presented to users.
var allScopes = []Scope{
	ScopeMessagesRead,
	ScopeMessagesSend,
	ScopeCampaignsRead,
	ScopeCampaignsWrite,
	ScopeWebhooksRead,
}

func scopeNames(scopes []Scope) []string {
	names := make([]string, len(scopes))
	for i, s := range scopes {
		names[i] = string(s)
	}
	return names
}
