// Package gguf reads the metadata header of a GGUF model file: enough of it
// to size the KV cache exactly (flockd#46) and to learn the training
// context when the catalog does not say. Tensor data is never touched.
//
// Format (v2/v3, little-endian): magic "GGUF", uint32 version, uint64
// tensor count, uint64 kv count, then kv pairs of (string key, uint32
// type, value). Arrays carry an element type and count. The tokenizer
// vocabulary lives in these pairs too, so a header can be several MB;
// values we do not need are skipped, not stored.
package gguf

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Meta is the subset of the header that sizing needs. Zero means absent.
type Meta struct {
	Architecture    string
	BlockCount      int // transformer layers
	HeadCount       int
	HeadCountKV     int // total across layers when the header lists per-layer counts
	HeadCountKVList bool
	EmbeddingLength int
	KeyLength       int // per head; defaults to EmbeddingLength/HeadCount
	ValueLength     int
	ContextLength   int // training window
}

// KVBytesPerToken is the f16 KV-cache cost of one context token across all
// layers: 2 (K and V) × layers × kv heads × head size × 2 bytes. That is
// what llama-server allocates per --ctx-size token with its default cache
// type; models whose header lacks the geometry return 0 so callers fall
// back to their heuristic. Sliding-window and MLA architectures cost less
// than this figure, never more, so the estimate errs safe.
func (m Meta) KVBytesPerToken() int64 {
	if m.BlockCount <= 0 || m.HeadCount <= 0 {
		return 0
	}
	keyLen, valLen := m.KeyLength, m.ValueLength
	if keyLen <= 0 && m.EmbeddingLength > 0 {
		keyLen = m.EmbeddingLength / m.HeadCount
	}
	if valLen <= 0 {
		valLen = keyLen
	}
	if keyLen <= 0 {
		return 0
	}
	kvHeads := int64(m.HeadCountKV)
	if kvHeads <= 0 {
		kvHeads = int64(m.HeadCount) // multi-head attention: one KV head per head
	}
	if m.HeadCountKVList {
		return kvHeads * int64(keyLen+valLen) * 2
	}
	return int64(m.BlockCount) * kvHeads * int64(keyLen+valLen) * 2
}

const (
	magic = "GGUF"

	tUint8   = 0
	tInt8    = 1
	tUint16  = 2
	tInt16   = 3
	tUint32  = 4
	tInt32   = 5
	tFloat32 = 6
	tBool    = 7
	tString  = 8
	tArray   = 9
	tUint64  = 10
	tInt64   = 11
	tFloat64 = 12

	// maxString bounds one string value; the longest legitimate ones are
	// chat templates (tens of KB). Anything larger is a corrupt header.
	maxString = 16 << 20
	// maxArray bounds an array's element count (vocabularies are ~256k).
	maxArray = 1 << 24
)

// ReadMeta reads the header of the GGUF at path. For a sharded model pass
// the first shard: that is where the metadata lives.
func ReadMeta(path string) (Meta, error) {
	f, err := os.Open(path)
	if err != nil {
		return Meta{}, err
	}
	defer f.Close()
	return Read(f)
}

// Read parses the header from r.
func Read(r io.Reader) (Meta, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	var hdr [4]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return Meta{}, fmt.Errorf("gguf: %w", err)
	}
	if string(hdr[:]) != magic {
		return Meta{}, errors.New("gguf: not a GGUF file")
	}
	var version uint32
	if err := binary.Read(br, binary.LittleEndian, &version); err != nil {
		return Meta{}, fmt.Errorf("gguf: %w", err)
	}
	if version < 2 || version > 3 {
		return Meta{}, fmt.Errorf("gguf: unsupported version %d", version)
	}
	var nTensors, nKV uint64
	if err := binary.Read(br, binary.LittleEndian, &nTensors); err != nil {
		return Meta{}, fmt.Errorf("gguf: %w", err)
	}
	if err := binary.Read(br, binary.LittleEndian, &nKV); err != nil {
		return Meta{}, fmt.Errorf("gguf: %w", err)
	}
	if nKV > maxArray {
		return Meta{}, fmt.Errorf("gguf: implausible metadata count %d", nKV)
	}

	p := &parser{r: br}
	var m Meta
	// Keys are "<arch>.block_count" etc.; general.architecture normally
	// comes first, but keep every candidate until it is known.
	arch := ""
	ints := map[string]int64{}
	lists := map[string]int64{}
	for i := uint64(0); i < nKV; i++ {
		key, err := p.str()
		if err != nil {
			return Meta{}, err
		}
		typ, err := p.u32()
		if err != nil {
			return Meta{}, err
		}
		switch {
		case key == "general.architecture" && typ == tString:
			if arch, err = p.str(); err != nil {
				return Meta{}, err
			}
		case typ == tArray:
			sum, n, err := p.skipArray(wantList(key))
			if err != nil {
				return Meta{}, err
			}
			if n > 0 && wantList(key) {
				lists[key] = sum
			}
		default:
			v, isInt, err := p.scalar(typ)
			if err != nil {
				return Meta{}, err
			}
			if isInt && wantInt(key) {
				ints[key] = v
			}
		}
	}
	if arch == "" {
		return Meta{}, errors.New("gguf: header has no general.architecture")
	}
	m.Architecture = arch
	get := func(suffix string) int { return int(ints[arch+"."+suffix]) }
	m.BlockCount = get("block_count")
	m.HeadCount = get("attention.head_count")
	m.HeadCountKV = get("attention.head_count_kv")
	m.EmbeddingLength = get("embedding_length")
	m.KeyLength = get("attention.key_length")
	m.ValueLength = get("attention.value_length")
	m.ContextLength = get("context_length")
	if sum, ok := lists[arch+".attention.head_count_kv"]; ok {
		m.HeadCountKV, m.HeadCountKVList = int(sum), true
	}
	return m, nil
}

