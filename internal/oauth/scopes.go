package oauth

import "strings"

// Scope limits what a connection may do. A tool is available only when the
// connection holds its scope, and even then the user's role must allow the
// underlying action: effective access is role permissions ∩ scopes.
type Scope string

const (
	ScopeMessagesRead   Scope = "messages:read"   // contacts, conversations, templates, message logs, stats
	ScopeMessagesSend   Scope = "messages:send"   // send messages, create and tag contacts
	ScopeCampaignsRead  Scope = "campaigns:read"  // campaigns and their recipients
	ScopeCampaignsWrite Scope = "campaigns:write" // create, fill, start, pause and cancel campaigns
	ScopeChatbotRead    Scope = "chatbot:read"    // keyword rules, AI contexts, chatbot flows and settings
	ScopeChatbotWrite   Scope = "chatbot:write"   // create and edit them
	ScopeWebhooksRead   Scope = "webhooks:read"   // outbound webhooks and their delivery logs
	ScopeWebhooksWrite  Scope = "webhooks:write"  // create, edit and test outbound webhooks
)

// AllScopes is every scope, in the order they are presented to users.
var AllScopes = []Scope{
	ScopeMessagesRead,
	ScopeMessagesSend,
	ScopeCampaignsRead,
	ScopeCampaignsWrite,
	ScopeChatbotRead,
	ScopeChatbotWrite,
	ScopeWebhooksRead,
	ScopeWebhooksWrite,
}

func knownScope(s Scope) bool {
	for _, known := range AllScopes {
		if s == known {
			return true
		}
	}
	return false
}

// ParseScopes parses a space-separated scope parameter. An empty parameter
// means every scope. Unknown scopes are an invalid_scope error.
func ParseScopes(param string) ([]Scope, error) {
	fields := strings.Fields(param)
	if len(fields) == 0 {
		return append([]Scope(nil), AllScopes...), nil
	}
	var scopes []Scope
	for _, f := range fields {
		s := Scope(f)
		if !knownScope(s) {
			return nil, oauthErr(ErrInvalidScope, "unknown scope "+f)
		}
		scopes = appendUnique(scopes, s)
	}
	return scopes, nil
}

// Intersect returns the scopes in a that are also in b, in AllScopes order.
func Intersect(a, b []Scope) []Scope {
	var out []Scope
	for _, s := range AllScopes {
		if contains(a, s) && contains(b, s) {
			out = append(out, s)
		}
	}
	return out
}

// Join formats scopes as a space-separated parameter.
func Join(scopes []Scope) string {
	names := make([]string, len(scopes))
	for i, s := range scopes {
		names[i] = string(s)
	}
	return strings.Join(names, " ")
}

// Split parses a stored space-separated scope list, dropping unknown scopes.
func Split(stored string) []Scope {
	var scopes []Scope
	for _, f := range strings.Fields(stored) {
		if knownScope(Scope(f)) {
			scopes = appendUnique(scopes, Scope(f))
		}
	}
	return scopes
}

func contains(scopes []Scope, s Scope) bool {
	for _, x := range scopes {
		if x == s {
			return true
		}
	}
	return false
}

func appendUnique(scopes []Scope, s Scope) []Scope {
	if contains(scopes, s) {
		return scopes
	}
	return append(scopes, s)
}
