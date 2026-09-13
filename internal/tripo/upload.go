package tripo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
		return "", fmt.Errorf("上传需要不超过150 MB的GLB文件")
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "model.glb")
	if err != nil {
		return "", err
	}
	if _, err = part.Write(data); err != nil {
		return "", err
	}
	if err = writer.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/files", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("模型输入上传失败")
	}
	defer resp.Body.Close()
	var envelope struct {
		Code *int `json:"code"`
		Data struct {
			Token string `json:"file_token"`
		} `json:"data"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&envelope); err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 || envelope.Code == nil || *envelope.Code != 0 || envelope.Data.Token == "" {
		return "", fmt.Errorf("模型输入上传响应无效（HTTP %d）", resp.StatusCode)
	}
	return envelope.Data.Token, nil
}
