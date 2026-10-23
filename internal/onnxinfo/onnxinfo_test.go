package onnxinfo

import (
	"os"
	"path/filepath"
	"testing"
)

// pb builds protobuf messages for the test.
type pb []byte

func (b pb) varint(num int, v uint64) pb {
	return appendVarint(appendVarint(b, uint64(num)<<3|wireVarint), v)
}
func (b pb) bytes(num int, d []byte) pb {
	b = appendVarint(b, uint64(num)<<3|wireBytes)
	return append(appendVarint(b, uint64(len(d))), d...)
}
func (b pb) str(num int, s string) pb { return b.bytes(num, []byte(s)) }

func appendVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

// valueInfoPB encodes a float tensor; a negative dimension is written as dim_param "batch".
func valueInfoPB(name string, dims ...int64) pb {
	var shape pb
	for _, d := range dims {
		if d < 0 {
			shape = shape.bytes(1, pb(nil).str(2, "batch"))
		} else {
			shape = shape.bytes(1, pb(nil).varint(1, uint64(d)))
		}
	}
	tensor := pb(nil).varint(1, 1).bytes(2, shape)
	return pb(nil).str(1, name).bytes(2, pb(nil).bytes(1, tensor))
}

func model(meta ...string) []byte {
	weights := pb(nil).varint(1, 61).varint(1, 14).varint(2, 1).str(8, "fc.weight").bytes(9, make([]byte, 64)) // dims, type, name, raw_data
	graph := pb(nil).
		bytes(1, pb(nil).str(1, "obs").str(2, "actions").str(4, "Gemm")). // a node
		str(2, "main_graph").
		bytes(5, weights).
		bytes(11, valueInfoPB("fc.weight", 61, 14)). // weights listed as an input, like old exporters do
		bytes(11, valueInfoPB("obs", -1, 61)).
		bytes(12, valueInfoPB("actions", 1, 14))
	m := pb(nil).varint(1, 8).str(2, "pytorch").bytes(8, pb(nil).varint(2, 17)).bytes(7, graph)
	for i := 0; i+1 < len(meta); i += 2 {
		m = m.bytes(14, pb(nil).str(1, meta[i]).str(2, meta[i+1]))
	}
	return m
}

func TestParse(t *testing.T) {
	b := model("joint_names", "a,b", "run_path", "local")
	// Appending a field to a protobuf message is valid: a later metadata entry wins.
	b = append(b, pb(nil).bytes(14, pb(nil).str(1, "joint_names").str(2, "b,a"))...)
	m, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if m.IRVersion != 8 || m.Producer != "pytorch" {
		t.Fatalf("header: %+v", m)
	}
	if len(m.Inputs) != 1 || m.Inputs[0].Name != "obs" || ShapeString(m.Inputs[0].Shape) != "[?,61]" {
		t.Fatalf("inputs: %+v", m.Inputs)
	}
	if len(m.Outputs) != 1 || m.Outputs[0].Name != "actions" || ShapeString(m.Outputs[0].Shape) != "[1,14]" {
		t.Fatalf("outputs: %+v", m.Outputs)
	}
	if m.Metadata["joint_names"] != "b,a" || m.Metadata["run_path"] != "local" {
		t.Fatalf("metadata: %v", m.Metadata)
	}
	if !SameShape(m.Inputs[0].Shape, []int64{1, 61}) || SameShape(m.Inputs[0].Shape, []int64{1, 62}) || SameShape(m.Outputs[0].Shape, []int64{14}) {
		t.Fatal("SameShape")
	}
}

func TestRejects(t *testing.T) {
	good := model()
	for name, b := range map[string][]byte{
		"empty":     {},
		"truncated": good[:len(good)-5],
		"text":      []byte("hello, this is not a model"),
		"zip":       {0x50, 0x4b, 0x03, 0x04, 0x14, 0x00},
		"no graph":  pb(nil).varint(1, 8).str(2, "x"),
	} {
		if _, err := Parse(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestRuntimeModels reads real models when a runtime checkout is around (ONNX_DIR=…/python).
func TestRuntimeModels(t *testing.T) {
	dir := os.Getenv("ONNX_DIR")
	if dir == "" {
		t.Skip("set ONNX_DIR to a folder of .onnx files")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.onnx"))
	for _, f := range files {
		m, err := ReadFile(f)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		t.Logf("%s: in %s out %s, %d metadata keys", filepath.Base(f), ShapeString(m.Inputs[0].Shape), ShapeString(m.Outputs[0].Shape), len(m.Metadata))
	}
}
