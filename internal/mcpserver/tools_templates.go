package mcpserver

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
)

const auditTemplates = models.ResourceTemplates

func init() {
	defineTool(ScopeTemplatesWrite, "templates:write", &mcp.Tool{
		Name: "whatomate_create_template",
		Description: "Create a WhatsApp message template as a local draft. Give example values for every {{variable}}; Meta requires them. " +
			"A media header (image, video, document) takes a public sample URL. Nothing goes to Meta until whatomate_submit_template.",
		Annotations: write("Create template", false),
	}, createTemplate)

	defineTool(ScopeTemplatesWrite, "templates:write", &mcp.Tool{
		Name: "whatomate_update_template",
		Description: "Change a draft or rejected template. Only the fields you pass change. Approved, pending and paused templates can't be edited here, " +
			"since editing would stop them sending until Meta approves them again; create a new template (e.g. name_v2) instead.",
		Annotations: write("Update template", true),
	}, updateTemplate)

	defineTool(ScopeTemplatesWrite, "templates:write", &mcp.Tool{
		Name: "whatomate_submit_template",
		Description: "Submit a draft or rejected template to Meta for review. Without confirm=true this returns a preview of exactly what Meta will review; " +
			"submit only after the user approves. Meta usually decides within minutes to a day; check with whatomate_sync_templates.",
		Annotations: outbound("Submit template", false),
	}, submitTemplate)

	defineTool(ScopeTemplatesWrite, "templates:sync", &mcp.Tool{
		Name:        "whatomate_sync_templates",
		Description: "Fetch the templates of a WhatsApp account from Meta, updating their approval status (approved, rejected, paused) and adding templates created elsewhere.",
		Annotations: write("Sync templates", true),
	}, syncTemplates)
}

// Meta's template limits
const (
	maxTemplateBody   = 1024
	maxTemplateHeader = 60
	maxTemplateFooter = 60
	maxButtonText     = 25
	maxButtons        = 10
)

var templateNameRe = regexp.MustCompile(`^[a-z0-9_]{1,512}$`)

// Media header sample limits by type
var headerMedia = map[string]struct {
	maxBytes int64
	types    []string
}{
	"IMAGE":    {5 << 20, []string{"image/jpeg", "image/png"}},
	"VIDEO":    {16 << 20, []string{"video/mp4", "video/3gpp"}},
	"DOCUMENT": {25 << 20, []string{"application/pdf"}},
}

type templateButton struct {
	Type        string `json:"type" jsonschema:"quick_reply, url, phone_number or copy_code"`
	Text        string `json:"text,omitempty" jsonschema:"Button label, at most 25 characters (not used for copy_code)"`
	URL         string `json:"url,omitempty" jsonschema:"For url: the link; may end with one {{1}} for a per-message suffix"`
	URLExample  string `json:"url_example,omitempty" jsonschema:"For a url with {{1}}: an example full value of the suffix"`
	PhoneNumber string `json:"phone_number,omitempty" jsonschema:"For phone_number: international format with +, e.g. +919876543210"`
	Example     string `json:"example,omitempty" jsonschema:"For copy_code: an example code"`
}

// templateSpec is a template as assistants describe it.
type templateSpec struct {
	HeaderType     string            `json:"header_type,omitempty" jsonschema:"none (default), text, image, video or document"`
	HeaderText     string            `json:"header_text,omitempty" jsonschema:"For text: at most 60 characters, at most one {{variable}}"`
	HeaderExample  string            `json:"header_example,omitempty" jsonschema:"Example value for the header variable"`
	HeaderMediaURL string            `json:"header_media_url,omitempty" jsonschema:"For image/video/document: public URL of a sample file (JPEG/PNG ≤5 MB, MP4 ≤16 MB, PDF ≤25 MB)"`
	Body           string            `json:"body,omitempty" jsonschema:"Message text, at most 1024 characters. Variables are {{1}}, {{2}}… or named {{customer_name}}, not both"`
	BodyExamples   map[string]string `json:"body_examples,omitempty" jsonschema:"Example value for each body variable, keyed by its name, e.g. {\"1\": \"Asha\"}"`
	Footer         string            `json:"footer,omitempty" jsonschema:"Small grey text, at most 60 characters, no variables"`
	Buttons        []templateButton  `json:"buttons,omitempty" jsonschema:"Up to 10 buttons: quick replies, up to 2 url, 1 phone_number, 1 copy_code"`
}

