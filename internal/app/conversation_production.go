package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/lkmaVanilla/tripo-3d-agent/internal/asset"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/tripo"
)

// productionVersion 限定初始引用或本Run已产出的版本，禁止串用同会话的其他历史模型。
func (s *Service) productionVersion(ctx context.Context, v Session, versionID string) (AssetVersion, error) {
	c, err := s.store.GetRunConversation(ctx, v.ID)
	if err != nil {
		return AssetVersion{}, err
	}
	a, err := s.store.GetAssetVersion(ctx, c.ID, versionID)
	if err != nil {
		return a, err
	}
	if (v.InputVersion == nil || v.InputVersion.ID != a.ID) && a.SourceRunID != v.ID {
		return AssetVersion{}, ErrInvalidVersion
	}
	return a, nil
}

// versionBytes 每次副作用前重新核对应用保存的不可变文件，绝不读用户路径。
func versionBytes(v AssetVersion) ([]byte, error) {
	f, err := os.Open(v.Path)
	if err != nil {
		return nil, fmt.Errorf("引用版本文件不可用")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, tripo.UploadLimit+1))
	if err != nil || len(b) > tripo.UploadLimit {
		return nil, fmt.Errorf("引用文件无法读取或超过供应商上限")
	}
	h := sha256.Sum256(b)
	if v.SHA256 == "" || hex.EncodeToString(h[:]) != v.SHA256 {
		return nil, fmt.Errorf("引用版本文件摘要不匹配")
	}
	if !asset.Inspect(b, 20_000, tripo.UploadLimit).Valid {
		return nil, fmt.Errorf("引用版本不是有效静态GLB")
	}
	return b, nil
}

func (s *Service) prepareVersionInput(ctx context.Context, id, opID string) (tripo.Params, error) {
	v, err := s.liveOperation(ctx, id, opID)
	if err != nil {
		return tripo.Params{}, err
	}
	op := v.Current
	if op.Stage != "ready" {
		return tripo.Params{}, fmt.Errorf("已进入生产提交，禁止重新准备输入")
	}
	version, err := s.productionVersion(ctx, v, op.InputVersionID)
	if err != nil {
		return tripo.Params{}, err
	}
	if op.Kind != "decimate" || op.Params.Input != "version:"+version.ID || version.SHA256 != op.InputSHA256 {
		return tripo.Params{}, fmt.Errorf("加工输入身份不匹配")
	}
	data, err := versionBytes(version)
	if err != nil {
		return tripo.Params{}, err
	}
	params := op.Params
	if op.PreparedInput != nil && op.PreparedInput.Token != "" {
		params.Input = op.PreparedInput.Token
		return params, nil
	}
	uploader, ok := s.provider.(tripo.FileUploader)
	if !ok {
		return params, fmt.Errorf("供应商未提供历史模型输入准备能力")
	}
	for {
		v, err = s.store.Edit(ctx, id, func(x *Session) error {
			if err := checkOperation(*x, opID); err != nil {
				return err
			}
			if x.Current.Stage != "ready" {
				return fmt.Errorf("生产阶段已改变")
			}
			if x.Current.PreparedInput == nil {
				x.Current.PreparedInput = &PreparedInput{}
			}
			if x.Current.PreparedInput.Attempts >= 3 {
				return fmt.Errorf("模型输入上传尝试已用完")
			}
			x.Current.PreparedInput.Attempts++
			return nil
		}, "input_preparing", map[string]any{"operation_id": opID, "version_id": version.ID})
		if err != nil {
			return params, err
		}
		deadline := time.Now().Add(45 * time.Second)
		if !v.Deadline.IsZero() && v.Deadline.Before(deadline) {
			deadline = v.Deadline
		}
		upCtx, cancel := context.WithDeadline(ctx, deadline)
		token, e := uploader.UploadModel(upCtx, data)
		cancel()
		if e != nil {
			if ctx.Err() != nil {
				return params, ctx.Err()
			}
			if v.Current.PreparedInput.Attempts >= 3 {
				return params, e
			}
			if err = wait(ctx, time.Second); err != nil {
				return params, err
			}
			continue
		}
		if token == "" {
			return params, fmt.Errorf("模型输入上传未返回引用")
		}
		_, err = s.store.Edit(ctx, id, func(x *Session) error {
			if err := checkOperation(*x, opID); err != nil {
				return err
			}
			if x.Current.Stage != "ready" {
				return fmt.Errorf("生产阶段已改变")
			}
			x.Current.PreparedInput.Token = token
			return nil
		}, "input_prepared", map[string]any{"operation_id": opID, "version_id": version.ID})
		if err != nil {
			return params, err
		}
		params.Input = token
		return params, nil
	}
}
