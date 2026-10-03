package dicom

import (
	"bytes"
	"fmt"
	"io"
)

// Element is one parsed data element presented to transform callbacks.
type Element struct {
	Tag       Tag
	VR        string // "" in implicit VR datasets
	Length    uint32
	Undefined bool
}

// Transform streams a DICOM dataset through callbacks. When W is non-nil the
// dataset is reproduced with every non-rewritten byte preserved:
//
//   - OnValue returns (true, newValue) to replace a small buffered value,
//     (true, nil) to drop the element, or (false, nil) to keep it.
//   - OnStream receives large opaque values as a bounded reader; with W set
//     it must stream the bytes that should appear in the output.
//   - OnPixel fires once when pixel data (7FE0,0010) begins.
//
// Only metadata values up to maxBufferedValue are buffered; pixel data and
// large opaque values are streamed. The WHOLE file is always validated
// structurally, including content following the pixel data.
type Transform struct {
	W        io.Writer
	OnValue  func(depth int, e Element, val []byte) (rewrite bool, out []byte, err error)
	OnStream func(depth int, e Element, length int64, r io.Reader) error
	OnPixel  func(depth int, e Element) error
	// Inject is written into the output before the first top-level dataset
	// element (used for de-identification markers). Parse passes nil.
	Inject []byte

	injected bool
}

// run walks a full dataset until EOF.
func (t *Transform) run(br *reader, syn syntax, depth int) error {
	return t.walk(br, syn, depth, -1, false)
}

// walk processes elements. limit >= 0 bounds a defined-length item
// (br must already be bounded); expectDelim terminates on an item delimiter.
func (t *Transform) walk(br *reader, syn syntax, depth int, limit int64, expectDelim bool) error {
	var consumed int64
	emitInject := depth == 0
	for {
		if limit >= 0 && consumed >= limit {
			return nil
		}
		e, raw, err := readHeader(br, syn)
		if err == io.EOF {
			if depth == 0 && raw != nil && len(raw) > 0 {
				return fmt.Errorf("%w: %d stray byte(s) after last element", ErrTruncated, len(raw))
			}
			if expectDelim || limit >= 0 {
				return fmt.Errorf("%w: unterminated item", ErrTruncated)
			}
			return nil
		}
		if err != nil {
			return err
		}

		if e.Tag.Group == 0xFFFE {
			if e.Tag != itemDelimTag || !expectDelim || e.Length != 0 {
				return fmt.Errorf("%w: unexpected tag %s inside item", ErrCorrupt, tagName(e.Tag))
			}
			if t.W != nil {
				if _, err := t.W.Write(raw); err != nil {
					return err
				}
			}
			return nil
		}

		if emitInject && t.W != nil && !t.injected && len(t.Inject) > 0 {
			if _, err := t.W.Write(t.Inject); err != nil {
				return err
			}
			t.injected = true
		}
		emitInject = false

		if e.Tag == TagPixelData && depth == 0 {
			if t.OnPixel != nil {
				if err := t.OnPixel(depth, e); err != nil {
					return err
				}
			}
			n, err := t.passPixel(br, syn, e, raw)
			if err != nil {
				return err
			}
			consumed += n
			// Pixel data is normally the final element, but any legal
			// trailing elements are still validated; trailing garbage that
			// does not parse as an element is rejected by readHeader.
			continue
		}

		if isSequence(e, br, syn) {
			n, err := t.passSequence(br, syn, depth, e, raw)
			if err != nil {
				return err
			}
			consumed += n
			continue
		}

		n, err := t.passSimple(br, syn, depth, e, raw)
		if err != nil {
			return err
		}
		consumed += n
	}
}