type createTemplateInput struct {
	WhatsAppAccount string `json:"whatsapp_account" jsonschema:"WhatsApp account name; whatomate_list_whatsapp_accounts lists them"`
	Name            string `json:"name" jsonschema:"Lowercase letters, digits and underscores, e.g. order_shipped"`
	Language        string `json:"language" jsonschema:"Language code, e.g. en, en_US, hi"`
	Category        string `json:"category" jsonschema:"MARKETING, UTILITY or AUTHENTICATION"`
	templateSpec
	AddSecurityRecommendation bool `json:"add_security_recommendation,omitempty" jsonschema:"AUTHENTICATION only: add Meta's do-not-share warning"`
	CodeExpirationMinutes     int  `json:"code_expiration_minutes,omitempty" jsonschema:"AUTHENTICATION only: 1-90, shows when the code expires"`
}

// validateTemplate applies Meta's rules that would otherwise only fail at
// review. keepsMedia says an existing media header sample stays in use.
func validateTemplate(category string, s *templateSpec, keepsMedia bool) error {
	auth := category == "AUTHENTICATION"
	switch strings.ToUpper(s.HeaderType) {
	case "", "NONE":
	case "TEXT":
		if s.HeaderText == "" {
			return fmt.Errorf("header_text is required for a text header")
		}
		if n := len([]rune(s.HeaderText)); n > maxTemplateHeader {
			return fmt.Errorf("header_text is %d characters; Meta allows %d", n, maxTemplateHeader)
		}
		if vars := templateVars(s.HeaderText); len(vars) > 1 {
			return fmt.Errorf("a text header can have at most one variable")
		} else if len(vars) == 1 && s.HeaderExample == "" {
			return fmt.Errorf("header_example is required for the header variable {{%s}}", vars[0])
		}
	case "IMAGE", "VIDEO", "DOCUMENT":
		if s.HeaderMediaURL == "" && !keepsMedia {
			return fmt.Errorf("header_media_url is required for a %s header", strings.ToLower(s.HeaderType))
		}
	default:
		return fmt.Errorf("header_type must be none, text, image, video or document")
	}

	if strings.TrimSpace(s.Body) == "" {
		return fmt.Errorf("body is required")
	}
	if n := len([]rune(s.Body)); n > maxTemplateBody {
		return fmt.Errorf("body is %d characters; Meta allows %d", n, maxTemplateBody)
	}
	if !auth {
		vars := templateVars(s.Body)
		positional, named := 0, 0
		for _, v := range vars {
			if _, err := strconv.Atoi(v); err == nil {
				positional++
			} else {
				named++
			}
			if strings.TrimSpace(s.BodyExamples[v]) == "" {
				return fmt.Errorf("body_examples needs an example for {{%s}}", v)
			}
		}
		if positional > 0 && named > 0 {
			return fmt.Errorf("use either numbered ({{1}}) or named ({{name}}) variables in the body, not both")
		}
		trimmed := strings.TrimSpace(s.Body)
		if strings.HasPrefix(trimmed, "{{") || strings.HasSuffix(trimmed, "}}") {
			return fmt.Errorf("Meta rejects a body that starts or ends with a variable; add text before and after it")
		}
	}

	if n := len([]rune(s.Footer)); n > maxTemplateFooter {
		return fmt.Errorf("footer is %d characters; Meta allows %d", n, maxTemplateFooter)
	}
	if strings.Contains(s.Footer, "{{") {
		return fmt.Errorf("the footer can't contain variables")
	}

	if len(s.Buttons) > maxButtons {
		return fmt.Errorf("%d buttons; Meta allows %d", len(s.Buttons), maxButtons)
	}
	counts := map[string]int{}
	for i, b := range s.Buttons {
		t := strings.ToLower(b.Type)
		counts[t]++
		if t != "copy_code" {
			if strings.TrimSpace(b.Text) == "" {
				return fmt.Errorf("button %d needs text", i+1)
			}
			if n := len([]rune(b.Text)); n > maxButtonText {
				return fmt.Errorf("button %q is %d characters; Meta allows %d", b.Text, n, maxButtonText)
			}
		}
		switch t {
		case "quick_reply":
		case "url":
			if !strings.HasPrefix(b.URL, "https://") && !strings.HasPrefix(b.URL, "http://") {
				return fmt.Errorf("url button %q needs an http(s) url", b.Text)
			}
			if strings.Contains(b.URL, "{{") && b.URLExample == "" {
				return fmt.Errorf("url button %q has a variable, so it needs url_example", b.Text)
			}
		case "phone_number":
			if !strings.HasPrefix(b.PhoneNumber, "+") {
				return fmt.Errorf("phone button %q needs a phone_number starting with +", b.Text)
			}
		case "copy_code":
			if b.Example == "" {
				return fmt.Errorf("copy_code button needs an example code")
			}
		default:
			return fmt.Errorf("button type %q must be quick_reply, url, phone_number or copy_code", b.Type)
		}
	}
	if counts["url"] > 2 || counts["phone_number"] > 1 || counts["copy_code"] > 1 {
		return fmt.Errorf("Meta allows at most 2 url buttons, 1 phone_number button and 1 copy_code button")
	}
	return nil
}

