package tripo

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net/http"
	"strings"
)

// UploadLimit 使用供应商公布的模型文件上限，不是本地技术验收上限。
const UploadLimit = 150_000_000

// FileUploader 是后续加工所需的可选Provider能力，不改变旧任务Provider契约。
type FileUploader interface {
	UploadModel(context.Context, []byte) (string, error)
}

// UploadModel 仅做一次上传；次数、停止和恢复控制属于应用层。
func (c *Client) UploadModel(ctx context.Context, data []byte) (string, error) {
	if len(data) < 12 || len(data) > UploadLimit || string(data[:4]) != "glTF" {
		return "", Failure("upload", "local_validation", fmt.Errorf("上传需要不超过150 MB的GLB文件"))
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "model.glb")
	if err != nil {
		return "", Failure("upload", "local_validation", err)
	}
	if _, err = part.Write(data); err != nil {
		return "", Failure("upload", "local_validation", err)
	}
	if err = writer.Close(); err != nil {
		return "", Failure("upload", "local_validation", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/files", &body)
	if err != nil {
		return "", Failure("upload", "local_validation", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	var result struct {
		Token string `json:"file_token"`
	}
	detail, err := c.doAPI(req, "upload", &result)
	if err != nil {
		return "", err
	}
	if result.Token == "" {
		return "", failure(detail, "protocol", nil, false)
	}
	return result.Token, nil
}
