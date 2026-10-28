package mcpserver

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
)

const auditWhatsAppFlows = models.ResourceFlowsWhatsApp

func init() {
	defineTool(ScopeMessagesRead, "flows.whatsapp:read", &mcp.Tool{
		Name:        "whatomate_list_whatsapp_flows",
		Description: "List WhatsApp Flows: interactive forms (sign-ups, bookings, surveys) that open inside WhatsApp. Shows each flow's status (DRAFT, PUBLISHED, DEPRECATED) and Meta ID.",
		Annotations: readOnly("List WhatsApp flows"),
	}, listWhatsAppFlows)

	defineTool(ScopeMessagesRead, "flows.whatsapp:read", &mcp.Tool{
		Name:        "whatomate_get_whatsapp_flow",
		Description: "Get a WhatsApp Flow in full: its screens in Meta Flow JSON, status and preview link.",
		Annotations: readOnly("Get WhatsApp flow"),
	}, getWhatsAppFlow)

	defineTool(ScopeFlowsWrite, "flows.whatsapp:write", &mcp.Tool{
		Name: "whatomate_create_whatsapp_flow",
		Description: "Create a WhatsApp Flow as a local draft. screens use Meta's Flow JSON screen format ({id, title, terminal, layout: {type: SingleColumnLayout, children: [...]}}); " +
			"the last screen needs a Footer whose on-click-action is complete. Then check it at Meta with whatomate_save_whatsapp_flow_to_meta.",
		Annotations: write("Create WhatsApp flow", false),
	}, createWhatsAppFlow)

	defineTool(ScopeFlowsWrite, "flows.whatsapp:write", &mcp.Tool{
		Name: "whatomate_update_whatsapp_flow",
		Description: "Change a draft WhatsApp Flow. Only the fields you pass change; screens replace all screens. " +
			"Published flows can't be changed; duplicate them with whatomate_duplicate_whatsapp_flow and edit the copy.",
		Annotations: write("Update WhatsApp flow", true),
	}, updateWhatsAppFlow)

	defineTool(ScopeFlowsWrite, "flows.whatsapp:write", &mcp.Tool{
		Name: "whatomate_save_whatsapp_flow_to_meta",
		Description: "Upload a draft WhatsApp Flow to Meta, where it is validated and gets a preview link. It stays a draft and isn't sent to anyone. " +
			"If Meta reports errors, fix the screens with whatomate_update_whatsapp_flow and save again.",
		Annotations: &mcp.ToolAnnotations{Title: "Save WhatsApp flow to Meta", DestructiveHint: falsePtr, IdempotentHint: true, OpenWorldHint: truePtr},
	}, saveWhatsAppFlowToMeta)

	defineTool(ScopeFlowsWrite, "flows.whatsapp:write", &mcp.Tool{
		Name: "whatomate_publish_whatsapp_flow",
		Description: "Publish a WhatsApp Flow so it can be sent. Publishing is permanent: a published flow can never be edited again, only duplicated. " +
			"Without confirm=true this returns a preview; publish only after the user approves.",
		Annotations: outbound("Publish WhatsApp flow", true),
	}, publishWhatsAppFlow)

	defineTool(ScopeFlowsWrite, "flows.whatsapp:write", &mcp.Tool{
		Name:        "whatomate_duplicate_whatsapp_flow",
		Description: "Copy a WhatsApp Flow into a new draft, the way to change a published flow.",
		Annotations: write("Duplicate WhatsApp flow", false),
	}, duplicateWhatsAppFlow)
}

var flowCategories = []string{"SIGN_UP", "SIGN_IN", "APPOINTMENT_BOOKING", "LEAD_GENERATION", "CONTACT_US", "CUSTOMER_SUPPORT", "SURVEY", "OTHER"}

type flowSummary struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Status          string `json:"status"`
	Category        string `json:"category"`
	WhatsAppAccount string `json:"whatsapp_account"`
	MetaFlowID      string `json:"meta_flow_id,omitempty"`
	PreviewURL      string `json:"preview_url,omitempty"`
	Screens         int    `json:"screen_count"`
	HasLocalChanges bool   `json:"has_local_changes"`
}