// sampleValues builds Whatomate's stored example format for the variables
// of header and body.
func sampleValues(s *templateSpec) []any {
	var out []any
	add := func(component, name, value string) {
		if idx, err := strconv.Atoi(name); err == nil {
			out = append(out, map[string]any{"component": component, "index": idx, "value": value})
		} else {
			out = append(out, map[string]any{"component": component, "param_name": name, "value": value})
		}
	}
	if strings.EqualFold(s.HeaderType, "text") {
		for _, v := range templateVars(s.HeaderText) {
			add("header", v, s.HeaderExample)
		}
	}
	for _, v := range templateVars(s.Body) {
		add("body", v, s.BodyExamples[v])
	}
	return out
}

func storedButtons(buttons []templateButton) []any {
	out := make([]any, len(buttons))
	for i, b := range buttons {
		m := map[string]any{"type": strings.ToUpper(b.Type), "text": b.Text}
		switch strings.ToLower(b.Type) {
		case "url":
			m["url"] = b.URL
			if b.URLExample != "" {
				m["example"] = b.URLExample
			}
		case "phone_number":
			m["phone_number"] = b.PhoneNumber
		case "copy_code":
			m["example"] = b.Example
		}
		out[i] = m
	}
	return out
}

// uploadHeaderSample downloads a header sample from a public URL and uploads
// it to Meta, returning the handle Meta needs to review the template.
func (c *call) uploadHeaderSample(account, headerType, rawURL string) (string, error) {
	limits := headerMedia[headerType]
	client := c.s.app.HTTPClient // refuses private addresses in production
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(c.ctx, http.MethodGet, rawURL, nil)
	if err != nil || (req.URL.Scheme != "https" && req.URL.Scheme != "http") {
		return "", fmt.Errorf("header_media_url must be an http(s) URL")
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("couldn't download header_media_url: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("header_media_url returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limits.maxBytes+1))
	if err != nil {
		return "", fmt.Errorf("couldn't download header_media_url: %v", err)
	}
	if int64(len(data)) > limits.maxBytes {
		return "", fmt.Errorf("the %s is larger than Meta's %d MB limit", strings.ToLower(headerType), limits.maxBytes>>20)
	}
	mimeType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mimeType == "" || mimeType == "application/octet-stream" {
		mimeType = http.DetectContentType(data)
		mimeType, _, _ = mime.ParseMediaType(mimeType)
	}
	if !slices.Contains(limits.types, mimeType) {
		return "", fmt.Errorf("a %s header must be %s, but the URL serves %s", strings.ToLower(headerType), strings.Join(limits.types, " or "), mimeType)
	}
	name := path.Base(req.URL.Path)
	if name == "" || name == "/" || name == "." {
		name = "sample"
	}
	var out struct {
		Handle string `json:"handle"`
	}
	if err := c.upload("/api/templates/upload-media", map[string]string{"account": account}, "file", name, mimeType, data, &out); err != nil {
		return "", err
	}
	return out.Handle, nil
}

