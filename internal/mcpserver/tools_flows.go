package mcpserver

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/expr-lang/expr"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
)

const auditChatbotFlows = models.ResourceFlowsChatbot

func init() {
	defineTool(ScopeChatbotRead, "", &mcp.Tool{
		Name:        "whatomate_describe_flow_nodes",
		Description: "Explain how chatbot conversation flows are built: every node type, its config fields and the edge conditions it routes on. Read this before creating or changing a flow.",
		Annotations: readOnly("Describe flow nodes"),
	}, describeFlowNodes)

	defineTool(ScopeChatbotRead, "flows.chatbot:read", &mcp.Tool{
		Name:        "whatomate_list_chatbot_flows",
		Description: "List chatbot conversation flows: multi-step conversations started by trigger keywords (collect details, branch on answers, call APIs, hand over to agents).",
		Annotations: readOnly("List chatbot flows"),
	}, listChatbotFlows)

	defineTool(ScopeChatbotRead, "flows.chatbot:read", &mcp.Tool{
		Name:        "whatomate_get_chatbot_flow",
		Description: "Get a chatbot flow in full, including its graph of nodes and edges, so it can be reviewed or changed.",
		Annotations: readOnly("Get chatbot flow"),
	}, getChatbotFlow)

	defineTool(ScopeChatbotWrite, "flows.chatbot:write", &mcp.Tool{
		Name: "whatomate_create_chatbot_flow",
		Description: "Create a chatbot conversation flow from a graph of nodes and edges (see whatomate_describe_flow_nodes). The graph is checked before saving; " +
			"errors are returned so you can fix them. Flows calling outside URLs (api_call or webhook nodes) return a preview first and are saved only with confirm=true after the user approves. " +
			"New flows are created disabled unless enabled=true.",
		Annotations: write("Create chatbot flow", false),
	}, createChatbotFlow)

	defineTool(ScopeChatbotWrite, "flows.chatbot:write", &mcp.Tool{
		Name: "whatomate_update_chatbot_flow",
		Description: "Change a chatbot flow. Only the fields you pass change; a graph replaces the whole graph, so start from whatomate_get_chatbot_flow. " +
			"Disable a flow with enabled=false instead of deleting it. New outside URLs need confirm=true after the user approves.",
		Annotations: write("Update chatbot flow", true),
	}, updateChatbotFlow)
}

const flowNodesGuide = `A chatbot flow is a graph: {"nodes": [...], "edges": [...], "entry_node": "<id>"}.

Node: {"id": "unique-id", "type": "<type>", "label": "short name", "config": {...}}. Positions are optional; nodes are laid out automatically.
Edge: {"from": "<node id>", "to": "<node id>", "condition": "<outcome>"}. A node follows the edge whose condition matches its outcome, else its "default" edge; with no matching edge the conversation ends.
Text fields can use {{variable}} placeholders filled from saved answers (store_as) and set_variable.

Node types (config fields; outcomes it routes on):
- start: entry point, no config. Outcome: default.
- message: send text. config.message. Outcome: default.
- buttons: ask with up to 3 reply buttons, wait for the tap. config.body, config.buttons [{"id","title"}] (title ≤ 20 chars; or url/phone buttons {"type":"url","title","url"} / {"type":"phone","title","phone_number"}), optional config.store_as. Outcome: button:<id>.
- prompt: ask a question and wait for a typed answer. config.body, config.store_as (variable name), optional config.validation_regex, config.validation_error, config.max_retries (default 3). Outcomes: default (valid answer), max_retries.
- condition: branch on an expression over variables, e.g. "age >= 18 && city == 'Pune'". config.expression. Outcomes: true, false.
- api_call: HTTP request. config.url, config.method (GET/POST/PUT/PATCH/DELETE), optional config.headers {}, config.body (string), config.response_mapping {"variable": "json.path"}, config.message_template (sent after). Outcomes: http:2xx, http:non2xx.
- webhook: fire-and-forget HTTP request. config.url, config.method, config.headers, config.body. Outcome: default.
- set_variable: config.set {"variable": "value or {{other}}"}. Outcome: default.
- timing: branch on the day and time. config.schedule [{"day":"monday","enabled":true,"start_time":"09:00","end_time":"18:00"}] (day names). Outcomes: in_hours, out_of_hours.
- ai_response: reply with the chatbot AI. Optional config.prompt_template. Outcome: default.
- whatsapp_flow: send a published WhatsApp (Meta) Flow form. config.flow_id (Meta flow ID), config.body, optional config.header, config.cta. Outcome: default (after the form is submitted).
- transfer: hand the chat to a human agent; ends the flow. Optional config.body (sent first), config.team_id, config.notes.
- goto_flow: continue in another chatbot flow; ends this one. config.flow_id (chatbot flow ID).
- end: end the conversation. Optional config.message.

Example: ask for a name and greet.
{"entry_node":"start","nodes":[
 {"id":"start","type":"start"},
 {"id":"ask","type":"prompt","config":{"body":"What's your name?","store_as":"name"}},
 {"id":"hi","type":"message","config":{"message":"Nice to meet you, {{name}}!"}},
 {"id":"done","type":"end"}],
 "edges":[{"from":"start","to":"ask","condition":"default"},{"from":"ask","to":"hi","condition":"default"},{"from":"hi","to":"done","condition":"default"}]}`

