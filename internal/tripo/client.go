package tripo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const Model = "v3.1-20260211"
const DownloadLimit int64 = 150 * 1024 * 1024

type Params struct {
	Prompt         string `json:"prompt"`
	FaceLimit      int    `json:"face_limit"`
	TextureQuality string `json:"texture_quality"`
	Input          string `json:"input,omitempty"`
}
type Task struct {
	ID           string  `json:"task_id"`
	Status       string  `json:"status"`
	Progress     int     `json:"progress"`
	ErrorCode    int     `json:"error_code,omitempty"`
	ErrorMessage string  `json:"error_message,omitempty"`
	Credits      float64 `json:"credits_consumed,omitempty"`
	Output       struct {
		ModelURL string `json:"model_url"`
	} `json:"output"`
}
type Provider interface {
	Submit(context.Context, string, Params) (string, error)
	Query(context.Context, string) (Task, error)
	Download(context.Context, string) ([]byte, error)
}
type Client struct {
	BaseURL, APIKey string
	HTTP            *http.Client
	DownloadHTTP    *http.Client
}
type APIError struct {
	Status, Code int
	Unknown      bool
}

func (e *APIError) Error() string {
	return fmt.Sprintf("Tripo 请求失败（HTTP %d，code %d，提交结果未知=%t）", e.Status, e.Code, e.Unknown)
}
func IsUnknown(err error) bool { var e *APIError; return errors.As(err, &e) && e.Unknown }

func New(key string) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			if !publicIP(ip.IP) {
				return nil, fmt.Errorf("产物地址必须使用公共网络地址")
			}
		}
		for _, ip := range ips {
			conn, e := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
			if e == nil {
				return conn, nil
			}
			err = e
		}
		return nil, err
	}
	return &Client{BaseURL: "https://openapi.tripo3d.ai/v3", APIKey: key, HTTP: &http.Client{Timeout: 45 * time.Second}, DownloadHTTP: &http.Client{Transport: transport, Timeout: 2 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 3 {
			return fmt.Errorf("产物重定向次数超限")
		}
		return checkURL(req.URL)
	}}}
}
func publicIP(ip net.IP) bool {
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsUnspecified()
}
func checkURL(u *url.URL) error {
	if u.Scheme != "https" || u.User != nil || u.Hostname() == "" {
		return fmt.Errorf("产物地址必须为 HTTPS")
	}
	return nil
}

func (c *Client) Submit(ctx context.Context, kind string, p Params) (string, error) {
	body := map[string]any{"model": Model, "prompt": p.Prompt, "face_limit": p.FaceLimit, "texture": true, "pbr": true, "texture_quality": p.TextureQuality, "quad": false, "smart_low_poly": true, "generate_parts": false}
	path := "/generation/text-to-model"
	if kind == "decimate" {
		path = "/mesh/decimate"
		body = map[string]any{"model": "v2.0", "input": p.Input, "face_limit": p.FaceLimit, "quad": false, "bake": true}
	}
	var data Task
	if err := c.request(ctx, http.MethodPost, path, body, &data); err != nil {
		return "", err
	}
	if data.ID == "" {
		return "", &APIError{Status: 200, Unknown: true}
	}
	return data.ID, nil
}
func (c *Client) Query(ctx context.Context, id string) (Task, error) {
	var t Task
	err := c.request(ctx, http.MethodGet, "/tasks/"+url.PathEscape(id), nil, &t)
	return t, err
}
func (c *Client) request(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		if method == http.MethodPost {
			return &APIError{Unknown: true}
		}
		return fmt.Errorf("Tripo 查询网络失败")
	}
	defer res.Body.Close()
	var envelope struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	err = json.NewDecoder(io.LimitReader(res.Body, 2<<20)).Decode(&envelope)
	if err != nil || envelope.Code == nil {
		return &APIError{Status: res.StatusCode, Unknown: method == http.MethodPost}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 || *envelope.Code != 0 {
		return &APIError{Status: res.StatusCode, Code: *envelope.Code, Unknown: method == http.MethodPost && res.StatusCode >= 500}
	}
	if err = json.Unmarshal(envelope.Data, out); err != nil {
		return &APIError{Status: res.StatusCode, Unknown: method == http.MethodPost}
	}
	return nil
}
func (c *Client) Download(ctx context.Context, rawURL string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("无效产物地址")
	}
	if err = checkURL(u); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	res, err := c.DownloadHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("产物下载失败")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("产物下载 HTTP %d", res.StatusCode)
	}
	if res.ContentLength > DownloadLimit {
		return nil, fmt.Errorf("产物超过本地检查的 150 MiB 下载保护上限")
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, DownloadLimit+1))
	if err != nil {
		return nil, fmt.Errorf("产物下载不完整")
	}
	if int64(len(b)) > DownloadLimit {
		return nil, fmt.Errorf("产物超过下载保护上限")
	}
	return b, nil
}