// templateRequestBody builds the REST request for spec, uploading a media
// header sample when one is given; otherwise keptHandle, the handle of the
// already uploaded sample, is reused.
func (c *call) templateRequestBody(account string, s *templateSpec, keptHandle string) (map[string]any, error) {
	headerType := strings.ToUpper(s.HeaderType)
	if headerType == "" {
		headerType = "NONE"
	}
	headerContent := ""
	switch headerType {
	case "TEXT":
		headerContent = s.HeaderText
	case "IMAGE", "VIDEO", "DOCUMENT":
		if s.HeaderMediaURL == "" {
			headerContent = keptHandle
			break
		}
		handle, err := c.uploadHeaderSample(account, headerType, s.HeaderMediaURL)
		if err != nil {
			return nil, err
		}
		headerContent = handle
	}
	return map[string]any{
		"header_type":    headerType,
		"header_content": headerContent,
		"body_content":   s.Body,
		"footer_content": s.Footer,
		"buttons":        storedButtons(s.Buttons),
		"sample_values":  sampleValues(s),
	}, nil
}

func createTemplate(c *call, in createTemplateInput) (string, any, error) {
	in.Category = strings.ToUpper(in.Category)
	if !templateNameRe.MatchString(in.Name) {
		return "", nil, fmt.Errorf("name must be lowercase letters, digits and underscores, e.g. order_shipped")
	}
	if in.WhatsAppAccount == "" || in.Language == "" {
		return "", nil, fmt.Errorf("whatsapp_account and language are required")
	}
	if in.Category != "MARKETING" && in.Category != "UTILITY" && in.Category != "AUTHENTICATION" {
		return "", nil, fmt.Errorf("category must be MARKETING, UTILITY or AUTHENTICATION")
	}
	if err := validateTemplate(in.Category, &in.templateSpec, false); err != nil {
		return "", nil, err
	}
	body, err := c.templateRequestBody(in.WhatsAppAccount, &in.templateSpec, "")
	if err != nil {
		return "", nil, err
	}
	body["whatsapp_account"] = in.WhatsAppAccount
	body["name"] = in.Name
	body["language"] = in.Language
	body["category"] = in.Category
	body["add_security_recommendation"] = in.AddSecurityRecommendation
	body["code_expiration_minutes"] = in.CodeExpirationMinutes

	var t handlers.TemplateResponse
	if err := c.post("/api/templates", body, &t); err != nil {
		return "", nil, err
	}
	c.audit(auditTemplates, t.ID.String(), models.AuditActionCreated, "create_template", map[string]any{"name": t.Name})
	return fmt.Sprintf("Created draft template %q (ID %s). Review it, then submit it to Meta with whatomate_submit_template.", t.Name, t.ID),
		summarizeTemplate(t), nil
}

type updateTemplateInput struct {
	ID string `json:"id" jsonschema:"Template ID (UUID)"`
	templateSpec
	Category string `json:"category,omitempty" jsonschema:"MARKETING, UTILITY or AUTHENTICATION"`
	Language string `json:"language,omitempty"`
}

// editableStatuses are the template states that can change without breaking
// live sends.
var editableStatuses = []string{"DRAFT", "REJECTED"}

func (c *call) template(id string) (handlers.TemplateResponse, error) {
	var t handlers.TemplateResponse
	err := c.get("/api/templates/"+pathID(id), nil, &t)
	return t, err
}

