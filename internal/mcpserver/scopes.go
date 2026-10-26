package mcpserver

import "github.com/shridarpatil/whatomate/internal/oauth"

// Scope limits what a connection may do; see oauth.Scope.
type Scope = oauth.Scope

const (
	ScopeMessagesRead   = oauth.ScopeMessagesRead
	ScopeMessagesSend   = oauth.ScopeMessagesSend
	ScopeCampaignsRead  = oauth.ScopeCampaignsRead
	ScopeCampaignsWrite = oauth.ScopeCampaignsWrite
	ScopeWebhooksRead   = oauth.ScopeWebhooksRead
)

// allScopes is every scope, in the order they are presented to users.
var allScopes = oauth.AllScopes

func scopeNames(scopes []Scope) []string {
	names := make([]string, len(scopes))
	for i, s := range scopes {
		names[i] = string(s)
	}
	return names
}