func describeFlowNodes(*call, noInput) (string, any, error) {
	return flowNodesGuide, nil, nil
}

// chatFlowGraph is a chatbot flow graph as assistants send it.
type chatFlowGraph struct {
	Nodes     []chatFlowNode `json:"nodes" jsonschema:"The flow's nodes"`
	Edges     []chatFlowEdge `json:"edges" jsonschema:"Connections between nodes"`
	EntryNode string         `json:"entry_node,omitempty" jsonschema:"ID of the first node; defaults to the start node"`
}

type chatFlowNode struct {
	ID       string         `json:"id" jsonschema:"Unique node ID"`
	Type     string         `json:"type" jsonschema:"Node type; see whatomate_describe_flow_nodes"`
	Label    string         `json:"label,omitempty" jsonschema:"Short name shown in the editor"`
	Config   map[string]any `json:"config,omitempty" jsonschema:"Type-specific settings"`
	Position *struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
	} `json:"position,omitempty" jsonschema:"Editor position; omit to lay out automatically"`
}

type chatFlowEdge struct {
	From      string `json:"from" jsonschema:"Source node ID"`
	To        string `json:"to" jsonschema:"Target node ID"`
	Condition string `json:"condition,omitempty" jsonschema:"Outcome that follows this edge; default when omitted"`
}

var nodeOutcomes = map[string][]string{
	"start": {"default"}, "message": {"default"}, "buttons": nil, "prompt": {"default", "max_retries"},
	"condition": {"true", "false"}, "api_call": {"http:2xx", "http:non2xx"}, "webhook": {"default"},
	"set_variable": {"default"}, "timing": {"in_hours", "out_of_hours"}, "ai_response": {"default"},
	"whatsapp_flow": {"default"}, "transfer": {}, "goto_flow": {}, "end": {},
}

var dayNames = []string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}