// readHeader parses one header, preserving exact source bytes.
func readHeader(br *reader, syn syntax) (Element, []byte, error) {
	var raw bytes.Buffer
	var tagHdr [4]byte
	if _, err := io.ReadFull(io.TeeReader(br, &raw), tagHdr[:]); err != nil {
		if err == io.EOF && raw.Len() == 0 {
			return Element{}, nil, io.EOF
		}
		if raw.Len() > 0 {
			return Element{}, raw.Bytes(), fmt.Errorf("%w: partial tag header (%d bytes)", ErrTruncated, raw.Len())
		}
		return Element{}, nil, err
	}
	e := Element{Tag: Tag{syn.order.Uint16(tagHdr[0:2]), syn.order.Uint16(tagHdr[2:4])}}

	if e.Tag.Group == 0xFFFE {
		var lb [4]byte
		if _, err := io.ReadFull(io.TeeReader(br, &raw), lb[:]); err != nil {
			return e, nil, fmt.Errorf("%w: length of %s", ErrTruncated, tagName(e.Tag))
		}
		e.Length = syn.order.Uint32(lb[:])
		e.Undefined = e.Length == undefinedLength
		return e, raw.Bytes(), nil
	}

	if !syn.explicit {
		var lb [4]byte
		if _, err := io.ReadFull(io.TeeReader(br, &raw), lb[:]); err != nil {
			return e, nil, fmt.Errorf("%w: length of %s", ErrTruncated, tagName(e.Tag))
		}
		e.Length = syn.order.Uint32(lb[:])
		e.Undefined = e.Length == undefinedLength
		return e, raw.Bytes(), nil
	}

	var vrh [2]byte
	if _, err := io.ReadFull(io.TeeReader(br, &raw), vrh[:]); err != nil {
		return e, nil, fmt.Errorf("%w: VR of %s", ErrTruncated, tagName(e.Tag))
	}
	e.VR = string(vrh[:])
	if longVRs[e.VR] {
		var lb [6]byte
		if _, err := io.ReadFull(io.TeeReader(br, &raw), lb[:]); err != nil {
			return e, nil, fmt.Errorf("%w: length of %s", ErrTruncated, tagName(e.Tag))
		}
		e.Length = syn.order.Uint32(lb[2:6])
		e.Undefined = e.Length == undefinedLength
	} else {
		var lb [2]byte
		if _, err := io.ReadFull(io.TeeReader(br, &raw), lb[:]); err != nil {
			return e, nil, fmt.Errorf("%w: length of %s", ErrTruncated, tagName(e.Tag))
		}
		e.Length = uint32(syn.order.Uint16(lb[:]))
	}
	return e, raw.Bytes(), nil
}

// isSequence recognises SQ content. Explicit VR: VR is SQ, or UN with
// undefined length. Implicit VR: an undefined-length element beginning with
// an item tag (anything else with undefined length is corrupt).
func isSequence(e Element, br *reader, syn syntax) bool {
	if e.VR == "SQ" {
		return true
	}
	if e.VR == "UN" && e.Undefined {
		return true
	}
	if e.VR != "" || !e.Undefined {
		return false
	}
	var probe [8]byte
	n, err := io.ReadFull(br, probe[:])
	if err != nil {
		return false
	}
	br.unread = append(br.unread, probe[:n]...)
	grp := syn.order.Uint16(probe[0:2])
	elm := syn.order.Uint16(probe[2:4])
	return grp == 0xFFFE && (elm == 0xE000 || elm == 0xE0DD)
}

// passSimple handles a non-sequence, non-pixel element and returns the
// number of value bytes consumed.
func (t *Transform) passSimple(br *reader, syn syntax, depth int, e Element, raw []byte) (int64, error) {
	if e.Undefined {
		return 0, fmt.Errorf("%w: undefined length on non-sequence %s", ErrCorrupt, tagName(e.Tag))
	}
	if e.Length <= maxBufferedValue {
		buf := make([]byte, e.Length)
		if _, err := io.ReadFull(br, buf); err != nil {
			return 0, fmt.Errorf("%w: value of %s", ErrTruncated, tagName(e.Tag))
		}
		if t.OnValue != nil {
			rewrite, out, err := t.OnValue(depth, e, buf)
			if err != nil {
				return 0, err
			}
			if rewrite {
				if t.W != nil {
					if out == nil {
						return int64(e.Length), nil // dropped
					}
					if err := writeElement(t.W, syn, e, out); err != nil {
						return 0, err
					}
				}
				return int64(e.Length), nil
			}
		}
		if t.W != nil {
			if _, err := t.W.Write(raw); err != nil {
				return 0, err
			}
			if _, err := t.W.Write(buf); err != nil {
				return 0, err
			}
		}
		return int64(e.Length), nil
	}

	lr := io.LimitReader(br, int64(e.Length))
	if t.OnStream != nil {
		if err := t.OnStream(depth, e, int64(e.Length), lr); err != nil {
			return 0, err
		}
		if _, err := io.Copy(io.Discard, lr); err != nil {
			return 0, err
		}
	} else if t.W != nil {
		if _, err := t.W.Write(raw); err != nil {
			return 0, err
		}
		if _, err := io.Copy(t.W, lr); err != nil {
			return 0, err
		}
	} else {
		if _, err := io.Copy(io.Discard, lr); err != nil {
			return 0, err
		}
	}
	return int64(e.Length), nil
}