func summarizeFlow(f handlers.FlowResponse) flowSummary {
	return flowSummary{ID: f.ID.String(), Name: f.Name, Status: f.Status, Category: f.Category, WhatsAppAccount: f.WhatsAppAccount,
		MetaFlowID: f.MetaFlowID, PreviewURL: f.PreviewURL, Screens: len(f.Screens), HasLocalChanges: f.HasLocalChanges}
}

func flowStateText(f handlers.FlowResponse) string {
	switch {
	case f.Status == "PUBLISHED":
		return "published"
	case f.MetaFlowID == "":
		return "local draft, not saved to Meta yet"
	case f.HasLocalChanges:
		return "draft at Meta, with local changes not saved to Meta"
	default:
		return "draft at Meta"
	}
}

func listWhatsAppFlows(c *call, in listChatbotInput) (string, any, error) {
	q := pageArgs(in.Page, in.Limit, 50, 100)
	setIf(q, "search", in.Search)
	var resp struct {
		Flows []handlers.FlowResponse `json:"flows"`
		Total int64                   `json:"total"`
	}
	if err := c.get("/api/flows", q, &resp); err != nil {
		return "", nil, err
	}
	out := make([]flowSummary, len(resp.Flows))
	if len(out) == 0 {
		return "No WhatsApp flows yet.", map[string]any{"flows": out}, nil
	}
	rows := make([][]string, len(resp.Flows))
	for i, f := range resp.Flows {
		out[i] = summarizeFlow(f)
		rows[i] = []string{f.Name, f.Status, f.Category, f.WhatsAppAccount, f.MetaFlowID, out[i].ID}
	}
	return fmt.Sprintf("%d of %d WhatsApp flows:\n\n", len(resp.Flows), resp.Total) +
		table([]string{"Name", "Status", "Category", "Account", "Meta flow ID", "ID"}, rows), map[string]any{"flows": out, "total": resp.Total}, nil
}

func (c *call) whatsAppFlow(id string) (handlers.FlowResponse, error) {
	var resp struct {
		Flow handlers.FlowResponse `json:"flow"`
	}
	err := c.get("/api/flows/"+pathID(id), nil, &resp)
	return resp.Flow, err
}

func getWhatsAppFlow(c *call, in idInput) (string, any, error) {
	f, err := c.whatsAppFlow(in.ID)
	if err != nil {
		return "", nil, err
	}
	screens, _ := json.MarshalIndent(f.Screens, "", " ")
	text := fmt.Sprintf("**%s** (%s, %s, account %s, JSON version %s)\n\n", f.Name, f.Status, f.Category, f.WhatsAppAccount, f.JSONVersion)
	text += "- State: " + flowStateText(f) + "\n"
	if f.MetaFlowID != "" {
		text += "- Meta flow ID: " + f.MetaFlowID + " (use it in a chatbot whatsapp_flow node)\n"
	}
	if f.PreviewURL != "" {
		text += "- Preview: " + f.PreviewURL + "\n"
	}
	text += fmt.Sprintf("\nScreens:\n```json\n%s\n```", truncate(string(screens), 20000))
	return text, map[string]any{"flow": summarizeFlow(f), "screens": f.Screens, "json_version": f.JSONVersion}, nil
}

// checkScreens catches the structural mistakes Meta would reject, before a
// round trip to Meta.
func checkScreens(screens []map[string]any) error {
	if len(screens) == 0 {
		return fmt.Errorf("screens is empty; a flow needs at least one screen")
	}
	ids := map[string]bool{}
	terminal := false
	for i, sc := range screens {
		id, _ := sc["id"].(string)
		if id == "" {
			return fmt.Errorf("screen %d has no id", i+1)
		}
		if ids[id] {
			return fmt.Errorf("screen id %q is used twice", id)
		}
		ids[id] = true
		layout, _ := sc["layout"].(map[string]any)
		if layout == nil {
			return fmt.Errorf("screen %q has no layout; use {\"type\": \"SingleColumnLayout\", \"children\": [...]}", id)
		}
		if children, _ := layout["children"].([]any); len(children) == 0 {
			return fmt.Errorf("screen %q has no components", id)
		}
		if t, _ := sc["terminal"].(bool); t {
			terminal = true
		}
	}
	if !terminal {
		return fmt.Errorf("no screen has \"terminal\": true; the last screen must be terminal with a Footer whose on-click-action is complete")
	}
	return nil
}