func configString(cfg map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := cfg[k].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// validateFlowGraph checks a graph the way the flow engine will run it.
// Errors would break the flow and block saving; warnings are likely
// mistakes that still run.
func validateFlowGraph(g *chatFlowGraph) (errs, warns []string) {
	if len(g.Nodes) == 0 {
		return []string{"the graph has no nodes"}, nil
	}
	nodes := make(map[string]*chatFlowNode, len(g.Nodes))
	for i := range g.Nodes {
		n := &g.Nodes[i]
		switch {
		case n.ID == "":
			errs = append(errs, fmt.Sprintf("node %d has no id", i+1))
			continue
		case nodes[n.ID] != nil:
			errs = append(errs, fmt.Sprintf("node id %q is used twice", n.ID))
			continue
		}
		nodes[n.ID] = n
		if _, ok := nodeOutcomes[n.Type]; !ok {
			errs = append(errs, fmt.Sprintf("node %q has unknown type %q", n.ID, n.Type))
			continue
		}
		if n.Config == nil {
			n.Config = map[string]any{}
		}
		errs = append(errs, checkNodeConfig(n)...)
	}

	if g.EntryNode == "" {
		for _, n := range g.Nodes {
			if n.Type == "start" {
				g.EntryNode = n.ID
				break
			}
		}
	}
	if g.EntryNode == "" {
		errs = append(errs, "set entry_node or add a start node")
	} else if nodes[g.EntryNode] == nil {
		errs = append(errs, fmt.Sprintf("entry_node %q is not a node", g.EntryNode))
	}

	out := map[string][]string{}
	for i := range g.Edges {
		e := &g.Edges[i]
		if e.Condition == "" {
			e.Condition = "default"
		}
		from, to := nodes[e.From], nodes[e.To]
		if from == nil || to == nil {
			errs = append(errs, fmt.Sprintf("edge %s → %s points at a node that does not exist", e.From, e.To))
			continue
		}
		out[e.From] = append(out[e.From], e.Condition)
		if e.Condition != "default" && !outcomeAllowed(from, e.Condition) {
			warns = append(warns, fmt.Sprintf("edge %s → %s: a %s node never produces outcome %q", e.From, e.To, from.Type, e.Condition))
		}
	}

	for _, n := range g.Nodes {
		// Unknown types were reported above; transfer, goto_flow and end
		// end the conversation, so they need no edges
		expected, ok := nodeOutcomes[n.Type]
		if n.ID == "" || !ok || (len(expected) == 0 && n.Type != "buttons") {
			continue
		}
		if len(out[n.ID]) == 0 {
			warns = append(warns, fmt.Sprintf("node %q (%s) has no outgoing edge, so the conversation ends there", n.ID, n.Type))
			continue
		}
		if slices.Contains(out[n.ID], "default") {
			continue
		}
		for _, o := range requiredOutcomes(&n) {
			if !slices.Contains(out[n.ID], o) {
				warns = append(warns, fmt.Sprintf("node %q has no edge for outcome %q (and no default edge)", n.ID, o))
			}
		}
	}

	if g.EntryNode != "" && nodes[g.EntryNode] != nil {
		reached := reachable(g)
		for _, n := range g.Nodes {
			if n.ID != "" && !reached[n.ID] {
				warns = append(warns, fmt.Sprintf("node %q can't be reached from the entry node", n.ID))
			}
		}
	}
	return errs, warns
}

func outcomeAllowed(n *chatFlowNode, outcome string) bool {
	if n.Type == "buttons" {
		return strings.HasPrefix(outcome, "button:")
	}
	return slices.Contains(nodeOutcomes[n.Type], outcome)
}

// requiredOutcomes are the outcomes a node can produce that should each have
// an edge when it has no default edge.
func requiredOutcomes(n *chatFlowNode) []string {
	if n.Type == "buttons" {
		var outs []string
		for _, b := range nodeButtons(n.Config) {
			if b.ID != "" && (b.Type == "" || b.Type == "reply") {
				outs = append(outs, "button:"+b.ID)
			}
		}
		return outs
	}
	if n.Type == "prompt" && configString(n.Config, "validation_regex") == "" {
		return []string{"default"}
	}
	return nodeOutcomes[n.Type]
}

func nodeButtons(cfg map[string]any) []chatButton {
	raw, _ := json.Marshal(cfg["buttons"])
	var buttons []chatButton
	_ = json.Unmarshal(raw, &buttons)
	return buttons
}

func checkNodeConfig(n *chatFlowNode) []string {
	var errs []string
	need := func(what string, ok bool) {
		if !ok {
			errs = append(errs, fmt.Sprintf("%s node %q needs %s", n.Type, n.ID, what))
		}
	}
	cfg := n.Config
	switch n.Type {
	case "message":
		need("config.message", configString(cfg, "message", "text") != "")
	case "buttons":
		need("config.body", configString(cfg, "body", "message", "text") != "" || n.Label != "")
		buttons := nodeButtons(cfg)
		need("config.buttons", len(buttons) > 0)
		if err := validateButtons(buttons); err != nil {
			errs = append(errs, fmt.Sprintf("buttons node %q: %v", n.ID, err))
		}
	case "prompt":
		need("config.body", configString(cfg, "body", "message", "text") != "")
		if re := configString(cfg, "validation_regex"); re != "" {
			if _, err := regexp.Compile(re); err != nil {
				errs = append(errs, fmt.Sprintf("prompt node %q: validation_regex is invalid: %v", n.ID, err))
			}
		}
	case "condition":
		exp := configString(cfg, "expression")
		need("config.expression", exp != "")
		if exp != "" {
			if _, err := expr.Compile(exp, expr.AllowUndefinedVariables()); err != nil {
				errs = append(errs, fmt.Sprintf("condition node %q: expression does not parse: %v", n.ID, err))
			}
		}
	case "api_call", "webhook":
		u := configString(cfg, "url")
		need("config.url (http or https)", strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://"))
		if m := strings.ToUpper(configString(cfg, "method")); m != "" && !slices.Contains([]string{"GET", "POST", "PUT", "PATCH", "DELETE"}, m) {
			errs = append(errs, fmt.Sprintf("%s node %q: method must be GET, POST, PUT, PATCH or DELETE", n.Type, n.ID))
		}
	case "set_variable":
		set, _ := cfg["set"].(map[string]any)
		need("config.set with at least one variable", len(set) > 0)
	case "timing":
		schedule, _ := cfg["schedule"].([]any)
		need("config.schedule", len(schedule) > 0)
		for _, item := range schedule {
			day, _ := item.(map[string]any)
			name, _ := day["day"].(string)
			start, _ := day["start_time"].(string)
			end, _ := day["end_time"].(string)
			enabled, _ := day["enabled"].(bool)
			if !slices.Contains(dayNames, strings.ToLower(name)) {
				errs = append(errs, fmt.Sprintf("timing node %q: day %q must be a day name like monday", n.ID, name))
			} else if enabled && (!hhmm.MatchString(start) || !hhmm.MatchString(end)) {
				errs = append(errs, fmt.Sprintf("timing node %q: %s needs start_time and end_time as HH:MM", n.ID, name))
			}
		}
	case "goto_flow":
		_, err := uuid.Parse(configString(cfg, "flow_id"))
		need("config.flow_id (a chatbot flow ID)", err == nil)
	case "whatsapp_flow":
		need("config.flow_id", configString(cfg, "flow_id") != "")
		need("config.body", configString(cfg, "body", "message", "text") != "")
	case "transfer":
		if t := configString(cfg, "team_id"); t != "" {
			if _, err := uuid.Parse(t); err != nil {
				errs = append(errs, fmt.Sprintf("transfer node %q: team_id must be a team ID", n.ID))
			}
		}
	}
	return errs
}

// reachable returns the nodes reachable from the entry node.
func reachable(g *chatFlowGraph) map[string]bool {
	next := map[string][]string{}
	for _, e := range g.Edges {
		next[e.From] = append(next[e.From], e.To)
	}
	seen := map[string]bool{g.EntryNode: true}
	queue := []string{g.EntryNode}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, to := range next[id] {
			if !seen[to] {
				seen[to] = true
				queue = append(queue, to)
			}
		}
	}
	return seen
}

// graphURLs lists the outside URLs a graph calls.
func graphURLs(g *chatFlowGraph) []string {
	var urls []string
	for _, n := range g.Nodes {
		if n.Type == "api_call" || n.Type == "webhook" {
			if u := configString(n.Config, "url"); u != "" {
				urls = append(urls, u)
			}
		}
	}
	return urls
}

// toStoredGraph converts the graph to the stored v2 format, laying nodes out
// in columns by distance from the entry node when no positions were given.
func toStoredGraph(g *chatFlowGraph) map[string]any {
	depth := map[string]int{g.EntryNode: 0}
	next := map[string][]string{}
	for _, e := range g.Edges {
		next[e.From] = append(next[e.From], e.To)
	}
	queue := []string{g.EntryNode}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, to := range next[id] {
			if _, ok := depth[to]; !ok {
				depth[to] = depth[id] + 1
				queue = append(queue, to)
			}
		}
	}
	rows := map[int]int{}
	nodes := make([]map[string]any, len(g.Nodes))
	for i, n := range g.Nodes {
		x, y := 0.0, 0.0
		if n.Position != nil {
			x, y = n.Position.X, n.Position.Y
		} else {
			d, ok := depth[n.ID]
			if !ok {
				d = len(g.Nodes) // unreachable nodes go in a last column
			}
			x, y = float64(d*320), float64(rows[d]*180)
			rows[d]++
		}
		label := n.Label
		if label == "" {
			label = n.ID
		}
		cfg := n.Config
		if cfg == nil {
			cfg = map[string]any{}
		}
		nodes[i] = map[string]any{"id": n.ID, "type": n.Type, "label": label, "config": cfg, "position": map[string]any{"x": x, "y": y}}
	}
	edges := make([]map[string]any, len(g.Edges))
	for i, e := range g.Edges {
		edges[i] = map[string]any{"from": e.From, "to": e.To, "condition": e.Condition}
	}
	return map[string]any{"version": 2, "entry_node": g.EntryNode, "nodes": nodes, "edges": edges}
}

