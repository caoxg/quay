// Package onnxinfo reads what Quay checks before installing a policy: an ONNX model's inputs and
// outputs (name and shape) and its metadata, without the weights. It decodes just enough of the
// protobuf wire format, so it needs no ONNX or protobuf library.
package onnxinfo

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Tensor is one graph input or output. A dimension is -1 when it is symbolic (e.g. "batch").
type Tensor struct {
	Name  string  `json:"name"`
	Shape []int64 `json:"shape"`
}

// Model is the part of an ONNX ModelProto Quay looks at.
type Model struct {
	IRVersion int64             `json:"irVersion"`
	Producer  string            `json:"producer,omitempty"`
	Inputs    []Tensor          `json:"inputs"` // graph inputs that are not initializers
	Outputs   []Tensor          `json:"outputs"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// ReadFile parses the ONNX model at path.
func ReadFile(path string) (*Model, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse decodes an ONNX model.
func Parse(b []byte) (*Model, error) {
	m := &Model{Metadata: map[string]string{}}
	var graph []byte
	haveGraph := false
	err := fields(b, func(num int, wt int, v uint64, data []byte) error {
		switch {
		case num == 1 && wt == wireVarint: // ir_version
			m.IRVersion = int64(v)
		case num == 2 && wt == wireBytes: // producer_name
			m.Producer = string(data)
		case num == 7 && wt == wireBytes: // graph
			graph, haveGraph = data, true
		case num == 14 && wt == wireBytes: // metadata_props
			var k, val string
			if err := fields(data, func(n, w int, _ uint64, d []byte) error {
				if w == wireBytes && n == 1 {
					k = string(d)
				} else if w == wireBytes && n == 2 {
					val = string(d)
				}
				return nil
			}); err != nil {
				return err
			}
			m.Metadata[k] = val
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("not an ONNX model: %w", err)
	}
	if !haveGraph || m.IRVersion == 0 {
		return nil, errors.New("not an ONNX model (no graph)")
	}
	var inputs []Tensor
	initializers := map[string]bool{}
	err = fields(graph, func(num int, wt int, _ uint64, data []byte) error {
		if wt != wireBytes {
			return nil
		}
		switch num {
		case 5: // initializer (TensorProto): only its name
			return fields(data, func(n, w int, _ uint64, d []byte) error {
				if n == 8 && w == wireBytes {
					initializers[string(d)] = true
				}
				return nil
			})
		case 11, 12: // input, output (ValueInfoProto)
			t, err := valueInfo(data)
			if err != nil {
				return err
			}
			if num == 11 {
				inputs = append(inputs, t)
			} else {
				m.Outputs = append(m.Outputs, t)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("not an ONNX model: %w", err)
	}
	m.Inputs = []Tensor{}
	for _, t := range inputs { // models from older exporters also list their weights as inputs
		if !initializers[t.Name] {
			m.Inputs = append(m.Inputs, t)
		}
	}
	if m.Outputs == nil {
		m.Outputs = []Tensor{}
	}
	return m, nil
}

// valueInfo reads a ValueInfoProto: name = 1, type = 2 (TypeProto: tensor_type = 1 (elem_type = 1,
// shape = 2 (TensorShapeProto: dim = 1 (dim_value = 1, dim_param = 2)))).
func valueInfo(b []byte) (Tensor, error) {
	t := Tensor{Shape: []int64{}}
	err := fields(b, func(num, wt int, _ uint64, data []byte) error {
		switch {
		case num == 1 && wt == wireBytes:
			t.Name = string(data)
		case num == 2 && wt == wireBytes:
			return sub(data, 1, func(tensor []byte) error {
				return sub(tensor, 2, func(shape []byte) error {
					return sub(shape, 1, func(dim []byte) error {
						d := int64(-1)
						err := fields(dim, func(n, w int, v uint64, _ []byte) error {
							if n == 1 && w == wireVarint {
								d = int64(v)
							}
							return nil
						})
						t.Shape = append(t.Shape, d)
						return err
					})
				})
			})
		}
		return nil
	})
	return t, err
}

// String formats a shape as [1,61] (a symbolic dimension shows as ?).
func ShapeString(s []int64) string {
	p := make([]string, len(s))
	for i, d := range s {
		if d < 0 {
			p[i] = "?"
		} else {
			p[i] = fmt.Sprint(d)
		}
	}
	return "[" + strings.Join(p, ",") + "]"
}

// SameShape compares a model's shape with a wanted one; a symbolic dimension (-1) on either side
// matches anything, so a batch dimension exported as "batch" still matches 1.
func SameShape(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] >= 0 && want[i] >= 0 && got[i] != want[i] {
			return false
		}
	}
	return true
}

const (
	wireVarint = 0
	wire64     = 1
	wireBytes  = 2
	wire32     = 5
)

// sub calls fn for every length-delimited field num in b.
func sub(b []byte, num int, fn func([]byte) error) error {
	return fields(b, func(n, w int, _ uint64, d []byte) error {
		if n == num && w == wireBytes {
			return fn(d)
		}
		return nil
	})
}

// fields walks the protobuf fields in b.
func fields(b []byte, fn func(num, wt int, v uint64, data []byte) error) error {
	for len(b) > 0 {
		key, n := varint(b)
		if n <= 0 {
			return errors.New("bad field key")
		}
		b = b[n:]
		num, wt := int(key>>3), int(key&7)
		if num == 0 {
			return errors.New("bad field number")
		}
		var v uint64
		var data []byte
		switch wt {
		case wireVarint:
			v, n = varint(b)
			if n <= 0 {
				return errors.New("bad varint")
			}
			b = b[n:]
		case wire64:
			if len(b) < 8 {
				return errors.New("truncated")
			}
			b = b[8:]
		case wire32:
			if len(b) < 4 {
				return errors.New("truncated")
			}
			b = b[4:]
		case wireBytes:
			l, n := varint(b)
			if n <= 0 || l > uint64(len(b)-n) {
				return errors.New("truncated")
			}
			data, b = b[n:n+int(l)], b[n+int(l):]
		default:
			return fmt.Errorf("unsupported wire type %d", wt)
		}
		if err := fn(num, wt, v, data); err != nil {
			return err
		}
	}
	return nil
}

func varint(b []byte) (uint64, int) {
	var x uint64
	for i := 0; i < len(b) && i < 10; i++ {
		x |= uint64(b[i]&0x7f) << (7 * i)
		if b[i] < 0x80 {
			return x, i + 1
		}
	}
	return 0, 0
}
