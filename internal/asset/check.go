// Package asset 根据实际 GLB 字节执行确定性的技术检查，不评判资产外观与语义。
package asset

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/qmuntal/gltf"
	"github.com/qmuntal/gltf/modeler"
)

// Check 是可供页面和执行记录直接展示的一项检查证据。
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// Report 同时保存实测值与验收上限，避免把生成目标误当作实际结果。
// Valid 表示文件处于可检查的支持范围，Passed 还要求面数与体积均合格。
type Report struct {
	Valid        bool    `json:"valid"`
	Passed       bool    `json:"passed"`
	Triangles    int     `json:"triangles"`
	Bytes        int64   `json:"bytes"`
	MaxTriangles int     `json:"max_triangles"`
	MaxBytes     int64   `json:"max_bytes"`
	Checks       []Check `json:"checks"`
	Visual       string  `json:"visual"`
}

// Inspect 只检查文件有效性、所有网格的总三角面数和实际文件体积。
// 文件无法可靠解析时，面数标为无法验证，而不是用零面数宣告通过。
func Inspect(data []byte, maxTriangles int, maxBytes int64) Report {
	r := Report{Bytes: int64(len(data)), MaxTriangles: maxTriangles, MaxBytes: maxBytes, Visual: "未进行视觉检查；技术通过不代表类别、风格或外观符合需求。"}
	triangles, err := geometry(data)
	r.Valid = err == nil
	if err != nil {
		r.Checks = append(r.Checks, Check{"文件有效性", "failed", err.Error()}, Check{"几何规模", "unverifiable", "文件未通过有效性检查，不能给出可信面数"})
	} else {
		r.Triangles = triangles
		r.Checks = append(r.Checks, Check{"文件有效性", "passed", "静态、自包含、非空三角面 GLB"}, Check{"几何规模", verdict(triangles <= maxTriangles), fmt.Sprintf("实测 %d / 上限 %d 个三角面", triangles, maxTriangles)})
	}
	r.Checks = append(r.Checks, Check{"文件体积", verdict(r.Bytes <= maxBytes), fmt.Sprintf("实测 %d / 上限 %d 字节", r.Bytes, maxBytes)})
	r.Passed = r.Valid && triangles <= maxTriangles && r.Bytes <= maxBytes
	return r
}

func verdict(ok bool) string {
	if ok {
		return "passed"
	}
	return "failed"
}