// checkGraph validates g and reports errors as a tool error. Warnings are
// returned for the response text.
func checkGraph(g *chatFlowGraph) ([]string, error) {
	errs, warns := validateFlowGraph(g)
	if len(errs) > 0 {
		return nil, fmt.Errorf("the flow graph has problems, nothing was saved:\n- %s", strings.Join(errs, "\n- "))
	}
	return warns, nil
}

func withWarnings(text string, warns []string) string {
	if len(warns) == 0 {
		return text
	}
	return text + "\n\nWarnings (saved anyway, check them):\n- " + strings.Join(warns, "\n- ")
}

type chatbotFlowView struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	Description     string          `json:"description"`
	TriggerKeywords []string        `json:"trigger_keywords"`
	IsEnabled       bool            `json:"is_enabled"`
	Graph           json.RawMessage `json:"graph"`
}

func listChatbotFlows(c *call, in listChatbotInput) (string, any, error) {
	q := pageArgs(in.Page, in.Limit, 50, 100)
	setIf(q, "search", in.Search)
	var resp struct {
		Flows []handlers.ChatbotFlowResponse `json:"flows"`
		Total int64                          `json:"total"`
	}
	if err := c.get("/api/chatbot/flows", q, &resp); err != nil {
		return "", nil, err
	}
	if len(resp.Flows) == 0 {
		return "No chatbot flows yet.", resp, nil
	}
	rows := make([][]string, len(resp.Flows))
	for i, f := range resp.Flows {
		rows[i] = []string{f.Name, strings.Join(f.TriggerKeywords, ", "), yesNo(f.Enabled), truncate(f.Description, 80), f.ID}
	}
	return fmt.Sprintf("%d of %d chatbot flows:\n\n", len(resp.Flows), resp.Total) +
		table([]string{"Name", "Trigger keywords", "Enabled", "Description", "ID"}, rows), resp, nil
}

