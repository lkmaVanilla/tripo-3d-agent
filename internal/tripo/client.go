// Package tripo 封装供应商 HTTP 协议，不负责 Agent 决策、业务预算或自动轮询。
// 提交得到任务 ID 后，由 app 层决定查询、下载、重试和终止的时机。
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

// Model 固定文本生成版本；减面端点在 Submit 中使用独立版本。
const Model = "v3.1-20260211"

// DownloadLimit 是防止下载消耗过多内存的保护值，不是资产验收的体积上限。
const DownloadLimit int64 = 150 * 1024 * 1024

// Params 表达一次生产输入；FaceLimit 是生成目标，不能当作实测三角面数。
type Params struct {
	Prompt         string `json:"prompt"`
	FaceLimit      int    `json:"face_limit"`
	TextureQuality string `json:"texture_quality"`
	Input          string `json:"input,omitempty"` // 减面使用的已有模型地址。
}

// Task 保存当前静态模型流程会用到的远端状态；不把所有任务类型都解释为模型文件。
type Task struct {
	ID           string  `json:"task_id"`
	Status       string  `json:"status"`
	Progress     int     `json:"progress"`
	ErrorCode    int     `json:"error_code,omitempty"`
	ErrorMessage string  `json:"error_message,omitempty"`
	Credits      float64 `json:"credits_consumed,omitempty"` // 任务累计用量，不能按轮询次数求和。
	Output       struct {
		ModelURL string `json:"model_url"`
	} `json:"output"`
}

// Provider 隔离真实供应商与受控测试实现；提交、查询和下载的重试边界各不相同。
type Provider interface {
	Submit(context.Context, string, Params) (string, error)
	Query(context.Context, string) (Task, error)
	Download(context.Context, string) ([]byte, error)
}

// Client 将带凭证的 API 请求和不带 API 凭证的产物下载分开配置。
type Client struct {
	BaseURL, APIKey string
	HTTP            *http.Client
	DownloadHTTP    *http.Client
}

// APIError.Unknown 表示无法确认生产是否已创建，调用方应保留扣额且禁止自动重发。
type APIError struct {
	Status, Code int
	Unknown      bool
}

func (e *APIError) Error() string {
	return fmt.Sprintf("Tripo 请求失败（HTTP %d，code %d，提交结果未知=%t）", e.Status, e.Code, e.Unknown)
}

// IsUnknown 允许业务层在错误被包装后仍识别提交结果未知的情况。
func IsUnknown(err error) bool { var e *APIError; return errors.As(err, &e) && e.Unknown }

// New 建立供应商客户端。下载地址来自外部响应，因此下载路径另外限制协议和目标 IP。
func New(key string) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// 下载直接连接已核对的目标 IP，避免代理替代本地地址校验。
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
		// 先核对本次解析出的全部地址，再直接拨号这些地址，避免校验后重新解析。
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

// publicIP 排除回环、私网与链路本地等不应由产物下载访问的地址。
func publicIP(ip net.IP) bool {
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsUnspecified()
}

// checkURL 同时用于初始地址和每次重定向；URL 不允许携带用户名密码。
func checkURL(u *url.URL) error {
	if u.Scheme != "https" || u.User != nil || u.Hostname() == "" {
		return fmt.Errorf("产物地址必须为 HTTPS")
	}
	return nil
}

// Submit 只尝试创建一次远端任务，不在本层自动重试 POST。
// 业务层需先持久化提交意图与扣额，再调用本方法，并尽快保存返回的任务 ID。
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
		// HTTP 成功但缺少任务 ID，也不能证明没有发生收费生产。
		return "", &APIError{Status: 200, Unknown: true}
	}
	return data.ID, nil
}

// Query 读取一个已知任务；它不创建新生产，也不自行决定轮询间隔。
func (c *Client) Query(ctx context.Context, id string) (Task, error) {
	var t Task
	err := c.request(ctx, http.MethodGet, "/tasks/"+url.PathEscape(id), nil, &t)
	return t, err
}

// request 统一解码供应商信封格式，并对 POST 的不确定结果作保守分类。
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
			// 网络错误可能发生在远端接收之后，不能把它当作安全重发的证据。
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

// Download 获取供本地技术检查的完整字节；下载成功本身不代表模型合格。
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
	// 即使响应未提供或错误声明 Content-Length，也以实际读取量执行保护上限。
	b, err := io.ReadAll(io.LimitReader(res.Body, DownloadLimit+1))
	if err != nil {
		return nil, fmt.Errorf("产物下载不完整")
	}
	if int64(len(b)) > DownloadLimit {
		return nil, fmt.Errorf("产物超过下载保护上限")
	}
	return b, nil
}