func notEditable(t handlers.TemplateResponse) error {
	return fmt.Errorf("template %q is %s. Editing it would stop it sending until Meta approves it again, so it can't be changed here. "+
		"Create a new template instead (e.g. %s_v2) with whatomate_create_template", t.Name, t.Status, t.Name)
}

// specFromTemplate rebuilds the editable fields of an existing template.
func specFromTemplate(t handlers.TemplateResponse) templateSpec {
	s := templateSpec{HeaderType: strings.ToLower(t.HeaderType), Body: t.BodyContent, Footer: t.FooterContent, BodyExamples: map[string]string{}}
	if strings.EqualFold(t.HeaderType, "TEXT") {
		s.HeaderText = t.HeaderContent
	}
	for _, raw := range t.SampleValues {
		sv, _ := raw.(map[string]any)
		value, _ := sv["value"].(string)
		name, _ := sv["param_name"].(string)
		if idx, ok := sv["index"].(float64); ok {
			name = strconv.Itoa(int(idx))
		}
		switch sv["component"] {
		case "header":
			s.HeaderExample = value
		case "body", nil:
			s.BodyExamples[name] = value
		}
	}
	for _, raw := range t.Buttons {
		b, _ := raw.(map[string]any)
		str := func(k string) string { v, _ := b[k].(string); return v }
		tb := templateButton{Type: strings.ToLower(str("type")), Text: str("text"), URL: str("url"), PhoneNumber: str("phone_number")}
		if tb.Type == "url" {
			tb.URLExample = str("example")
		} else if tb.Type == "copy_code" {
			tb.Example = str("example")
		}
		s.Buttons = append(s.Buttons, tb)
	}
	return s
}

func updateTemplate(c *call, in updateTemplateInput) (string, any, error) {
	t, err := c.template(in.ID)
	if err != nil {
		return "", nil, err
	}
	if !slices.Contains(editableStatuses, strings.ToUpper(t.Status)) {
		return "", nil, notEditable(t)
	}

	// The REST endpoint replaces the header, footer and OTP settings
	// wholesale, so start from the current template and apply the changes
	s := specFromTemplate(t)
	// Keep the uploaded media sample unless the header changes
	keptHandle := ""
	if in.HeaderType == "" && in.HeaderMediaURL == "" && headerMedia[strings.ToUpper(t.HeaderType)].maxBytes > 0 {
		keptHandle = t.HeaderContent
	}
	if in.HeaderType != "" {
		s.HeaderType = in.HeaderType
	}
	if in.HeaderText != "" {
		s.HeaderText = in.HeaderText
	}
	if in.HeaderExample != "" {
		s.HeaderExample = in.HeaderExample
	}
	if in.HeaderMediaURL != "" {
		s.HeaderMediaURL = in.HeaderMediaURL
	}
	if in.Body != "" {
		s.Body = in.Body
	}
	for k, v := range in.BodyExamples {
		s.BodyExamples[k] = v
	}
	if in.Footer != "" {
		s.Footer = in.Footer
	}
	if in.Buttons != nil {
		s.Buttons = in.Buttons
	}
	category := strings.ToUpper(t.Category)
	if in.Category != "" {
		category = strings.ToUpper(in.Category)
	}

	if err := validateTemplate(category, &s, keptHandle != ""); err != nil {
		return "", nil, err
	}
	body, err := c.templateRequestBody(t.WhatsAppAccount, &s, keptHandle)
	if err != nil {
		return "", nil, err
	}
	body["category"] = category
	if in.Language != "" {
		body["language"] = in.Language
	}
	body["add_security_recommendation"] = t.AddSecurityRecommendation
	body["code_expiration_minutes"] = t.CodeExpirationMinutes

	var updated handlers.TemplateResponse
	if err := c.put("/api/templates/"+pathID(in.ID), body, &updated); err != nil {
		return "", nil, err
	}
	c.audit(auditTemplates, in.ID, models.AuditActionUpdated, "update_template", map[string]any{"name": t.Name})
	return fmt.Sprintf("Updated draft template %q. Submit it to Meta with whatomate_submit_template when ready.", t.Name), summarizeTemplate(updated), nil
}