func getChatbotFlow(c *call, in idInput) (string, any, error) {
	var f chatbotFlowView
	if err := c.get("/api/chatbot/flows/"+pathID(in.ID), nil, &f); err != nil {
		return "", nil, err
	}
	var g chatFlowGraph
	_ = json.Unmarshal(f.Graph, &g)
	text := fmt.Sprintf("**%s** (enabled: %s)\n\n- Trigger keywords: %s\n- Description: %s\n- %d nodes, %d edges, entry node %q\n\nGraph:\n```json\n%s\n```",
		f.Name, yesNo(f.IsEnabled), cell(strings.Join(f.TriggerKeywords, ", ")), cell(f.Description), len(g.Nodes), len(g.Edges), g.EntryNode,
		truncate(string(f.Graph), 20000))
	return text, map[string]any{"id": f.ID, "name": f.Name, "description": f.Description, "trigger_keywords": f.TriggerKeywords,
		"enabled": f.IsEnabled, "graph": g}, nil
}

type createChatbotFlowInput struct {
	Name            string        `json:"name" jsonschema:"Flow name"`
	Description     string        `json:"description,omitempty"`
	TriggerKeywords []string      `json:"trigger_keywords" jsonschema:"Messages that start the flow"`
	Graph           chatFlowGraph `json:"graph" jsonschema:"Nodes and edges; see whatomate_describe_flow_nodes"`
	Enabled         bool          `json:"enabled,omitempty" jsonschema:"Start the flow enabled (default false, so it can be reviewed first)"`
	Confirm         bool          `json:"confirm,omitempty" jsonschema:"Set true only after the user approved the preview of outside URLs"`
}

