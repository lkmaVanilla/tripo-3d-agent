package tripo

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Diagnostic 只含可公开的请求事实；身份和尝试组由 Runtime 补充。
// 原始 body、URL、headers 和 cause 永不序列化到此结构。
type Diagnostic struct {
	SchemaVersion     int       `json:"schema_version"`
	Provider          string    `json:"provider"`
	Phase             string    `json:"phase"`
	Category          string    `json:"category"`
	CauseCode         string    `json:"cause_code,omitempty"`
	RequestStarted    bool      `json:"request_started"`
	OccurredAt        time.Time `json:"occurred_at"`
	DurationMS        int64     `json:"duration_ms"`
	HTTPStatus        *int      `json:"http_status,omitempty"`
	ProviderCode      *int      `json:"provider_code,omitempty"`
	TaskErrorCode     *int      `json:"task_error_code,omitempty"`
	ProviderTraceID   string    `json:"provider_trace_id,omitempty"`
	SubmissionUnknown bool      `json:"submission_unknown"`
	Message           string    `json:"message"`
	RunID             string    `json:"run_id,omitempty"`
	OperationID       string    `json:"operation_id,omitempty"`
	TaskID            string    `json:"task_id,omitempty"`
	AttemptID         string    `json:"attempt_id,omitempty"`
	RetryGroupID      string    `json:"retry_group_id,omitempty"`
	Attempt           int       `json:"attempt,omitempty"`
}

var diagnosticID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// LogValue 让文本和 JSON 日志都输出实际字段值，避免可选整数被格式化为指针地址。
func (d Diagnostic) LogValue() slog.Value {
	d = d.Safe()
	attrs := []slog.Attr{
		slog.Int("schema_version", d.SchemaVersion), slog.String("provider", d.Provider),
		slog.String("phase", d.Phase), slog.String("category", d.Category),
		slog.Bool("request_started", d.RequestStarted), slog.Time("occurred_at", d.OccurredAt),
		slog.Int64("duration_ms", d.DurationMS), slog.Bool("submission_unknown", d.SubmissionUnknown),
		slog.String("message", d.Message),
	}
	for _, field := range []struct{ name, value string }{
		{"cause_code", d.CauseCode}, {"provider_trace_id", d.ProviderTraceID}, {"run_id", d.RunID},
		{"operation_id", d.OperationID}, {"task_id", d.TaskID}, {"attempt_id", d.AttemptID}, {"retry_group_id", d.RetryGroupID},
	} {
		if field.value != "" {
			attrs = append(attrs, slog.String(field.name, field.value))
		}
	}
	for _, field := range []struct {
		name  string
		value *int
	}{
		{"http_status", d.HTTPStatus}, {"provider_code", d.ProviderCode}, {"task_error_code", d.TaskErrorCode},
	} {
		if field.value != nil {
			attrs = append(attrs, slog.Int(field.name, *field.value))
		}
	}
	if d.Attempt > 0 {
		attrs = append(attrs, slog.Int("attempt", d.Attempt))
	}
	return slog.GroupValue(attrs...)
}

