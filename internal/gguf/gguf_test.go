package gguf

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// enc builds a GGUF header the way llama.cpp's writer does: magic,
// version 3, no tensors, then the pairs in order.
type enc struct {
	buf bytes.Buffer
	n   uint64
}

func (e *enc) u32(v uint32) { _ = binary.Write(&e.buf, binary.LittleEndian, v) }
func (e *enc) u64(v uint64) { _ = binary.Write(&e.buf, binary.LittleEndian, v) }
func (e *enc) s(v string)   { e.u64(uint64(len(v))); e.buf.WriteString(v) }

func (e *enc) str(key, val string)       { e.n++; e.s(key); e.u32(tString); e.s(val) }
func (e *enc) uint(key string, v uint32) { e.n++; e.s(key); e.u32(tUint32); e.u32(v) }
func (e *enc) f32(key string, v float32) {
	e.n++
	e.s(key)
	e.u32(tFloat32)
	_ = binary.Write(&e.buf, binary.LittleEndian, v)
}
func (e *enc) strs(key string, vals ...string) {
	e.n++
	e.s(key)
	e.u32(tArray)
	e.u32(tString)
	e.u64(uint64(len(vals)))
	for _, v := range vals {
		e.s(v)
	}
}
func (e *enc) ints(key string, vals ...int32) {
	e.n++
	e.s(key)
	e.u32(tArray)
	e.u32(tInt32)
	e.u64(uint64(len(vals)))
	for _, v := range vals {
		_ = binary.Write(&e.buf, binary.LittleEndian, v)
	}
}

func (e *enc) bytes(version uint32) []byte {
	var out bytes.Buffer
	out.WriteString(magic)
	_ = binary.Write(&out, binary.LittleEndian, version)
	_ = binary.Write(&out, binary.LittleEndian, uint64(0)) // tensors
	_ = binary.Write(&out, binary.LittleEndian, e.n)
	out.Write(e.buf.Bytes())
	out.WriteString("tensor infos would follow; never read")
	return out.Bytes()
}

// Llama-3.2 3B's geometry, in the order llama.cpp writes it, with the
// tokenizer arrays that make real headers large.
func llama3B() *enc {
	e := &enc{}
	e.str("general.architecture", "llama")
	e.str("general.name", "Llama 3.2 3B Instruct")
	e.uint("llama.block_count", 28)
	e.uint("llama.context_length", 131072)
	e.uint("llama.embedding_length", 3072)
	e.uint("llama.attention.head_count", 24)
	e.uint("llama.attention.head_count_kv", 8)
	e.f32("llama.rope.freq_base", 500000)
	e.uint("llama.attention.key_length", 128)
	e.uint("llama.attention.value_length", 128)
	e.strs("tokenizer.ggml.tokens", "<|begin_of_text|>", "hello", "world")
	e.ints("tokenizer.ggml.token_type", 3, 1, 1)
	e.str("tokenizer.chat_template", "{{ messages }}")
	return e
}

func TestReadLlamaGeometry(t *testing.T) {
	m, err := Read(bytes.NewReader(llama3B().bytes(3)))
	if err != nil {
		t.Fatal(err)
	}
	want := Meta{Architecture: "llama", BlockCount: 28, HeadCount: 24, HeadCountKV: 8,
		EmbeddingLength: 3072, KeyLength: 128, ValueLength: 128, ContextLength: 131072}
	if m != want {
		t.Fatalf("got %+v\nwant %+v", m, want)
	}
	// 2 × 28 layers × 8 KV heads × 128 × 2 bytes: 112 KB/token, which is
	// what llama-server allocates (26.7 GB RSS at 262k tokens on the laptop).
	if got := m.KVBytesPerToken(); got != 114688 {
		t.Fatalf("KVBytesPerToken = %d, want 114688", got)
	}
	// v2 headers use the same layout.
	if m2, err := Read(bytes.NewReader(llama3B().bytes(2))); err != nil || m2 != want {
		t.Fatalf("v2: %+v, %v", m2, err)
	}
}

