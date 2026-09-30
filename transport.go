package prompton

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The three runtime endpoints. Nothing else is ever called: PromptOn is a
// control plane, not a proxy, so the provider call stays in the app.

const maxResponseBytes = 32 << 20

type snapshotResponse struct {
	Status       int
	Body         []byte
	ETag         string
	LastModified string
	RetryAfter   time.Duration
}

func (c *Client) newRequest(ctx context.Context, method, path string, query url.Values, body []byte) (*http.Request, error) {
	u := c.cfg.baseURL() + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// fetchSnapshot performs GET /prompts with If-None-Match. A 304 carries no
// body and nothing to parse.
func (c *Client) fetchSnapshot(ctx context.Context, environment, etag string) (*snapshotResponse, error) {
	return c.fetchSnapshotPath(ctx, "/prompts", environment, etag)
}

// fetchPromptSnapshot performs GET /prompts/{key} with If-None-Match. It is the runtime config
// path: one prompt key, one conditional request, no bulk fallback.
func (c *Client) fetchPromptSnapshot(ctx context.Context, key, environment, etag string) (*snapshotResponse, error) {
	return c.fetchSnapshotPath(ctx, "/prompts/"+url.PathEscape(key), environment, etag)
}

func (c *Client) fetchSnapshotPath(ctx context.Context, path, environment, etag string) (*snapshotResponse, error) {
	if c.cfg.APIKey == "" {
		return nil, ErrNoAPIKey
	}
	query := url.Values{"environment": []string{environment}}
	req, err := c.newRequest(ctx, http.MethodGet, path, query, nil)
	if err != nil {
		return nil, err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	// Runtime config fetches are single-shot attempts. The default config HTTP
	// client disables keep-alives so the transport cannot retry an idempotent GET
	// on a stale reused connection.
	req.Close = true
	resp, err := c.cfg.configHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer drainAndClose(resp)

	out := &snapshotResponse{
		Status:       resp.StatusCode,
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		RetryAfter:   parseRetryAfter(resp.Header.Get("Retry-After"), c.cfg.now()),
	}
	if resp.StatusCode == http.StatusNotModified {
		return out, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, err
	}
	out.Body = body
	if resp.StatusCode != http.StatusOK {
		apiErr := parseAPIError(resp.StatusCode, body)
		if apiErr.RetryAfter == 0 && out.RetryAfter > 0 {
			apiErr.RetryAfter = out.RetryAfter.Seconds()
		}
		return out, apiErr
	}
	return out, nil
}

type resolveRequest struct {
	UseCase     string
	Environment string
	Prompt      string
	// Variables is sent when it is non-nil, empty map included: an empty object
	// asks the server to render, and the difference between "render with
	// nothing" and "do not render" is the difference between a 400 naming the
	// missing variable and a 200 carrying the raw template.
	Variables map[string]interface{}
}

func (r resolveRequest) body() map[string]interface{} {
	out := map[string]interface{}{}
	if r.Environment != "" {
		out["environment"] = r.Environment
	}
	if r.Prompt != "" {
		out["template"] = r.Prompt
	}
	if r.Variables != nil {
		out["variables"] = r.Variables
	}
	return out
}

type resolveResponse struct {
	Key        string `json:"key"`
	Kind       string `json:"kind"`
	Deployment struct {
		ID       string `json:"id"`
		Revision string `json:"revision"`
	} `json:"deployment"`
	Prompt          *string                `json:"template"`
	PromptNames     []string               `json:"template_names"`
	ModelID         *string                `json:"model_id"`
	Model           *string                `json:"model"`
	Provider        *string                `json:"provider"`
	Params          map[string]interface{} `json:"params"`
	Tools           map[string]interface{} `json:"tools"`
	ProviderOptions map[string]interface{} `json:"provider_options"`
	PromptVersion   *struct {
		ID     string `json:"id"`
		Number int    `json:"number"`
	} `json:"prompt_version"`
	Messages []Message `json:"messages"`
	Text     *string   `json:"text"`
	Source   string    `json:"source"`
	Warnings []string  `json:"warnings"`
	ETag     string    `json:"etag"`
}

func (r *resolveResponse) UnmarshalJSON(data []byte) error {
	type alias resolveResponse
	var aux struct {
		*alias
		LegacyPrompt      *string  `json:"prompt"`
		LegacyPromptNames []string `json:"prompt_names"`
	}
	aux.alias = (*alias)(r)
	if err := decodeJSON(data, &aux); err != nil {
		return err
	}
	if r.Prompt == nil {
		r.Prompt = aux.LegacyPrompt
	}
	if len(r.PromptNames) == 0 && len(aux.LegacyPromptNames) > 0 {
		r.PromptNames = aux.LegacyPromptNames
	}
	return nil
}

func (c *Client) postResolve(ctx context.Context, body resolveRequest) (*resolveResponse, error) {
	if c.cfg.APIKey == "" {
		return nil, ErrNoAPIKey
	}
	payload := canonicalJSON(body.body())
	path := "/prompts/" + url.PathEscape(body.UseCase) + "/render"
	req, err := c.newRequest(ctx, http.MethodPost, path, nil, payload)
	if err != nil {
		return nil, err
	}
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer drainAndClose(resp)
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, parseAPIError(resp.StatusCode, data)
	}
	var out resolveResponse
	if err := decodeJSON(data, &out); err != nil {
		return nil, fmt.Errorf("prompton: invalid prompt response: %w", err)
	}
	return &out, nil
}

// BatchResult is what POST /logs answers with. One bad record never
// fails the batch: read Rejected, and never resend the accepted ones.
type BatchResult struct {
	Accepted   int             `json:"accepted"`
	Duplicates int             `json:"duplicates"`
	Rejected   []RejectedEntry `json:"rejected"`
}

// RejectedEntry names one record the server refused.
type RejectedEntry struct {
	Index   int    `json:"index"`
	ID      string `json:"id"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (c *Client) postLogs(ctx context.Context, environment string, records []map[string]interface{}) (*BatchResult, error) {
	if c.cfg.APIKey == "" {
		return nil, ErrNoAPIKey
	}
	payload := encodeBatch(records)
	query := url.Values{"environment": []string{environment}}
	req, err := c.newRequest(ctx, http.MethodPost, "/logs", query, payload)
	if err != nil {
		return nil, err
	}
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer drainAndClose(resp)
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := parseAPIError(resp.StatusCode, data)
		if apiErr.RetryAfter == 0 {
			if d := parseRetryAfter(resp.Header.Get("Retry-After"), c.cfg.now()); d > 0 {
				apiErr.RetryAfter = d.Seconds()
			}
		}
		return nil, apiErr
	}
	var out BatchResult
	if err := decodeJSON(data, &out); err != nil {
		return nil, fmt.Errorf("prompton: invalid /logs response: %w", err)
	}
	return &out, nil
}

// encodeBatch wraps records in the request envelope the endpoint accepts.
func encodeBatch(records []map[string]interface{}) []byte {
	return canonicalJSON(map[string]interface{}{"logs": records})
}

func (c *Client) postEvents(ctx context.Context, environment string, events []map[string]interface{}) (*BatchResult, error) {
	if c.cfg.APIKey == "" {
		return nil, ErrNoAPIKey
	}
	payload := canonicalJSON(map[string]interface{}{"logs": []interface{}{}, "events": events})
	query := url.Values{"environment": []string{environment}}
	req, err := c.newRequest(ctx, http.MethodPost, "/logs", query, payload)
	if err != nil {
		return nil, err
	}
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer drainAndClose(resp)
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := parseAPIError(resp.StatusCode, data)
		if apiErr.RetryAfter == 0 {
			if d := parseRetryAfter(resp.Header.Get("Retry-After"), c.cfg.now()); d > 0 {
				apiErr.RetryAfter = d.Seconds()
			}
		}
		return nil, apiErr
	}
	var out struct {
		Accepted   int             `json:"accepted"`
		Duplicates int             `json:"duplicates"`
		Rejected   []RejectedEntry `json:"rejected"`
		Events     *BatchResult    `json:"events"`
	}
	if err := decodeJSON(data, &out); err != nil {
		return nil, fmt.Errorf("prompton: invalid /logs response: %w", err)
	}
	if out.Events != nil {
		return out.Events, nil
	}
	return &BatchResult{Accepted: out.Accepted, Duplicates: out.Duplicates, Rejected: out.Rejected}, nil
}

func drainAndClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
}

func parseAPIError(status int, body []byte) *APIError {
	out := &APIError{Status: status}
	var envelope struct {
		Error struct {
			Code    string                 `json:"code"`
			Message string                 `json:"message"`
			Details map[string]interface{} `json:"details"`
		} `json:"error"`
	}
	if err := decodeJSON(body, &envelope); err == nil {
		out.Code = envelope.Error.Code
		out.Message = envelope.Error.Message
		out.Details = envelope.Error.Details
	}
	if out.Message == "" {
		out.Message = strings.TrimSpace(string(body))
		if len(out.Message) > 512 {
			out.Message = out.Message[:512]
		}
	}
	if v, ok := out.Details["retry_after"]; ok {
		switch n := v.(type) {
		case float64:
			out.RetryAfter = n
		default:
			if f, err := strconv.ParseFloat(valueToString(v), 64); err == nil {
				out.RetryAfter = f
			}
		}
	}
	return out
}

// parseRetryAfter reads the header in either of its forms: delay seconds, or an
// HTTP date.
func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(value, 64); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs * float64(time.Second))
	}
	if t, err := http.ParseTime(value); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
		return 0
	}
	return 0
}