// geometry 先核对容器长度和资源引用，再允许解码器读取缓冲区。
// 第三方模型中的非法索引即使触发解码器 panic，也转成检查失败，不使服务退出。
func geometry(data []byte) (triangles int, err error) {
	defer func() {
		if recover() != nil {
			triangles = 0
			err = fmt.Errorf("模型包含无效引用或损坏的数据")
		}
	}()
	if len(data) < 20 || string(data[:4]) != "glTF" || binary.LittleEndian.Uint32(data[4:8]) != 2 || uint64(binary.LittleEndian.Uint32(data[8:12])) != uint64(len(data)) {
		return 0, fmt.Errorf("不是长度有效的 GLB 2.0 文件")
	}
	// GLB 使用小端长度字段，区块按 4 字节对齐；先校验长度再切片。
	jsonSize := int(binary.LittleEndian.Uint32(data[12:16]))
	if jsonSize%4 != 0 || jsonSize <= 0 || jsonSize > len(data)-20 || binary.LittleEndian.Uint32(data[16:20]) != 0x4e4f534a {
		return 0, fmt.Errorf("GLB JSON 区块无效")
	}
	var raw any
	if err = json.Unmarshal(data[20:20+jsonSize], &raw); err != nil {
		return 0, fmt.Errorf("模型 JSON 无法解析")
	}
	if err = checkReferences(raw); err != nil {
		return 0, err
	}
	for pos := 20 + jsonSize; pos < len(data); {
		if len(data)-pos < 8 {
			return 0, fmt.Errorf("GLB 区块头被截断")
		}
		n := int(binary.LittleEndian.Uint32(data[pos : pos+4]))
		if n%4 != 0 || n > len(data)-pos-8 {
			return 0, fmt.Errorf("GLB 二进制区块长度无效")
		}
		pos += 8 + n
	}
	var doc gltf.Document
	if err = gltf.NewDecoder(bytes.NewReader(data)).Decode(&doc); err != nil {
		return 0, fmt.Errorf("模型缓冲区无法解析：%w", err)
	}
	if doc.Asset.Version != "2.0" || len(doc.Animations) > 0 || len(doc.Skins) > 0 {
		return 0, fmt.Errorf("仅支持静态 GLB 2.0，不支持动画或骨骼")
	}
	for _, bv := range doc.BufferViews {
		if bv == nil || bv.Buffer < 0 || bv.Buffer >= len(doc.Buffers) || bv.ByteOffset < 0 || bv.ByteLength <= 0 {
			return 0, fmt.Errorf("无效 bufferView")
		}
		b := doc.Buffers[bv.Buffer]
		if b == nil || bv.ByteOffset > len(b.Data) || bv.ByteLength > len(b.Data)-bv.ByteOffset {
			return 0, fmt.Errorf("bufferView 超出实际文件")
		}
	}
	// accessor 的展开大小可能远大于文件体积；在读取稀疏数据前限制累计分配规模。
	remaining := 256 << 20
	for _, a := range doc.Accessors {
		if a == nil || a.Count < 1 || a.ByteOffset < 0 {
			return 0, fmt.Errorf("无效或无法安全验证的 accessor")
		}
		width := a.ComponentType.ByteSize() * a.Type.Components()
		if width <= 0 || a.Count > remaining/width {
			return 0, fmt.Errorf("展开后的网格数据超过 256 MiB 检查资源上限，无法验证")
		}
		remaining -= a.Count * width
		if _, e := modeler.ReadAccessor(&doc, a, nil); e != nil {
			return 0, fmt.Errorf("accessor 数据无效：%w", e)
		}
	}
	// 按文件内所有 mesh 的 primitive 计数，不只看默认场景可见对象，
	// 也不按节点实例数量重复累加同一个 mesh。
	for _, m := range doc.Meshes {
		if m == nil || len(m.Weights) > 0 {
			return 0, fmt.Errorf("不支持变形网格")
		}
		for _, p := range m.Primitives {
			if p == nil || p.Mode != gltf.PrimitiveTriangles || len(p.Targets) > 0 {
				return 0, fmt.Errorf("仅支持静态三角面网格")
			}
			idx, ok := p.Attributes["POSITION"]
			if !ok || idx < 0 || idx >= len(doc.Accessors) {
				return 0, fmt.Errorf("缺少有效顶点位置")
			}
			for _, attr := range p.Attributes {
				if attr < 0 || attr >= len(doc.Accessors) || doc.Accessors[attr].Count != doc.Accessors[idx].Count {
					return 0, fmt.Errorf("顶点属性引用或数量无效")
				}
			}
			if p.Material != nil && (*p.Material < 0 || *p.Material >= len(doc.Materials) || doc.Materials[*p.Material] == nil) {
				return 0, fmt.Errorf("网格引用的材质不存在")
			}
			positions, e := modeler.ReadPosition(&doc, doc.Accessors[idx], nil)
			if e != nil {
				return 0, e
			}
			for _, v := range positions {
				for _, c := range v {
					if math.IsNaN(float64(c)) || math.IsInf(float64(c), 0) {
						return 0, fmt.Errorf("顶点坐标不是有限数")
					}
				}
			}
			// 索引网格按索引条目计数，非索引网格按顶点条目计数，三条组成一面。
			count := len(positions)
			if p.Indices != nil {
				if *p.Indices < 0 || *p.Indices >= len(doc.Accessors) {
					return 0, fmt.Errorf("无效网格索引")
				}
				indices, e := modeler.ReadIndices(&doc, doc.Accessors[*p.Indices], nil)
				if e != nil {
					return 0, e
				}
				count = len(indices)
				for _, i := range indices {
					if uint64(i) >= uint64(len(positions)) {
						return 0, fmt.Errorf("三角面索引超出顶点范围")
					}
				}
			}
			if count < 3 || count%3 != 0 {
				return 0, fmt.Errorf("网格为空或三角面索引数量无效")
			}
			triangles += count / 3
		}
	}
	if triangles == 0 {
		return 0, fmt.Errorf("模型没有非空三角面网格")
	}
	if err := sceneReferences(&doc); err != nil {
		return 0, err
	}
	return triangles, nil
}

