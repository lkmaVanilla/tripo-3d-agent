// Package tripo 封装供应商 HTTP 协议，不负责 Agent 决策、业务预算或自动轮询。
// 提交得到任务 ID 后，由 app 层决定查询、下载、重试和终止的时机。
package tripo

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// Model 固定文本生成版本；减面端点在 Submit 中使用独立版本。
const Model = "v3.1-20260211"

// DownloadLimit 是防止下载消耗过多内存的保护值，不是资产验收的体积上限。
const DownloadLimit int64 = 150 * 1024 * 1024

var ErrDownloadLimit = errors.New("产物超过本地检查的 150 MiB 下载保护上限")

// Params 表达一次生产输入；FaceLimit 是生成目标，不能当作实测三角面数。
type Params struct {
	Prompt         string `json:"prompt"`
	FaceLimit      int    `json:"face_limit"`
	TextureQuality string `json:"texture_quality"`
	Input          string `json:"input,omitempty"` // 减面使用的已有模型地址。
}

// Task 保存当前静态模型流程会用到的远端状态；不把所有任务类型都解释为模型文件。
type Task struct {
	// Call 只携带查询元数据，不进入供应商协议或原始任务序列化。
	Call         Diagnostic `json:"-"`
	ID           string     `json:"task_id"`
	Status       string     `json:"status"`
	Progress     int        `json:"progress"`
	ErrorCode    int        `json:"error_code,omitempty"`
	ErrorMessage string     `json:"error_message,omitempty"`
	Credits      float64    `json:"credits_consumed,omitempty"` // 任务累计用量，不能按轮询次数求和。
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

// IsUnknown 允许业务层在错误被包装后仍识别提交结果未知的情况。
func IsUnknown(err error) bool { var e *APIError; return errors.As(err, &e) && e.Unknown }

// New 建立供应商客户端。下载地址来自外部响应，因此下载路径另外限制协议和目标 IP。
func New(key string) *Client {
	apiTransport := http.DefaultTransport.(*http.Transport).Clone()
	// HTTP/2 即使 GetBody=nil，仍会为尚未写入的不可用连接自动换连接再试。
	// 带凭证 API 固定 HTTP/1.1，使一次尝试及其诊断严格对应一次传输尝试。
	apiTransport.Protocols = new(http.Protocols)
	apiTransport.Protocols.SetHTTP1(true)
	// Clone 会继承默认传输已初始化的 ALPN 列表；同时移除 h2 通告，
	// 避免服务端选中 h2，而本地却按 HTTP/1.1 解析响应。
	if apiTransport.TLSClientConfig == nil {
		apiTransport.TLSClientConfig = new(tls.Config)
	}
	apiTransport.TLSClientConfig.NextProtos = []string{"http/1.1"}
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
				return nil, Failure("download", "local_validation", fmt.Errorf("产物地址必须使用公共网络地址"))
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
	return &Client{BaseURL: DefaultBaseURL, APIKey: key, HTTP: &http.Client{Transport: apiTransport, Timeout: 45 * time.Second}, DownloadHTTP: &http.Client{Transport: transport, Timeout: 2 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
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
	detail, err := c.request(ctx, "submit", http.MethodPost, path, body, &data)
	if err != nil {
		return "", err
	}
	if data.ID == "" {
		// HTTP 成功但缺少任务 ID，也不能证明没有发生收费生产。
		return "", failure(detail, "protocol", nil, true)
	}
	return data.ID, nil
}

// Query 读取一个已知任务；它不创建新生产，也不自行决定轮询间隔。
func (c *Client) Query(ctx context.Context, id string) (Task, error) {
	var t Task
	detail, err := c.request(ctx, "query", http.MethodGet, "/tasks/"+url.PathEscape(id), nil, &t)
	t.Call = detail
	return t, err
}

// Download 获取供本地技术检查的完整字节；下载成功本身不代表模型合格。
func (c *Client) Download(ctx context.Context, rawURL string) (data []byte, err error) {
	start := time.Now()
	d := Diagnostic{Phase: "download"}
	defer func() {
		if err != nil {
			e, ok := err.(*APIError)
			if !ok {
				e = failure(d, "", err, false)
			}
			e.Detail.OccurredAt, e.Detail.DurationMS = time.Now().UTC(), time.Since(start).Milliseconds()
			e.Detail = e.Detail.Safe(c.APIKey)
			err = e
		}
	}()
	u, parseErr := url.Parse(rawURL)
	if parseErr != nil {
		return nil, failure(d, "local_validation", parseErr, false)
	}
	if checkErr := checkURL(u); checkErr != nil {
		return nil, failure(d, "local_validation", checkErr, false)
	}
	req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if reqErr != nil {
		return nil, failure(d, "local_validation", reqErr, false)
	}
	d.RequestStarted = true
	res, callErr := c.DownloadHTTP.Do(req)
	if callErr != nil {
		return nil, failure(d, "", callErr, false)
	}
	defer res.Body.Close()
	d.HTTPStatus = &res.StatusCode
	d.ProviderTraceID = res.Header.Get("X-Tripo-Trace-ID")
	if res.StatusCode != 200 {
		return nil, failure(d, "http", nil, false)
	}
	if res.ContentLength > DownloadLimit {
		return nil, failure(d, "local_validation", ErrDownloadLimit, false)
	}
	data, err = io.ReadAll(io.LimitReader(res.Body, DownloadLimit+1))
	if err != nil {
		return nil, failure(d, "", err, false)
	}
	if int64(len(data)) > DownloadLimit {
		return nil, failure(d, "local_validation", ErrDownloadLimit, false)
	}
	return data, nil
}