func toAnySlice(screens []map[string]any) []any {
	out := make([]any, len(screens))
	for i, s := range screens {
		out[i] = s
	}
	return out
}

type createWhatsAppFlowInput struct {
	WhatsAppAccount string           `json:"whatsapp_account" jsonschema:"WhatsApp account name"`
	Name            string           `json:"name" jsonschema:"Flow name"`
	Category        string           `json:"category" jsonschema:"SIGN_UP, SIGN_IN, APPOINTMENT_BOOKING, LEAD_GENERATION, CONTACT_US, CUSTOMER_SUPPORT, SURVEY or OTHER"`
	Screens         []map[string]any `json:"screens" jsonschema:"Screens in Meta Flow JSON format"`
	JSONVersion     string           `json:"json_version,omitempty" jsonschema:"Meta Flow JSON version (default 6.0)"`
}

func createWhatsAppFlow(c *call, in createWhatsAppFlowInput) (string, any, error) {
	in.Category = strings.ToUpper(in.Category)
	if in.WhatsAppAccount == "" || strings.TrimSpace(in.Name) == "" {
		return "", nil, fmt.Errorf("whatsapp_account and name are required")
	}
	if !slices.Contains(flowCategories, in.Category) {
		return "", nil, fmt.Errorf("category must be one of %s", strings.Join(flowCategories, ", "))
	}
	if err := checkScreens(in.Screens); err != nil {
		return "", nil, err
	}
	body := map[string]any{"whatsapp_account": in.WhatsAppAccount, "name": in.Name, "category": in.Category,
		"json_version": in.JSONVersion, "screens": toAnySlice(in.Screens)}
	var resp struct {
		Flow handlers.FlowResponse `json:"flow"`
	}
	if err := c.post("/api/flows", body, &resp); err != nil {
		return "", nil, err
	}
	f := resp.Flow
	c.audit(auditWhatsAppFlows, f.ID.String(), models.AuditActionCreated, "create_whatsapp_flow", map[string]any{"name": f.Name})
	return fmt.Sprintf("Created draft WhatsApp flow %q (ID %s). Check it at Meta with whatomate_save_whatsapp_flow_to_meta.", f.Name, f.ID),
		summarizeFlow(f), nil
}

type updateWhatsAppFlowInput struct {
	ID          string           `json:"id" jsonschema:"WhatsApp flow ID (UUID)"`
	Name        string           `json:"name,omitempty"`
	Category    string           `json:"category,omitempty"`
	Screens     []map[string]any `json:"screens,omitempty" jsonschema:"Replaces all screens"`
	JSONVersion string           `json:"json_version,omitempty"`
}

func updateWhatsAppFlow(c *call, in updateWhatsAppFlowInput) (string, any, error) {
	f, err := c.whatsAppFlow(in.ID)
	if err != nil {
		return "", nil, err
	}
	if f.Status != "DRAFT" {
		return "", nil, fmt.Errorf("flow %q is %s and can't be changed; duplicate it with whatomate_duplicate_whatsapp_flow and edit the copy", f.Name, f.Status)
	}
	body := map[string]any{}
	if in.Name != "" {
		body["name"] = in.Name
	}
	if in.Category != "" {
		in.Category = strings.ToUpper(in.Category)
		if !slices.Contains(flowCategories, in.Category) {
			return "", nil, fmt.Errorf("category must be one of %s", strings.Join(flowCategories, ", "))
		}
		body["category"] = in.Category
	}
	if in.JSONVersion != "" {
		body["json_version"] = in.JSONVersion
	}
	if in.Screens != nil {
		if err := checkScreens(in.Screens); err != nil {
			return "", nil, err
		}
		body["screens"] = toAnySlice(in.Screens)
	}
	if len(body) == 0 {
		return "", nil, fmt.Errorf("nothing to change")
	}
	var resp struct {
		Flow handlers.FlowResponse `json:"flow"`
	}
	if err := c.put("/api/flows/"+pathID(in.ID), body, &resp); err != nil {
		return "", nil, err
	}
	c.audit(auditWhatsAppFlows, in.ID, models.AuditActionUpdated, "update_whatsapp_flow", map[string]any{"fields": changedFields(body)})
	text := "Updated WhatsApp flow " + f.Name + ": " + changedFields(body) + "."
	if f.MetaFlowID != "" {
		text += " Save it to Meta again to update the draft there."
	}
	return text, summarizeFlow(resp.Flow), nil
}

