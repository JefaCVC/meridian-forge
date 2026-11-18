// Meridian Forge — git objects and packfiles.
//
// Pack reading handles OFS_DELTA and REF_DELTA chains; pack writing emits
// plain (undeltified) PACK v2, which the conformance suite accepts for every
// clone/fetch check. Objects are always re-hashed from their content, so the
// store can never hold a wrongly-addressed object.
package main

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// ---- commit / tree / tag parsing ----

// parseCommit extracts the tree oid, parent oids and message of a commit.
func parseCommit(data []byte) (tree string, parents []string, message string) {
	// Headers end at the first blank line.
	hdrEnd := bytes.Index(data, []byte("\n\n"))
	var hdr []byte
	if hdrEnd >= 0 {
		hdr = data[:hdrEnd]
		message = string(data[hdrEnd+2:])
	} else {
		hdr = data
	}
	for _, line := range bytes.Split(hdr, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("tree ")) {
			tree = string(line[5:])
		} else if bytes.HasPrefix(line, []byte("parent ")) {
			parents = append(parents, string(line[7:]))
		}
	}
	// The suite compares commit messages trimmed, but keep the raw message.
	return tree, parents, message
}

// parseTagTarget extracts the object oid and type a tag points at.
func parseTagTarget(data []byte) (target string, typ string) {
	for _, line := range bytes.Split(data, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("object ")) {
			target = string(line[7:])
		} else if bytes.HasPrefix(line, []byte("type ")) {
			typ = string(line[5:])
		}
		if len(line) == 0 {
			break
		}
	}
	return target, typ
}

// treeEntryRaw is one entry of a raw tree object.
type treeEntryRaw struct {
	Mode string
	Name string
	OID  string // hex
}

// parseTree parses a tree object into its entries, in stored order.
func parseTree(data []byte) []treeEntryRaw {
	var out []treeEntryRaw
	i := 0
	for i < len(data) {
		sp := bytes.IndexByte(data[i:], ' ')
		if sp < 0 {
			break
		}
		mode := string(data[i : i+sp])
		i += sp + 1
		nul := bytes.IndexByte(data[i:], 0)
		if nul < 0 || i+nul+21 > len(data) {
			break
		}
		name := string(data[i : i+nul])
		i += nul + 1
		oid := hex.EncodeToString(data[i : i+20])
		i += 20
		out = append(out, treeEntryRaw{Mode: mode, Name: name, OID: oid})
	}
	return out
}

// gitSortKey is the key git sorts tree entries by: directories sort as if
// their name ended with '/'.
func gitSortKey(name, mode string) string {
	if mode == "40000" {
		return name + "/"
	}
	return name
}

// buildTree serializes entries into a tree object, sorted git-style.
func buildTree(entries []treeEntryRaw) []byte {
	sorted := append([]treeEntryRaw{}, entries...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return gitSortKey(sorted[i].Name, sorted[i].Mode) < gitSortKey(sorted[j].Name, sorted[j].Mode)
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.Mode)
		buf.WriteByte(' ')
		buf.WriteString(e.Name)
		buf.WriteByte(0)
		raw, _ := hex.DecodeString(e.OID)
		buf.Write(raw)
	}
	return buf.Bytes()
}

// buildCommit serializes a commit object.
func buildCommit(tree string, parents []string, message string, epoch int64) []byte {
	var buf bytes.Buffer
	buf.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		buf.WriteString("parent " + p + "\n")
	}
	ident := fmt.Sprintf("Meridian Forge <forge@meridian-forge.local> %d +0000", epoch)
	buf.WriteString("author " + ident + "\n")
	buf.WriteString("committer " + ident + "\n")
	buf.WriteString("\n")
	buf.WriteString(message)
	if !strings.HasSuffix(message, "\n") {
		buf.WriteString("\n")
	}
	return buf.Bytes()
}

// ---- delta application ----

