package tripo

import (
	"fmt"
	"net/url"
	"strings"
)

const DefaultBaseURL = "https://openapi.tripo3d.com/v3"

// NormalizeBaseURL 限制带凭证的目标；错误不回显可能夹带密钥的原始配置。
func NormalizeBaseURL(raw string) (string, error) {
	if raw == "" {
		return DefaultBaseURL, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") || u.RawPath != "" {
		return "", fmt.Errorf("TRIPO_BASE_URL 必须为官方 Tripo V3 HTTPS 基址，不能包含凭证、查询或片段")
	}
	if (u.Hostname() != "openapi.tripo3d.com" && u.Hostname() != "openapi.tripo3d.ai") || (u.Port() != "" && u.Port() != "443") || strings.TrimSuffix(u.Path, "/") != "/v3" || strings.HasSuffix(u.Host, ":") {
		return "", fmt.Errorf("TRIPO_BASE_URL 的主机、端口或 V3 路径无效")
	}
	return "https://" + u.Hostname() + "/v3", nil
}

// NewWithBaseURL 是配置入口；测试仍可显式注入 Client，生产不接受任意网络地址。
func NewWithBaseURL(key, baseURL string) (*Client, error) {
	base, err := NormalizeBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	c := New(key)
	c.BaseURL = base
	return c, nil
}