func saveWhatsAppFlowToMeta(c *call, in idInput) (string, any, error) {
	f, err := c.whatsAppFlow(in.ID)
	if err != nil {
		return "", nil, err
	}
	if f.Status == "PUBLISHED" {
		return "", nil, fmt.Errorf("flow %q is already published; duplicate it to make changes", f.Name)
	}
	if err := c.post("/api/flows/"+pathID(in.ID)+"/save-to-meta", nil, nil); err != nil {
		return "", nil, err
	}
	saved, err := c.whatsAppFlow(in.ID)
	if err != nil {
		return "", nil, err
	}
	c.audit(auditWhatsAppFlows, in.ID, models.AuditActionUpdated, "save_whatsapp_flow_to_meta", map[string]any{"meta_flow_id": saved.MetaFlowID})
	text := fmt.Sprintf("Saved %q to Meta as a draft (Meta flow ID %s). Meta accepted the screens.", saved.Name, saved.MetaFlowID)
	if saved.PreviewURL != "" {
		text += " Preview it at " + saved.PreviewURL + "."
	}
	return text + " Publish it with whatomate_publish_whatsapp_flow once the user is happy.", summarizeFlow(saved), nil
}

type publishWhatsAppFlowInput struct {
	ID      string `json:"id" jsonschema:"WhatsApp flow ID (UUID)"`
	Confirm bool   `json:"confirm,omitempty" jsonschema:"Set true only after the user approved publishing"`
}

func publishWhatsAppFlow(c *call, in publishWhatsAppFlowInput) (string, any, error) {
	f, err := c.whatsAppFlow(in.ID)
	if err != nil {
		return "", nil, err
	}
	switch {
	case f.Status != "DRAFT":
		return "", nil, fmt.Errorf("flow %q is %s; only drafts can be published", f.Name, f.Status)
	case f.MetaFlowID == "":
		return "", nil, fmt.Errorf("flow %q hasn't been saved to Meta yet; save it with whatomate_save_whatsapp_flow_to_meta first", f.Name)
	case f.HasLocalChanges:
		return "", nil, fmt.Errorf("flow %q has changes not saved to Meta; save it with whatomate_save_whatsapp_flow_to_meta first so Meta publishes the latest version", f.Name)
	}
	if !in.Confirm {
		text := fmt.Sprintf("PREVIEW, nothing has been published.\n\nPublishing %q (%d screens, %s) makes it available to send. "+
			"This is permanent: a published flow can never be edited again, only duplicated.", f.Name, len(f.Screens), f.Category)
		if f.PreviewURL != "" {
			text += " The user can try it first at " + f.PreviewURL + "."
		}
		text += "\n\nCall again with confirm=true only if the user approves."
		return text, map[string]any{"preview": true, "requires_confirmation": true, "flow": summarizeFlow(f)}, nil
	}
	var resp struct {
		Flow handlers.FlowResponse `json:"flow"`
	}
	if err := c.post("/api/flows/"+pathID(in.ID)+"/publish", nil, &resp); err != nil {
		return "", nil, err
	}
	c.audit(auditWhatsAppFlows, in.ID, models.AuditActionUpdated, "publish_whatsapp_flow", map[string]any{"name": f.Name})
	return fmt.Sprintf("Published %q. Send it from a chatbot flow's whatsapp_flow node with flow_id %s.", f.Name, f.MetaFlowID), summarizeFlow(resp.Flow), nil
}

func duplicateWhatsAppFlow(c *call, in idInput) (string, any, error) {
	var resp struct {
		Flow handlers.FlowResponse `json:"flow"`
	}
	if err := c.post("/api/flows/"+pathID(in.ID)+"/duplicate", nil, &resp); err != nil {
		return "", nil, err
	}
	f := resp.Flow
	c.audit(auditWhatsAppFlows, f.ID.String(), models.AuditActionCreated, "duplicate_whatsapp_flow", map[string]any{"from": in.ID})
	return fmt.Sprintf("Copied into new draft %q (ID %s). Edit it with whatomate_update_whatsapp_flow.", f.Name, f.ID), summarizeFlow(f), nil
}