// Safe 重建说明而非清洗任意异常正文；可选 secrets 用于去掉被响应头回显的凭证。
func (d Diagnostic) Safe(secrets ...string) Diagnostic {
	d.SchemaVersion, d.Provider = 1, "tripo"
	switch d.Phase {
	case "submit", "query", "upload", "download", "preflight":
	default:
		d.Phase = "unknown"
	}
	switch d.Category {
	case "dns", "connect", "tls", "timeout", "canceled", "http", "provider", "task_failed", "decode", "protocol", "local_validation", "unknown":
	default:
		d.Category = "unknown"
	}
	switch d.CauseCode {
	case "connection_reset", "connection_refused", "dns_not_found", "unexpected_eof":
	default:
		d.CauseCode = ""
	}
	for _, p := range []*string{&d.ProviderTraceID, &d.RunID, &d.OperationID, &d.TaskID, &d.AttemptID, &d.RetryGroupID} {
		if !diagnosticID.MatchString(*p) || strings.HasPrefix(*p, "tsk_") || strings.HasPrefix(*p, "file_") {
			*p = ""
		}
		for _, secret := range secrets {
			if secret != "" && strings.Contains(*p, secret) {
				*p = ""
			}
		}
	}
	if d.DurationMS < 0 {
		d.DurationMS = 0
	}
	if d.HTTPStatus != nil && (*d.HTTPStatus < 100 || *d.HTTPStatus > 599) {
		d.HTTPStatus = nil
	}
	phase := map[string]string{"submit": "提交 Tripo", "query": "查询 Tripo 任务", "upload": "准备并上传模型输入", "download": "下载模型文件", "preflight": "检查 Tripo 接入"}[d.Phase]
	if phase == "" {
		phase = "调用 Tripo"
	}
	reason := map[string]string{"dns": "域名解析失败", "connect": "网络连接失败", "tls": "TLS 连接失败", "timeout": "请求超时", "canceled": "本地调用被取消", "http": "HTTP 请求失败", "provider": "供应商拒绝请求", "task_failed": "远端任务失败或取消", "decode": "响应无法解析", "protocol": "响应缺少有效信息", "local_validation": "本地文件或地址检查未通过", "unknown": "调用失败，具体原因未记录"}[d.Category]
	d.Message = phase + "：" + reason + "。"
	if d.CauseCode == "connection_reset" {
		d.Message += "连接被重置。"
	}
	if d.CauseCode == "connection_refused" {
		d.Message += "连接被拒绝。"
	}
	if d.HTTPStatus != nil {
		d.Message += fmt.Sprintf("HTTP %d。", *d.HTTPStatus)
	}
	if d.ProviderCode != nil && *d.ProviderCode != 0 {
		d.Message += fmt.Sprintf("供应商错误码 %d。", *d.ProviderCode)
	}
	if d.TaskErrorCode != nil {
		d.Message += fmt.Sprintf("任务错误码 %d。", *d.TaskErrorCode)
	}
	return d
}

// APIError 兼容原 Status/Code/Unknown 判定，cause 只供 errors.Is/As 分类。
type APIError struct {
	Status, Code int
	Unknown      bool
	Detail       Diagnostic
	cause        error
}

func (e *APIError) Unwrap() error { return e.cause }
func (e *APIError) Error() string { return Describe(e, "unknown").Message }

func Describe(err error, phase string) Diagnostic {
	var e *APIError
	if errors.As(err, &e) {
		d := e.Detail
		if d.Phase == "" {
			d.Phase = phase
		}
		d.SubmissionUnknown = e.Unknown
		if d.HTTPStatus == nil && e.Status != 0 {
			n := e.Status
			d.HTTPStatus = &n
		}
		if d.ProviderCode == nil && e.Code != 0 {
			n := e.Code
			d.ProviderCode = &n
		}
		return d.Safe()
	}
	d := Diagnostic{Phase: phase}
	d.Category, d.CauseCode = classify(err)
	return d.Safe()
}

func classify(err error) (string, string) {
	var api *APIError
	if errors.As(err, &api) {
		return api.Detail.Category, api.Detail.CauseCode
	}
	var dns *net.DNSError
	var op *net.OpError
	var cert *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	var record tls.RecordHeaderError
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled", ""
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout", ""
	case errors.As(err, &dns):
		if dns.IsTimeout {
			return "timeout", ""
		}
		if dns.IsNotFound {
			return "dns", "dns_not_found"
		}
		return "dns", ""
	case errors.As(err, &cert), errors.As(err, &authority), errors.As(err, &record):
		return "tls", ""
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return "timeout", ""
	}
	if errors.Is(err, syscall.ECONNRESET) {
		return "connect", "connection_reset"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "connect", "connection_refused"
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return "connect", "unexpected_eof"
	}
	if errors.As(err, &op) {
		return "connect", ""
	}
	return "unknown", ""
}

// WithDiagnostic 将 Runtime 补齐的安全事实关联回错误，保留取消和未知提交的类型链。
func WithDiagnostic(cause error, d Diagnostic) *APIError {
	return failure(d, d.Category, cause, d.SubmissionUnknown)
}

// Failure 用于适配层或 Runtime 已经核实的协议/本地失败，不接收任意公开说明。
func Failure(phase, category string, cause error) *APIError {
	return &APIError{Detail: (Diagnostic{Phase: phase, Category: category}).Safe(), cause: cause}
}

func failure(d Diagnostic, category string, cause error, unknown bool) *APIError {
	if category == "" {
		category, d.CauseCode = classify(cause)
	}
	d.Category, d.SubmissionUnknown = category, unknown
	e := &APIError{Detail: d.Safe(), Unknown: unknown, cause: cause}
	if d.HTTPStatus != nil {
		e.Status = *d.HTTPStatus
	}
	if d.ProviderCode != nil {
		e.Code = *d.ProviderCode
	}
	return e
}