// wantInt reports whether an integer scalar with this key is kept. Only
// the geometry suffixes matter; the arch prefix is resolved afterwards.
func wantInt(key string) bool {
	for _, s := range []string{
		".block_count", ".attention.head_count", ".attention.head_count_kv",
		".embedding_length", ".attention.key_length", ".attention.value_length",
		".context_length",
	} {
		if strings.HasSuffix(key, s) {
			return true
		}
	}
	return false
}

// wantList: head_count_kv may be per-layer (an integer array) in models
// with heterogeneous layers; its sum is the total KV heads.
func wantList(key string) bool { return strings.HasSuffix(key, ".attention.head_count_kv") }

type parser struct{ r *bufio.Reader }

func (p *parser) u32() (uint32, error) {
	var v uint32
	err := binary.Read(p.r, binary.LittleEndian, &v)
	return v, err
}

func (p *parser) u64() (uint64, error) {
	var v uint64
	err := binary.Read(p.r, binary.LittleEndian, &v)
	return v, err
}

func (p *parser) str() (string, error) {
	n, err := p.u64()
	if err != nil {
		return "", fmt.Errorf("gguf: %w", err)
	}
	if n > maxString {
		return "", fmt.Errorf("gguf: string of %d bytes", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(p.r, b); err != nil {
		return "", fmt.Errorf("gguf: %w", err)
	}
	return string(b), nil
}

// scalar reads one non-array value and returns it as an int64 when it is
// an integer type.
func (p *parser) scalar(typ uint32) (v int64, isInt bool, err error) {
	switch typ {
	case tUint8:
		var x uint8
		err = binary.Read(p.r, binary.LittleEndian, &x)
		return int64(x), true, err
	case tInt8:
		var x int8
		err = binary.Read(p.r, binary.LittleEndian, &x)
		return int64(x), true, err
	case tUint16:
		var x uint16
		err = binary.Read(p.r, binary.LittleEndian, &x)
		return int64(x), true, err
	case tInt16:
		var x int16
		err = binary.Read(p.r, binary.LittleEndian, &x)
		return int64(x), true, err
	case tUint32:
		var x uint32
		err = binary.Read(p.r, binary.LittleEndian, &x)
		return int64(x), true, err
	case tInt32:
		var x int32
		err = binary.Read(p.r, binary.LittleEndian, &x)
		return int64(x), true, err
	case tUint64:
		var x uint64
		err = binary.Read(p.r, binary.LittleEndian, &x)
		return int64(x), true, err
	case tInt64:
		var x int64
		err = binary.Read(p.r, binary.LittleEndian, &x)
		return x, true, err
	case tFloat32, tBool:
		_, err = p.r.Discard(map[uint32]int{tFloat32: 4, tBool: 1}[typ])
		return 0, false, err
	case tFloat64:
		_, err = p.r.Discard(8)
		return 0, false, err
	case tString:
		_, err = p.str()
		return 0, false, err
	}
	return 0, false, fmt.Errorf("gguf: unknown value type %d", typ)
}

// skipArray consumes an array value. When sum is requested and the
// elements are integers their total is returned with the count.
func (p *parser) skipArray(sum bool) (total int64, n uint64, err error) {
	elem, err := p.u32()
	if err != nil {
		return 0, 0, fmt.Errorf("gguf: %w", err)
	}
	n, err = p.u64()
	if err != nil {
		return 0, 0, fmt.Errorf("gguf: %w", err)
	}
	if n > maxArray {
		return 0, 0, fmt.Errorf("gguf: array of %d elements", n)
	}
	if elem == tArray {
		for i := uint64(0); i < n; i++ {
			if _, _, err := p.skipArray(false); err != nil {
				return 0, 0, err
			}
		}
		return 0, n, nil
	}
	for i := uint64(0); i < n; i++ {
		v, isInt, err := p.scalar(elem)
		if err != nil {
			return 0, 0, err
		}
		if sum && isInt {
			total += v
		}
	}
	return total, n, nil
}