// applyDelta applies a git delta to a base object, returning the result.
func applyDelta(base, delta []byte) ([]byte, error) {
	// varint: source size
	readVarint := func(d []byte, pos *int) (uint64, error) {
		var v uint64
		var shift uint
		for {
			if *pos >= len(d) {
				return 0, errors.New("truncated delta varint")
			}
			b := d[*pos]
			*pos++
			v |= uint64(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return v, nil
	}
	pos := 0
	srcSize, err := readVarint(delta, &pos)
	if err != nil {
		return nil, err
	}
	if uint64(len(base)) != srcSize {
		return nil, errors.New("delta base size mismatch")
	}
	dstSize, err := readVarint(delta, &pos)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, dstSize)
	for pos < len(delta) {
		cmd := delta[pos]
		pos++
		if cmd&0x80 != 0 {
			// copy from base
			var off, size uint64
			for i := 0; i < 4; i++ {
				if cmd&(1<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, errors.New("truncated delta copy")
					}
					off |= uint64(delta[pos]) << uint(8*i)
					pos++
				}
			}
			for i := 0; i < 3; i++ {
				if cmd&(0x10<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, errors.New("truncated delta copy")
					}
					size |= uint64(delta[pos]) << uint(8*i)
					pos++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if off+size > uint64(len(base)) {
				return nil, errors.New("delta copy out of range")
			}
			out = append(out, base[off:off+size]...)
		} else if cmd != 0 {
			// insert literal
			if pos+int(cmd) > len(delta) {
				return nil, errors.New("truncated delta insert")
			}
			out = append(out, delta[pos:pos+int(cmd)]...)
			pos += int(cmd)
		} else {
			return nil, errors.New("delta opcode 0")
		}
	}
	if uint64(len(out)) != dstSize {
		return nil, errors.New("delta result size mismatch")
	}
	return out, nil
}

// ---- pack reading ----

// readPack parses a PACK v2 stream and returns its objects. resolve is called
// for REF_DELTA bases that are not in this pack (the store's objects).
// The returned objects are keyed by their computed oid.
func readPack(pack []byte, resolve func(oid string) *Object) (map[string]*Object, error) {
	if len(pack) < 12 || string(pack[:4]) != "PACK" {
		return nil, errors.New("not a packfile")
	}
	version := binary.BigEndian.Uint32(pack[4:8])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	count := binary.BigEndian.Uint32(pack[8:12])
	// Trailer is the last 20 bytes; entries live before it.
	end := len(pack) - 20
	if end < 12 {
		return nil, errors.New("pack too short")
	}

	type pending struct {
		typ    byte
		data   []byte // inflated data (delta data for delta entries)
		ofsBase int   // for OFS_DELTA: absolute pack offset of base
		refBase string // for REF_DELTA: base oid
		offset int     // pack offset of this entry
	}
	// byOffset maps pack offset -> resolved object (for OFS_DELTA bases).
	byOffset := map[int]*Object{}
	entries := make([]pending, 0, count)

	off := 12
	for i := uint32(0); i < count; i++ {
		if off >= end {
			return nil, errors.New("pack truncated")
		}
		entryOff := off
		// type+size varint
		b := pack[off]
		off++
		typ := (b >> 4) & 7
		size := uint64(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			if off >= end {
				return nil, errors.New("pack truncated in size varint")
			}
			b = pack[off]
			off++
			size |= uint64(b&0x7f) << shift
			shift += 7
		}
		var ofsBase int
		var refBase string
		if typ == 6 { // OFS_DELTA
			if off >= end {
				return nil, errors.New("pack truncated in ofs base")
			}
			b = pack[off]
			off++
			neg := uint64(b & 0x7f)
			for b&0x80 != 0 {
				if off >= end {
					return nil, errors.New("pack truncated in ofs base")
				}
				b = pack[off]
				off++
				neg = ((neg + 1) << 7) | uint64(b&0x7f)
			}
			ofsBase = entryOff - int(neg)
		} else if typ == 7 { // REF_DELTA
			if off+20 > end {
				return nil, errors.New("pack truncated in ref base")
			}
			refBase = hex.EncodeToString(pack[off : off+20])
			off += 20
		}
		// Inflate the zlib stream. flate does not over-read when the reader
		// implements io.ByteReader (bytes.Reader does), so the reader's
		// position after the stream is exact — one pass gives both the data
		// and the consumed length.
		br := bytes.NewReader(pack[off:end])
		zr, err := zlib.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("bad zlib stream: %w", err)
		}
		data, err := io.ReadAll(zr)
		zr.Close()
		if err != nil {
			return nil, fmt.Errorf("inflate failed: %w", err)
		}
		off += (len(pack) - 20 - off) - br.Len()
		if uint64(len(data)) != size {
			return nil, fmt.Errorf("inflated size mismatch: got %d want %d", len(data), size)
		}
		entries = append(entries, pending{typ: typ, data: data, ofsBase: ofsBase, refBase: refBase, offset: entryOff})
	}

	// Resolve entries in order (bases always precede their deltas in a pack).
	out := map[string]*Object{}
	for _, e := range entries {
		var obj *Object
		switch e.typ {
		case objCommit, objTree, objBlob, objTag:
			obj = &Object{Type: e.typ, Data: e.data}
		case 6: // OFS_DELTA
			base := byOffset[e.ofsBase]
			if base == nil {
				return nil, fmt.Errorf("ofs-delta base missing at offset %d", e.ofsBase)
			}
			data, err := applyDelta(base.Data, e.data)
			if err != nil {
				return nil, fmt.Errorf("ofs-delta apply: %w", err)
			}
			obj = &Object{Type: base.Type, Data: data}
		case 7: // REF_DELTA
			var base *Object
			if resolve != nil {
				base = resolve(e.refBase)
			}
			if base == nil {
				return nil, fmt.Errorf("ref-delta base %s missing", e.refBase)
			}
			data, err := applyDelta(base.Data, e.data)
			if err != nil {
				return nil, fmt.Errorf("ref-delta apply: %w", err)
			}
			obj = &Object{Type: base.Type, Data: data}
		default:
			return nil, fmt.Errorf("unknown pack entry type %d", e.typ)
		}
		byOffset[e.offset] = obj
		oid := hashObject(obj.Type, obj.Data)
		out[oid] = obj
	}
	return out, nil
}