func TestKeyLengthDefaultsToEmbeddingOverHeads(t *testing.T) {
	// Llama-3.1 8B headers omit key/value_length: 4096/32 = 128.
	m := Meta{BlockCount: 32, HeadCount: 32, HeadCountKV: 8, EmbeddingLength: 4096}
	if got := m.KVBytesPerToken(); got != 131072 {
		t.Fatalf("8B = %d, want 131072", got)
	}
	// Multi-head attention (no head_count_kv): every head has a KV pair.
	m = Meta{BlockCount: 12, HeadCount: 12, EmbeddingLength: 768}
	if got := m.KVBytesPerToken(); got != 12*12*64*2*2 {
		t.Fatalf("MHA = %d, want %d", got, 12*12*64*2*2)
	}
	// Missing geometry: 0, the caller keeps its heuristic.
	if got := (Meta{Architecture: "x"}).KVBytesPerToken(); got != 0 {
		t.Fatalf("no geometry = %d, want 0", got)
	}
}

func TestPerLayerKVHeadsAreSummed(t *testing.T) {
	// Gemma-style: head_count_kv is an array, one entry per layer.
	e := &enc{}
	e.str("general.architecture", "gemma4")
	e.uint("gemma4.block_count", 4)
	e.uint("gemma4.attention.head_count", 16)
	e.ints("gemma4.attention.head_count_kv", 8, 8, 4, 0)
	e.uint("gemma4.attention.key_length", 512)
	e.uint("gemma4.attention.value_length", 512)
	m, err := Read(bytes.NewReader(e.bytes(3)))
	if err != nil {
		t.Fatal(err)
	}
	if !m.HeadCountKVList || m.HeadCountKV != 20 {
		t.Fatalf("list: %+v", m)
	}
	if got := m.KVBytesPerToken(); got != 20*1024*2 {
		t.Fatalf("summed = %d, want %d", got, 20*1024*2)
	}
}

func TestArchitecturePrefixIsHonoured(t *testing.T) {
	// Another architecture's keys must not leak into the result.
	e := &enc{}
	e.uint("llama.block_count", 99)
	e.str("general.architecture", "qwen2")
	e.uint("qwen2.block_count", 28)
	e.uint("qwen2.attention.head_count", 12)
	e.uint("qwen2.attention.head_count_kv", 2)
	e.uint("qwen2.embedding_length", 1536)
	m, err := Read(bytes.NewReader(e.bytes(3)))
	if err != nil {
		t.Fatal(err)
	}
	if m.BlockCount != 28 || m.KVBytesPerToken() != 28*2*128*2*2 {
		t.Fatalf("%+v kv=%d", m, m.KVBytesPerToken())
	}
}

func TestRejectsWhatIsNotAHeader(t *testing.T) {
	if _, err := Read(bytes.NewReader([]byte("GGML"))); err == nil {
		t.Fatal("bad magic accepted")
	}
	if _, err := Read(bytes.NewReader(llama3B().bytes(1))); err == nil {
		t.Fatal("v1 accepted")
	}
	if _, err := Read(bytes.NewReader(llama3B().bytes(3)[:40])); err == nil {
		t.Fatal("truncated header accepted")
	}
	e := &enc{}
	e.uint("llama.block_count", 28)
	if _, err := Read(bytes.NewReader(e.bytes(3))); err == nil {
		t.Fatal("header without an architecture accepted")
	}
	// ReadMeta on a missing path is an error, not a panic.
	if _, err := ReadMeta(filepath.Join(t.TempDir(), "nope.gguf")); err == nil {
		t.Fatal("missing file accepted")
	}
}

// The real thing, when the laptop has it: the header must agree with the
// synthetic one above.
func TestReadTheLaptopModel(t *testing.T) {
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".teraflock", "models", "llama-3.2-3b-instruct-q4_k_m.gguf")
	if _, err := os.Stat(path); err != nil {
		t.Skip("no local model")
	}
	m, err := ReadMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.KVBytesPerToken() != 114688 || m.ContextLength != 131072 {
		t.Fatalf("%+v", m)
	}
}
