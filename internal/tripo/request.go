package tripo

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

func (c *Client) request(ctx context.Context, phase, method, path string, body any, out any) (Diagnostic, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return Diagnostic{}, Failure(phase, "local_validation", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, reader)
	if err != nil {
		return Diagnostic{}, Failure(phase, "local_validation", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return c.doAPI(req, phase, out)
}

// doAPI 只做一次请求，禁止携带凭证的重定向及提交体重放；重试权限仍归 Runtime。
func (c *Client) doAPI(req *http.Request, phase string, out any) (d Diagnostic, err error) {
	start := time.Now()
	d = Diagnostic{Phase: phase, RequestStarted: true}
	defer func() {
		d.OccurredAt, d.DurationMS = time.Now().UTC(), time.Since(start).Milliseconds()
		d = d.Safe(c.APIKey)
		if err != nil {
			if e, ok := err.(*APIError); ok {
				d.Category, d.CauseCode, d.SubmissionUnknown = e.Detail.Category, e.Detail.CauseCode, e.Unknown
				e.Detail = d.Safe(c.APIKey)
			}
		}
	}()
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.GetBody = nil
	client := *c.HTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, callErr := client.Do(req)
	if callErr != nil {
		return d, failure(d, "", callErr, phase == "submit")
	}
	defer res.Body.Close()
	d.HTTPStatus = &res.StatusCode
	d.ProviderTraceID = res.Header.Get("X-Tripo-Trace-ID")
	// 3xx 本身已是完整诊断，不依赖重定向响应正文是否包含 JSON。
	if res.StatusCode >= 300 && res.StatusCode < 400 {
		return d, failure(d, "http", nil, phase == "submit")
	}
	var envelope struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	b, readErr := io.ReadAll(io.LimitReader(res.Body, (2<<20)+1))
	if readErr != nil {
		return d, failure(d, "", readErr, phase == "submit")
	}
	if len(b) > 2<<20 {
		return d, failure(d, "protocol", nil, phase == "submit")
	}
	if decodeErr := json.Unmarshal(b, &envelope); decodeErr != nil {
		return d, failure(d, "decode", decodeErr, phase == "submit")
	}
	d.ProviderCode = envelope.Code
	if envelope.Code == nil {
		return d, failure(d, "protocol", nil, phase == "submit")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return d, failure(d, "http", nil, phase == "submit" && res.StatusCode >= 500)
	}
	if *envelope.Code != 0 {
		return d, failure(d, "provider", nil, false)
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return d, failure(d, "protocol", nil, phase == "submit")
	}
	if decodeErr := json.Unmarshal(envelope.Data, out); decodeErr != nil {
		return d, failure(d, "decode", decodeErr, phase == "submit")
	}
	return d, nil
}

// Preflight 只验证国内/显式站点余额协议与鉴权，不初始化 Run 或创建任何任务。
func (c *Client) Preflight(ctx context.Context) (Diagnostic, error) {
	if c.APIKey == "" {
		return Diagnostic{}, Failure("preflight", "local_validation", nil)
	}
	var balance struct {
		Balance *float64 `json:"balance"`
	}
	d, err := c.request(ctx, "preflight", http.MethodGet, "/account/balance", nil, &balance)
	if err == nil && balance.Balance == nil {
		err = failure(d, "protocol", nil, false)
	}
	return d, err
}
