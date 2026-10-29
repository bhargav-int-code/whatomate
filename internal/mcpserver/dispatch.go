package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/shridarpatil/whatomate/internal/middleware"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttputil"
)

const (
	// clientHeader carries the MCP client name to the internal server, which
	// moves it into the request context and drops the header. The public
	// server never interprets it.
	clientHeader = "X-Whatomate-Mcp-Client"
	// internalTokenTTL bounds the JWT minted for each REST call. It never
	// leaves the process.
	internalTokenTTL = time.Minute
	dispatchTimeout  = 30 * time.Second
	maxBodySize      = 15 * 1024 * 1024
)

// dispatcher calls the REST API in-process. Requests go through a real
// fasthttp server on an in-memory listener, so handlers get a complete
// RequestCtx (usable as a context.Context) and the full middleware chain.
type dispatcher struct {
	client *fasthttp.Client
	secret []byte
}

func newDispatcher(router fasthttp.RequestHandler, secret string) *dispatcher {
	ln := fasthttputil.NewInmemoryListener()
	srv := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			client := string(ctx.Request.Header.Peek(clientHeader))
			ctx.Request.Header.Del(clientHeader)
			ctx.SetUserValue(middleware.ContextKeyMCPClient, client)
			router(ctx)
		},
		Name:               "Whatomate MCP",
		MaxRequestBodySize: maxBodySize,
	}
	go func() { _ = srv.Serve(ln) }()

	return &dispatcher{
		client: &fasthttp.Client{
			Dial:                func(string) (net.Conn, error) { return ln.Dial() },
			MaxResponseBodySize: maxBodySize,
		},
		secret: []byte(secret),
	}
}

// apiError is a non-2xx REST response.
type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string { return e.Message }

// envelope is the REST API's response wrapper.
type envelope struct {
	Status  string          `json:"status"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// do calls method path as p and decodes the response's data into out (which
// may be nil). body, if not nil, is sent as JSON.
func (d *dispatcher) do(ctx context.Context, p *Principal, method, path string, query url.Values, body, out any) error {
	var contentType string
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request: %w", err)
		}
		contentType, payload = "application/json", b
	}
	return d.send(ctx, p, method, path, query, contentType, payload, out)
}

// upload posts a multipart form with fields and one file to path as p.
func (d *dispatcher) upload(ctx context.Context, p *Principal, path string, fields map[string]string, fileField, filename, mimeType string, data []byte, out any) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			return fmt.Errorf("encoding form: %w", err)
		}
	}
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, fileField, filename))
	h.Set("Content-Type", mimeType)
	part, err := w.CreatePart(h)
	if err != nil {
		return fmt.Errorf("encoding form: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return fmt.Errorf("encoding form: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("encoding form: %w", err)
	}
	return d.send(ctx, p, http.MethodPost, path, nil, w.FormDataContentType(), buf.Bytes(), out)
}

// send makes the request and decodes the REST envelope.
func (d *dispatcher) send(ctx context.Context, p *Principal, method, path string, query url.Values, contentType string, payload []byte, out any) error {
	token, err := d.token(p)
	if err != nil {
		return fmt.Errorf("signing request: %w", err)
	}

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	uri := "http://whatomate.internal" + path
	if len(query) > 0 {
		uri += "?" + query.Encode()
	}
	req.SetRequestURI(uri)
	req.Header.SetMethod(method)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(clientHeader, p.Client)
	if payload != nil {
		req.Header.SetContentType(contentType)
		req.SetBody(payload)
	}

	deadline := time.Now().Add(dispatchTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	if err := d.client.DoDeadline(req, resp, deadline); err != nil {
		return fmt.Errorf("calling %s %s: %w", method, path, err)
	}

	var env envelope
	if err := json.Unmarshal(resp.Body(), &env); err != nil {
		return &apiError{Status: resp.StatusCode(), Message: fmt.Sprintf("unexpected response (HTTP %d)", resp.StatusCode())}
	}
	if resp.StatusCode() >= 300 || env.Status == "error" {
		msg := env.Message
		if msg == "" {
			msg = fmt.Sprintf("request failed (HTTP %d)", resp.StatusCode())
		}
		return &apiError{Status: resp.StatusCode(), Message: msg}
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}
	return nil
}

// token mints a short-lived access token for p, the same kind the REST API
// issues at login.
func (d *dispatcher) token(p *Principal) (string, error) {
	now := time.Now()
	claims := middleware.JWTClaims{
		UserID:         p.UserID,
		OrganizationID: p.OrgID,
		Email:          p.Email,
		RoleID:         p.RoleID,
		IsSuperAdmin:   p.IsSuperAdmin,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(internalTokenTTL)),
			IssuedAt:  jwt.NewNumericDate(now),
			Issuer:    "whatomate-mcp",
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(d.secret)
}

// statusOf returns the HTTP status of a REST error, or 0.
func statusOf(err error) int {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}