// passPixel copies pixel data verbatim. Encapsulated (undefined length)
// data is validated item-by-item: the basic offset table and every fragment
// are copied byte-for-byte.
func (t *Transform) passPixel(br *reader, syn syntax, e Element, raw []byte) (int64, error) {
	if t.W != nil {
		if _, err := t.W.Write(raw); err != nil {
			return 0, err
		}
	}
	if !e.Undefined {
		n, err := io.Copy(t.orDiscard(), io.LimitReader(br, int64(e.Length)))
		if err != nil {
			return 0, err
		}
		if n != int64(e.Length) {
			return 0, fmt.Errorf("%w: pixel data value of %s", ErrTruncated, tagName(e.Tag))
		}
		return int64(e.Length), nil
	}

	var n int64
	for {
		ie, iraw, err := readHeader(br, syn)
		if err != nil {
			return n, fmt.Errorf("%w: pixel fragment header: %v", ErrTruncated, err)
		}
		n += int64(len(iraw))
		switch {
		case ie.Tag == seqDelimTag:
			if ie.Length != 0 {
				return n, fmt.Errorf("%w: pixel sequence delimiter length %d", ErrCorrupt, ie.Length)
			}
			if t.W != nil {
				t.W.Write(iraw)
			}
			return n, nil
		case ie.Tag == itemTag:
			if ie.Undefined {
				return n, fmt.Errorf("%w: undefined-length pixel fragment", ErrCorrupt)
			}
			if t.W != nil {
				t.W.Write(iraw)
			}
			c, err := io.Copy(t.orDiscard(), io.LimitReader(br, int64(ie.Length)))
			if err != nil {
				return n, err
			}
			if c != int64(ie.Length) {
				return n, fmt.Errorf("%w: pixel fragment %d bytes", ErrTruncated, ie.Length)
			}
			n += c
		default:
			return n, fmt.Errorf("%w: unexpected tag %s in encapsulated pixel data", ErrCorrupt, tagName(ie.Tag))
		}
	}
}

func (t *Transform) orDiscard() io.Writer {
	if t.W != nil {
		return t.W
	}
	return io.Discard
}