type submitTemplateInput struct {
	ID      string `json:"id" jsonschema:"Template ID (UUID)"`
	Confirm bool   `json:"confirm,omitempty" jsonschema:"Set true only after the user approved the preview"`
}

// renderTemplate shows a template the way Meta reviews it.
func renderTemplate(t handlers.TemplateResponse) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s · %s · %s · account %s\n\n", t.Name, t.Category, t.Language, t.WhatsAppAccount)
	switch strings.ToUpper(t.HeaderType) {
	case "TEXT":
		fmt.Fprintf(&b, "Header: %s\n", t.HeaderContent)
	case "IMAGE", "VIDEO", "DOCUMENT":
		fmt.Fprintf(&b, "Header: %s (sample uploaded)\n", strings.ToLower(t.HeaderType))
	}
	fmt.Fprintf(&b, "Body:\n%s\n", t.BodyContent)
	if t.FooterContent != "" {
		fmt.Fprintf(&b, "Footer: %s\n", t.FooterContent)
	}
	for _, raw := range t.Buttons {
		btn, _ := raw.(map[string]any)
		fmt.Fprintf(&b, "Button: [%v] %v %v%v\n", btn["type"], btn["text"], btn["url"], btn["phone_number"])
	}
	if len(t.SampleValues) > 0 {
		b.WriteString("Examples:")
		for _, raw := range t.SampleValues {
			sv, _ := raw.(map[string]any)
			name := sv["param_name"]
			if name == nil {
				name = sv["index"]
			}
			fmt.Fprintf(&b, " %v.{{%v}}=%q", sv["component"], name, sv["value"])
		}
		b.WriteString("\n")
	}
	return b.String()
}

func submitTemplate(c *call, in submitTemplateInput) (string, any, error) {
	t, err := c.template(in.ID)
	if err != nil {
		return "", nil, err
	}
	if !slices.Contains(editableStatuses, strings.ToUpper(t.Status)) {
		return "", nil, fmt.Errorf("template %q is %s; only draft or rejected templates can be submitted", t.Name, t.Status)
	}
	if !in.Confirm {
		text := "PREVIEW, nothing has been submitted.\n\nThis is what Meta will review:\n\n" + renderTemplate(t) +
			"\nShow this to the user and call again with confirm=true only if they approve."
		return text, map[string]any{"preview": true, "requires_confirmation": true, "template": summarizeTemplate(t)}, nil
	}
	var resp struct {
		MetaTemplateID string `json:"meta_template_id"`
		Status         string `json:"status"`
	}
	if err := c.post("/api/templates/"+pathID(in.ID)+"/publish", nil, &resp); err != nil {
		return "", nil, err
	}
	c.audit(auditTemplates, in.ID, models.AuditActionUpdated, "submit_template", map[string]any{"name": t.Name})
	return fmt.Sprintf("Submitted %q to Meta (status %s). Check the result later with whatomate_sync_templates.", t.Name, resp.Status),
		map[string]any{"id": in.ID, "status": resp.Status, "meta_template_id": resp.MetaTemplateID}, nil
}

type syncTemplatesInput struct {
	WhatsAppAccount string `json:"whatsapp_account" jsonschema:"WhatsApp account name"`
}

func syncTemplates(c *call, in syncTemplatesInput) (string, any, error) {
	if in.WhatsAppAccount == "" {
		return "", nil, fmt.Errorf("whatsapp_account is required")
	}
	var resp map[string]any
	if err := c.post("/api/templates/sync", map[string]any{"whatsapp_account": in.WhatsAppAccount}, &resp); err != nil {
		return "", nil, err
	}
	msg, _ := resp["message"].(string)
	if msg == "" {
		msg = "Templates synced from Meta."
	}
	return msg + " List them with whatomate_list_templates to see each status.", resp, nil
}
