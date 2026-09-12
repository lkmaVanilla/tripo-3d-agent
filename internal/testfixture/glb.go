// Package testfixture contains synthetic assets used only by automated tests.
package testfixture

import (
	"bytes"
	"github.com/qmuntal/gltf"
	"github.com/qmuntal/gltf/modeler"
)

func Cube(triangles int) []byte {
	doc := gltf.NewDocument()
	positions := [][3]float32{{-1, -1, -1}, {1, -1, -1}, {1, 1, -1}, {-1, 1, -1}, {-1, -1, 1}, {1, -1, 1}, {1, 1, 1}, {-1, 1, 1}}
	faces := [][3]uint16{{0, 2, 1}, {0, 3, 2}, {4, 5, 6}, {4, 6, 7}, {0, 1, 5}, {0, 5, 4}, {3, 7, 6}, {3, 6, 2}, {0, 4, 7}, {0, 7, 3}, {1, 2, 6}, {1, 6, 5}}
	indices := make([]uint16, 0, triangles*3)
	for i := 0; i < triangles; i++ {
		f := faces[i%len(faces)]
		indices = append(indices, f[:]...)
	}
	position := modeler.WritePosition(doc, positions)
	index := modeler.WriteIndices(doc, indices)
	doc.Meshes = []*gltf.Mesh{{Primitives: []*gltf.Primitive{{Attributes: gltf.PrimitiveAttributes{"POSITION": position}, Indices: gltf.Index(index)}}}}
	doc.Nodes = []*gltf.Node{{Mesh: gltf.Index(0)}}
	doc.Scenes[0].Nodes = []int{0}
	var out bytes.Buffer
	if err := gltf.NewEncoder(&out).Encode(doc); err != nil {
		panic(err)
	}
	return out.Bytes()
}