// passSequence validates and reproduces one SQ. Output sequences always use
// undefined length with undefined-length items, so transformed child values
// never need a parent length recompute. Returns value bytes consumed.
func (t *Transform) passSequence(br *reader, syn syntax, depth int, e Element, raw []byte) (int64, error) {
	if t.W != nil {
		if err := writeSeqHeader(t.W, syn, e); err != nil {
			return 0, err
		}
	}

	if e.Undefined {
		var n int64
		for {
			ie, iraw, err := readHeader(br, syn)
			if err != nil {
				return n, fmt.Errorf("%w: item header in %s: %v", ErrTruncated, tagName(e.Tag), err)
			}
			n += int64(len(iraw))
			if ie.Tag == seqDelimTag {
				if ie.Length != 0 {
					return n, fmt.Errorf("%w: sequence delimiter length %d", ErrCorrupt, ie.Length)
				}
				if t.W != nil {
					t.W.Write(iraw)
				}
				return n, nil
			}
			if ie.Tag != itemTag {
				return n, fmt.Errorf("%w: expected item in %s, got %s", ErrCorrupt, tagName(e.Tag), tagName(ie.Tag))
			}
			c, err := t.passItem(br, syn, depth+1, ie)
			n += c
			if err != nil {
				return n, err
			}
		}
	}

	// Defined-length sequence: items are normally item-wrapped, but the
	// bare-dataset form (no item tags) is also accepted.
	var probe [8]byte
	k, err := io.ReadFull(br, probe[:])
	if err != nil {
		return 0, fmt.Errorf("%w: sequence %s content: %v", ErrTruncated, tagName(e.Tag), err)
	}
	if g := syn.order.Uint16(probe[0:2]); g != 0xFFFE || syn.order.Uint16(probe[2:4]) != 0xE000 {
		// Bare dataset occupying the whole declared length.
		br.unread = append(br.unread, probe[:k]...)
		if t.W != nil {
			if err := writeUndefinedItemHeader(t.W, syn); err != nil {
				return 0, err
			}
		}
		lr := &reader{r: io.LimitReader(br, int64(e.Length))}
		sub := *t
		sub.injected = false
		if err := sub.walk(lr, syn, depth+1, -1, false); err != nil {
			return 0, err
		}
		if err := discardFull(lr); err != io.EOF && err != nil {
			return 0, err
		}
		if t.W != nil {
			if err := writeDelimiter(t.W, itemDelimTag, syn); err != nil {
				return 0, err
			}
			if err := writeDelimiter(t.W, seqDelimTag, syn); err != nil {
				return 0, err
			}
		}
		return int64(e.Length), nil
	}

	// Item-wrapped. Probe held the first item header.
	remaining := int64(e.Length)
	for {
		var ie Element
		var iraw []byte
		if k == 8 {
			ie = Element{
				Tag:       itemTag,
				Length:    syn.order.Uint32(probe[4:8]),
				Undefined: syn.order.Uint32(probe[4:8]) == undefinedLength,
			}
			iraw = append(iraw, probe[:8]...)
			k = 0
		} else {
			if remaining <= 0 {
				break
			}
			var err error
			ie, iraw, err = readHeader(br, syn)
			if err != nil {
				return 0, err
			}
			if ie.Tag != itemTag {
				return 0, fmt.Errorf("%w: expected item in %s, got %s", ErrCorrupt, tagName(e.Tag), tagName(ie.Tag))
			}
		}
		remaining -= int64(len(iraw))
		c, err := t.passItem(br, syn, depth+1, ie)
		remaining -= c
		if err != nil {
			return 0, err
		}
		if remaining < 0 {
			return 0, fmt.Errorf("%w: sequence %s item overruns declared length", ErrTruncated, tagName(e.Tag))
		}
		if remaining == 0 {
			break
		}
	}
	if remaining != 0 {
		return 0, fmt.Errorf("%w: sequence %s short by %d bytes", ErrTruncated, tagName(e.Tag), remaining)
	}
	if t.W != nil {
		if err := writeDelimiter(t.W, seqDelimTag, syn); err != nil {
			return 0, err
		}
	}
	return int64(e.Length), nil
}

// passItem handles one item inside a sequence.
func (t *Transform) passItem(br *reader, syn syntax, depth int, e Element) (int64, error) {
	if t.W != nil {
		if err := writeUndefinedItemHeader(t.W, syn); err != nil {
			return 0, err
		}
	}
	if e.Undefined {
		// Consume until item delimiter. Count bytes by wrapping the source
		// reader through a counting reader.
		cr := &countingReader{r: br}
		sub := *t
		sub.injected = false
		counted := &reader{r: cr}
		if err := sub.walk(counted, syn, depth, -1, true); err != nil {
			return cr.n, err
		}
		// walk consumed the delimiter itself; the delimiter bytes are
		// included in the count.
		return cr.n, nil
	}

	lr := &reader{r: io.LimitReader(br, int64(e.Length))}
	sub := *t
	sub.injected = false
	if err := sub.walk(lr, syn, depth, int64(e.Length), false); err != nil {
		return 0, err
	}
	if err := discardFull(lr); err != io.EOF && err != nil {
		return 0, err
	}
	if t.W != nil {
		if err := writeDelimiter(t.W, itemDelimTag, syn); err != nil {
			return 0, err
		}
	}
	return int64(e.Length), nil
}

// countingReader counts every byte read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// discardFull returns nil when exactly zero bytes remain, io.EOF at the
// boundary of a LimitReader, or ErrTruncated on a short read.
func discardFull(br *reader) error {
	n, err := io.Copy(io.Discard, br)
	if err == nil && n > 0 {
		return nil
	}
	return err
}