// ---- pack writing ----

// writePack serializes objects into a PACK v2 stream (no deltas), sorted by
// oid for determinism. objects maps oid -> object.
func writePack(objects map[string]*Object) []byte {
	oids := make([]string, 0, len(objects))
	for oid := range objects {
		oids = append(oids, oid)
	}
	sort.Strings(oids)
	var buf bytes.Buffer
	// reserve: header 12 + entries; grow as needed
	buf.Grow(12 + len(oids)*64)
	h := sha1.New()
	// header
	hdr := make([]byte, 12)
	copy(hdr[0:4], "PACK")
	binary.BigEndian.PutUint32(hdr[4:8], 2)
	binary.BigEndian.PutUint32(hdr[8:12], uint32(len(oids)))
	buf.Write(hdr)
	h.Write(hdr)
	for _, oid := range oids {
		obj := objects[oid]
		var entry bytes.Buffer
		// type+size varint
		size := uint64(len(obj.Data))
		first := byte((obj.Type << 4) | byte(size&0x0f))
		size >>= 4
		if size > 0 {
			first |= 0x80
		}
		entry.WriteByte(first)
		for size > 0 {
			b := byte(size & 0x7f)
			size >>= 7
			if size > 0 {
				b |= 0x80
			}
			entry.WriteByte(b)
		}
		// zlib stream, best-speed deflate (packs here are small; latency wins)
		zw, _ := zlib.NewWriterLevel(&entry, flate.BestSpeed)
		zw.Write(obj.Data)
		zw.Close()
		buf.Write(entry.Bytes())
		h.Write(entry.Bytes())
	}
	// trailer: sha1 of everything before it
	buf.Write(h.Sum(nil))
	return buf.Bytes()
}

// emptyPack is the 32-byte packfile with zero objects.
func emptyPack() []byte {
	var buf bytes.Buffer
	hdr := make([]byte, 12)
	copy(hdr[0:4], "PACK")
	binary.BigEndian.PutUint32(hdr[4:8], 2)
	binary.BigEndian.PutUint32(hdr[8:12], 0)
	buf.Write(hdr)
	sum := sha1.Sum(hdr)
	buf.Write(sum[:])
	return buf.Bytes()
}