func createChatbotFlow(c *call, in createChatbotFlowInput) (string, any, error) {
	if strings.TrimSpace(in.Name) == "" || len(in.TriggerKeywords) == 0 {
		return "", nil, fmt.Errorf("name and trigger_keywords are required")
	}
	warns, err := checkGraph(&in.Graph)
	if err != nil {
		return "", nil, err
	}
	if urls := newOutboundURLs(graphURLs(&in.Graph), nil); len(urls) > 0 && !in.Confirm {
		text, data := confirmPreview(fmt.Sprintf("Flow %q calls these outside addresses:", in.Name), urls)
		return text, data, nil
	}
	body := map[string]any{"name": in.Name, "description": in.Description, "trigger_keywords": in.TriggerKeywords,
		"graph": toStoredGraph(&in.Graph), "enabled": in.Enabled}
	var resp struct {
		ID string `json:"id"`
	}
	if err := c.post("/api/chatbot/flows", body, &resp); err != nil {
		return "", nil, err
	}
	c.audit(auditChatbotFlows, resp.ID, models.AuditActionCreated, "create_chatbot_flow", map[string]any{"name": in.Name})
	state := "disabled; turn it on with whatomate_update_chatbot_flow enabled=true once reviewed"
	if in.Enabled {
		state = "enabled"
	}
	return withWarnings(fmt.Sprintf("Created flow %q (ID %s), %d nodes, %s.", in.Name, resp.ID, len(in.Graph.Nodes), state), warns),
		map[string]any{"id": resp.ID, "warnings": warns}, nil
}

type updateChatbotFlowInput struct {
	ID              string         `json:"id" jsonschema:"Chatbot flow ID (UUID)"`
	Name            *string        `json:"name,omitempty"`
	Description     *string        `json:"description,omitempty"`
	TriggerKeywords []string       `json:"trigger_keywords,omitempty" jsonschema:"Replaces the trigger keywords"`
	Graph           *chatFlowGraph `json:"graph,omitempty" jsonschema:"Replaces the whole graph"`
	Enabled         *bool          `json:"enabled,omitempty"`
	Confirm         bool           `json:"confirm,omitempty" jsonschema:"Set true only after the user approved the preview of new outside URLs"`
}

func updateChatbotFlow(c *call, in updateChatbotFlowInput) (string, any, error) {
	body := map[string]any{}
	setPtr(body, "name", in.Name)
	setPtr(body, "description", in.Description)
	setPtr(body, "enabled", in.Enabled)
	if in.TriggerKeywords != nil {
		body["trigger_keywords"] = in.TriggerKeywords
	}
	var warns []string
	if in.Graph != nil {
		var err error
		if warns, err = checkGraph(in.Graph); err != nil {
			return "", nil, err
		}
		var current chatbotFlowView
		if err := c.get("/api/chatbot/flows/"+pathID(in.ID), nil, &current); err != nil {
			return "", nil, err
		}
		var old chatFlowGraph
		_ = json.Unmarshal(current.Graph, &old)
		if added := newOutboundURLs(graphURLs(in.Graph), graphURLs(&old)); len(added) > 0 && !in.Confirm {
			text, data := confirmPreview(fmt.Sprintf("The new version of flow %q calls these new outside addresses:", current.Name), added)
			return text, data, nil
		}
		body["graph"] = toStoredGraph(in.Graph)
	}
	if len(body) == 0 {
		return "", nil, fmt.Errorf("nothing to change")
	}
	if err := c.put("/api/chatbot/flows/"+pathID(in.ID), body, nil); err != nil {
		return "", nil, err
	}
	c.audit(auditChatbotFlows, in.ID, models.AuditActionUpdated, "update_chatbot_flow", map[string]any{"fields": changedFields(body)})
	return withWarnings("Updated flow "+in.ID+": "+changedFields(body)+".", warns), map[string]any{"id": in.ID, "warnings": warns}, nil
}