// sceneReferences 验证场景引用构成无环、无多父节点的层级，并确认默认场景可展示网格。
func sceneReferences(doc *gltf.Document) error {
	parents := make([]int, len(doc.Nodes))
	for _, n := range doc.Nodes {
		if n == nil {
			return fmt.Errorf("节点无效")
		}
		if n.Skin != nil || len(n.Weights) > 0 {
			return fmt.Errorf("不支持骨骼或变形节点")
		}
		if n.Mesh != nil && (*n.Mesh < 0 || *n.Mesh >= len(doc.Meshes)) {
			return fmt.Errorf("节点引用的网格不存在")
		}
		for _, c := range n.Children {
			if c < 0 || c >= len(doc.Nodes) {
				return fmt.Errorf("子节点不存在")
			}
			parents[c]++
			if parents[c] > 1 {
				return fmt.Errorf("节点具有多个父节点")
			}
		}
	}
	// 已排除多父节点，用根节点队列遍历即可发现循环，避免递归栈随模型深度增长。
	var queue []int
	for i, count := range parents {
		if count == 0 {
			queue = append(queue, i)
		}
	}
	for i := 0; i < len(queue); i++ {
		queue = append(queue, doc.Nodes[queue[i]].Children...)
	}
	if len(queue) != len(doc.Nodes) {
		return fmt.Errorf("节点层级存在循环")
	}
	if len(doc.Scenes) == 0 || (doc.Scene != nil && (*doc.Scene < 0 || *doc.Scene >= len(doc.Scenes))) {
		return fmt.Errorf("缺少有效场景")
	}
	selected := 0
	if doc.Scene != nil {
		selected = *doc.Scene
	}
	for i, scene := range doc.Scenes {
		if scene == nil {
			return fmt.Errorf("场景无效")
		}
		seen := map[int]bool{}
		for _, root := range scene.Nodes {
			if root < 0 || root >= len(doc.Nodes) || parents[root] != 0 || seen[root] {
				return fmt.Errorf("场景根节点无效")
			}
			seen[root] = true
		}
		if i == selected {
			pending := append([]int(nil), scene.Nodes...)
			found := false
			for j := 0; j < len(pending); j++ {
				n := doc.Nodes[pending[j]]
				found = found || (n.Mesh != nil && len(doc.Meshes[*n.Mesh].Primitives) > 0)
				pending = append(pending, n.Children...)
			}
			if !found {
				return fmt.Errorf("默认场景没有可展示的网格")
			}
		}
	}
	return nil
}

// checkReferences 在解码前拒绝外部资源与当前不支持的网格压缩。
// data URI 仍属于文件自包含数据；此处不发起网络请求。
func checkReferences(v any) error {
	switch x := v.(type) {
	case map[string]any:
		for k, v := range x {
			if k == "uri" {
				s, ok := v.(string)
				if !ok || !strings.HasPrefix(s, "data:") {
					return fmt.Errorf("不支持外部资源引用")
				}
			}
			if strings.Contains(strings.ToLower(k), "meshopt") || strings.Contains(strings.ToLower(k), "draco") {
				return fmt.Errorf("不支持压缩网格")
			}
			if err := checkReferences(v); err != nil {
				return err
			}
		}
	case []any:
		for _, v := range x {
			if err := checkReferences(v); err != nil {
				return err
			}
		}
	case string:
		if strings.HasPrefix(x, "EXT_meshopt") || strings.HasPrefix(x, "KHR_draco") {
			return fmt.Errorf("不支持压缩网格")
		}
	}
	return nil
}
