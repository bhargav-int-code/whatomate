package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// call is one tool invocation: the REST API, as the connected user.
type call struct {
	ctx context.Context
	s   *Server
	p   *Principal
}

func (c *call) get(path string, query url.Values, out any) error {
	return c.s.dispatch.do(c.ctx, c.p, http.MethodGet, path, query, nil, out)
}

func (c *call) post(path string, body, out any) error {
	return c.s.dispatch.do(c.ctx, c.p, http.MethodPost, path, nil, body, out)
}

func (c *call) put(path string, body, out any) error {
	return c.s.dispatch.do(c.ctx, c.p, http.MethodPut, path, nil, body, out)
}

func (c *call) upload(path string, fields map[string]string, fileField, filename, mimeType string, data []byte, out any) error {
	return c.s.dispatch.upload(c.ctx, c.p, path, fields, fileField, filename, mimeType, data, out)
}

// toolFunc implements a tool. It returns markdown for the model to read and
// structured data (a struct or map, or nil) for clients that use it.
type toolFunc[In any] func(c *call, in In) (text string, data any, err error)

// tool is a registered tool and the scope it needs.
type tool struct {
	scope    Scope
	register func(srv *mcp.Server, s *Server)
}

// tools is every tool, in the order clients list them. Each tools_*.go file
// appends its tools in an init function.
var tools []tool

// defineTool adds a tool to the registry. perm is the role permission
// ("resource:action") the tool needs, checked before the tool runs. Some REST
// handlers do not check permissions themselves, so this is what keeps tools
// within the user's role.
func defineTool[In any](scope Scope, perm string, t *mcp.Tool, fn toolFunc[In]) {
	resource, action, _ := strings.Cut(perm, ":")
	tools = append(tools, tool{
		scope: scope,
		register: func(srv *mcp.Server, s *Server) {
			mcp.AddTool(srv, t, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
				var p *Principal
				if req.Extra != nil {
					p = principalFrom(req.Extra.TokenInfo)
				}
				if p == nil {
					return nil, nil, errors.New("not authenticated")
				}
				if perm != "" && !s.app.HasPermission(p.UserID, resource, action, p.OrgID) {
					return nil, nil, fmt.Errorf("permission denied: this needs the %s permission, which the connected user's role does not have; an admin can grant it under Settings → Roles", perm)
				}
				text, data, err := fn(&call{ctx: ctx, s: s, p: p}, in)
				if err != nil {
					return nil, nil, toolError(err)
				}
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, data, nil
			})
		},
	})
}

// registerTools adds the tools the scopes allow to srv.
func (s *Server) registerTools(srv *mcp.Server, scopes []Scope) {
	granted := make(map[Scope]bool, len(scopes))
	for _, sc := range scopes {
		granted[sc] = true
	}
	for _, t := range tools {
		if granted[t.scope] {
			t.register(srv, s)
		}
	}
}

// toolError turns a REST error into a message that tells the model what to
// do next.
func toolError(err error) error {
	var ae *apiError
	if !errors.As(err, &ae) {
		return err
	}
	switch ae.Status {
	case http.StatusForbidden:
		return fmt.Errorf("permission denied: %s. The connected user's role does not allow this; an admin can grant it under Settings → Roles", ae.Message)
	case http.StatusNotFound:
		return fmt.Errorf("%s. Check the ID; the search and list tools return valid IDs", ae.Message)
	case http.StatusTooManyRequests:
		return fmt.Errorf("rate limited: %s. Wait a minute before retrying", ae.Message)
	case http.StatusUnauthorized:
		return fmt.Errorf("authentication failed: %s. Reconnect the Whatomate connector", ae.Message)
	}
	return errors.New(ae.Message)
}

// Shared annotation values.
var (
	falsePtr = new(bool)
	truePtr  = func() *bool { b := true; return &b }()
)

func readOnly(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, OpenWorldHint: falsePtr}
}

// write annotates a tool that changes Whatomate data but nothing outside it.
func write(title string, idempotent bool) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, DestructiveHint: falsePtr, IdempotentHint: idempotent, OpenWorldHint: falsePtr}
}

// outbound annotates a tool that messages people on WhatsApp.
func outbound(title string, destructive bool) *mcp.ToolAnnotations {
	d := falsePtr
	if destructive {
		d = truePtr
	}
	return &mcp.ToolAnnotations{Title: title, DestructiveHint: d, OpenWorldHint: truePtr}
}

// Formatting helpers.

func fmtTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "—"
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

// truncate shortens s to n runes, on one line.
func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// cell escapes a value for a markdown table cell.
func cell(s string) string {
	if s == "" {
		return "—"
	}
	return strings.ReplaceAll(s, "|", `\|`)
}

// table renders a markdown table.
func table(headers []string, rows [][]string) string {
	var b strings.Builder
	b.WriteString("| " + strings.Join(headers, " | ") + " |\n|")
	for range headers {
		b.WriteString(" --- |")
	}
	b.WriteString("\n")
	for _, row := range rows {
		cells := make([]string, len(row))
		for i, v := range row {
			cells[i] = cell(v)
		}
		b.WriteString("| " + strings.Join(cells, " | ") + " |\n")
	}
	return b.String()
}

// pageArgs returns page/limit query values with limit clamped to [1, max].
func pageArgs(page, limit, def, max int) url.Values {
	if limit <= 0 {
		limit = def
	}
	if limit > max {
		limit = max
	}
	if page < 1 {
		page = 1
	}
	return url.Values{"page": {fmt.Sprint(page)}, "limit": {fmt.Sprint(limit)}}
}

// setIf sets key in q when value is not empty.
func setIf(q url.Values, key, value string) {
	if value != "" {
		q.Set(key, value)
	}
}

// pathID escapes an ID for use in a URL path.
func pathID(id string) string {
	return url.PathEscape(strings.TrimSpace(id))
}
