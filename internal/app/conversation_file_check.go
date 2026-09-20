package app

import "os"

// 不可变版本的公开可处理状态需要检测损坏，但每个WS心跳不应重复读完整模型。
// 缓存只用于展示；真正生产前 versionBytes 仍完整核对摘要与GLB技术有效性。
type versionFileCheck struct {
	info   os.FileInfo
	digest string
	valid  bool
}

func (s *Store) versionUsable(v AssetVersion) bool {
	if !versionUsable(v) {
		return false
	}
	info, e := os.Stat(v.Path)
	if e != nil {
		return false
	}
	s.versionCheckMu.Lock()
	defer s.versionCheckMu.Unlock()
	if old, ok := s.versionChecks[v.ID]; ok && old.digest == v.SHA256 && os.SameFile(old.info, info) && old.info.Size() == info.Size() && old.info.ModTime().Equal(info.ModTime()) {
		return old.valid
	}
	digest, size, e := conversationFileDigest(v.Path)
	valid := e == nil && digest == v.SHA256 && size == v.Bytes
	// 缓存有界；清空只增加一次重新核对，不改变事实。
	if len(s.versionChecks) >= 2048 || s.versionChecks == nil {
		s.versionChecks = map[string]versionFileCheck{}
	}
	s.versionChecks[v.ID] = versionFileCheck{info: info, digest: v.SHA256, valid: valid}
	return valid
}
