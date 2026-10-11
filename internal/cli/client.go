package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is a thin client for the control plane /v1 API.
type Client struct {
	BaseURL string
	Key     string
	HTTP    *http.Client
}

type envelope struct {
	OK      bool            `json:"ok"`
	Data    json.RawMessage `json:"data"`
	Code    string          `json:"code"`
	Message string          `json:"message"`
}

// Do sends one request and returns the envelope data.
func (c *Client) Do(ctx context.Context, method, path string, headers map[string]string, body any) (json.RawMessage, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+"/v1"+path, rd)
	if err != nil {
		return nil, newError("invalid_argument", err.Error())
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 11 * time.Minute}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, newError("request_failed", strings.ReplaceAll(err.Error(), c.Key, "***"))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, newError("request_failed", err.Error())
	}
	var env envelope
	if jerr := json.Unmarshal(raw, &env); jerr != nil {
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil, newError("bad_response", "response is not a Dock envelope")
		}
		return nil, newError(fmt.Sprintf("http_%d", resp.StatusCode), http.StatusText(resp.StatusCode))
	}
	if !env.OK {
		code := env.Code
		if code == "" {
			code = fmt.Sprintf("http_%d", resp.StatusCode)
		}
		return nil, newError(code, env.Message)
	}
	return env.Data, nil
}
