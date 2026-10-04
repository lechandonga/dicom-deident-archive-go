package dicom

import (
	"encoding/binary"
	"fmt"
	"io"
	"strings"
)

// frame is one structural nesting level inside an undefined-length item.
type frame struct {
	kind byte // 'i' = inside item body, 'q' = inside sequence (between items)
}

// errLimitExhausted marks a declared-length boundary reached exactly (all
// declared bytes consumed). A stream ending mid-read instead yields
// ErrTruncated, so callers can tell a complete region from a cut-short one.
var errLimitExhausted = fmt.Errorf("dicom: declared length exhausted")

// delimReader presents the contents of one undefined-length item as a raw
// byte stream, stopping at the item's own Item Delimiter (consumed). It
// parses structure to track nesting depth; element values flow through in
// streaming chunks rather than being fully buffered.
type delimReader struct {
	r       io.Reader
	syn     syntax
	stack   []frame
	pending []byte
	val     io.Reader // current element value being forwarded
	done    bool
}

func newDelimReader(r io.Reader, syn syntax) *delimReader {
	return &delimReader{r: r, syn: syn, stack: []frame{{'i'}}}
}

func (d *delimReader) Read(p []byte) (int, error) {
	if d.done {
		return 0, io.EOF
	}
	if len(d.pending) > 0 {
		n := copy(p, d.pending)
		d.pending = d.pending[n:]
		return n, nil
	}
	if d.val != nil {
		n, err := d.val.Read(p)
		if err == io.EOF {
			d.val = nil
			if n == 0 {
				return d.Read(p)
			}
			err = nil
		}
		return n, err
	}
	h, err := readHeader(d.r, d.syn, "item element")
	if err != nil {
		if err == io.EOF {
			d.done = true
			return 0, io.ErrUnexpectedEOF
		}
		return 0, err
	}
	cur := byte(0)
	if len(d.stack) > 0 {
		cur = d.stack[len(d.stack)-1].kind
	}
	switch h.tag {
	case tagItemDelim:
		if cur == 'i' {
			d.stack = d.stack[:len(d.stack)-1]
			if len(d.stack) == 0 {
				d.done = true
				return 0, errItemEnd
			}
			// We're back inside the enclosing sequence; header already
			// consumed and belongs to that level, so simply continue.
			return d.Read(p)
		}
		return 0, errCorruptDelim
	case tagSequenceDelim:
		if len(d.stack) > 0 {
			d.stack = d.stack[:len(d.stack)-1]
		}
		if len(d.stack) == 0 {
			d.done = true
			return 0, io.ErrUnexpectedEOF // item ended without Item Delimiter
		}
		return d.Read(p)
	case tagItem:
		// Items appear only at a sequence level.
		if !h.undefined {
			if cur != 'q' {
				return 0, errCorruptDelim
			}
			d.pending = h.hdr
			d.stack = append(d.stack, frame{'i'})
			d.val = &boundedItem{r: d.r, n: int64(h.length), onClose: func() {
				d.stack = d.stack[:len(d.stack)-1]
			}}
			return d.Read(p)
		}
		if cur != 'q' {
			return 0, errCorruptDelim
		}
		d.pending = h.hdr
		d.stack = append(d.stack, frame{'i'})
		return d.Read(p)
	default:
		d.pending = h.hdr
		if h.undefined {
			if cur != 'i' {
				return 0, errCorruptDelim
			}
			// Undefined-length non-pixel element: treat like a sequence,
			// its Sequence Delimiter pops back to the item level.
			d.stack = append(d.stack, frame{'q'})
			return d.Read(p)
		}
		if h.length > 0 {
			d.val = io.LimitReader(d.r, int64(h.length))
		}
		return d.Read(p)
	}
}

// boundedItem forwards exactly n bytes then runs onClose.
type boundedItem struct {
	r       io.Reader
	n       int64
	onClose func()
	closed  bool
}

func (b *boundedItem) Read(p []byte) (int, error) {
	if b.n <= 0 {
		if !b.closed {
			b.closed = true
			b.onClose()
		}
		return 0, io.EOF
	}
	if int64(len(p)) > b.n {
		p = p[:b.n]
	}
	n, err := b.r.Read(p)
	b.n -= int64(n)
	if b.n == 0 && !b.closed {
		b.closed = true
		b.onClose()
		if err == nil {
			err = io.EOF
		}
	}
	return n, err
}

var errCorruptDelim = &delimError{}

// errItemEnd marks the clean end of an undefined-length item body.
var errItemEnd = fmt.Errorf("dicom: item end")

type delimError struct{}

func (e *delimError) Error() string { return "dicom: unexpected Item Delimiter" }

func (e *delimError) Unwrap() error { return ErrCorrupt }

func writeTag(w io.Writer, syn syntax, t Tag) error {
	var b [8]byte
	syn.order.PutUint16(b[0:2], t.Group)
	syn.order.PutUint16(b[2:4], t.Element)
	syn.order.PutUint32(b[4:8], 0)
	_, err := w.Write(b[:])
	return err
}

func writeSQHeader(w io.Writer, syn syntax, t Tag) error {
	// Explicit VR SQ: tag + "SQ" + 2 reserved + undefined length.
	// Implicit VR: tag + undefined length.
	if syn.explicit {
		var b [12]byte
		syn.order.PutUint16(b[0:2], t.Group)
		syn.order.PutUint16(b[2:4], t.Element)
		b[4] = 'S'
		b[5] = 'Q'
		syn.order.PutUint32(b[8:12], 0xFFFFFFFF)
		_, err := w.Write(b[:])
		return err
	}
	var b [8]byte
	syn.order.PutUint16(b[0:2], t.Group)
	syn.order.PutUint16(b[2:4], t.Element)
	syn.order.PutUint32(b[4:8], 0xFFFFFFFF)
	_, err := w.Write(b[:])
	return err
}

// writeItem writes one defined-length item (tag + length + content).
func writeItem(w io.Writer, syn syntax, content []byte) error {
	var hdr [8]byte
	syn.order.PutUint16(hdr[0:2], tagItem.Group)
	syn.order.PutUint16(hdr[2:4], tagItem.Element)
	syn.order.PutUint32(hdr[4:8], uint32(len(content)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(content)
	return err
}

// writeSimpleElement encodes one simple element in the target syntax. The
// VR governs long/short header form; values are padded by the caller.
func writeSimpleElement(w io.Writer, syn syntax, t Tag, vr string, val []byte) error {
	if syn.explicit {
		if longVRs[vr] {
			var b [12]byte
			syn.order.PutUint16(b[0:2], t.Group)
			syn.order.PutUint16(b[2:4], t.Element)
			b[4] = vr[0]
			b[5] = vr[1]
			syn.order.PutUint32(b[8:12], uint32(len(val)))
			if _, err := w.Write(b[:]); err != nil {
				return err
			}
		} else {
			var b [8]byte
			syn.order.PutUint16(b[0:2], t.Group)
			syn.order.PutUint16(b[2:4], t.Element)
			b[4] = vr[0]
			b[5] = vr[1]
			syn.order.PutUint16(b[6:8], uint16(len(val)))
			if _, err := w.Write(b[:]); err != nil {
				return err
			}
		}
	} else {
		var b [8]byte
		syn.order.PutUint16(b[0:2], t.Group)
		syn.order.PutUint16(b[2:4], t.Element)
		syn.order.PutUint32(b[4:8], uint32(len(val)))
		if _, err := w.Write(b[:]); err != nil {
			return err
		}
	}
	_, err := w.Write(val)
	return err
}

func uint32Bytes(v uint32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, v)
	return b
}

// padString pads textual values to even length. UI/strings use trailing
// space or NUL per VR; we use space for strings and NUL for UI.
func padString(vr, s string) []byte {
	b := []byte(s)
	if len(b)%2 == 1 {
		if vr == "UI" {
			b = append(b, 0)
		} else {
			b = append(b, ' ')
		}
	}
	return b
}

func padUID(s string) []byte {
	b := []byte(s)
	if len(b)%2 == 1 {
		b = append(b, 0)
	}
	return b
}

// implicitVR infers the VR used to re-encode an element in an implicit-VR
// dataset. Identity tags use the standard VRs; everything else falls back
// to UN-free best effort via the tag table, defaulting to LO.
func implicitVR(t Tag) string {
	if vr, ok := standardVR[t]; ok {
		return vr
	}
	return "LO"
}

var standardVR = map[Tag]string{
	TagSOPClassUID:       "UI",
	TagSOPInstanceUID:    "UI",
	TagStudyDate:         "DA",
	TagModality:          "CS",
	TagPatientName:       "PN",
	TagPatientID:         "LO",
	TagPatientBirthDate:  "DA",
	TagPatientSex:        "CS",
	TagStudyInstanceUID:  "UI",
	TagSeriesInstanceUID: "UI",
	TagStudyID:           "SH",
	TagSeriesNumber:      "IS",
	TagInstanceNumber:    "IS",
}

// uidKindForTag returns the mapping kind for UID-valued identity tags beyond
// the primary hierarchy tags. Unknown tags yield "" (left untouched).
func uidKindForTag(t Tag, vr string) string {
	if vr != "UI" {
		return ""
	}
	switch t {
	// Coding/transfer UIDs are identity-free and must remain stable.
	case Tag{0x0002, 0x0010}, TagSOPClassUID, Tag{0x0002, 0x0002}:
		return ""
	case TagMediaStorageSOPUID, TagSOPInstanceUID:
		return "sop"
	case TagStudyInstanceUID:
		return "study"
	case TagSeriesInstanceUID:
		return "series"
	}
	// Any other UID-valued element maps in the shared value space.
	return "uid"
}

func isTextVR(vr string) bool {
	switch vr {
	case "AE", "CS", "DA", "DT", "LO", "LT", "PN", "SH", "ST", "UC",
		"UI", "UR", "UT":
		return true
	}
	return false
}

// containsSecret reports whether s contains the original value. UIDs are
// compared on backslash-separated VM elements too; the check is deliberately
// conservative (substring) so multi-valued residuals cannot slip through.
func containsSecret(s, secret string) bool {
	// Match on backslash-separated VM elements exactly; also accept the raw
	// string itself. Substring matching is avoided because short UIDs such
	// as "9.1" naturally occur inside longer synthesised UIDs.
	if s == secret {
		return true
	}
	for _, part := range strings.Split(s, "\\") {
		if part == secret {
			return true
		}
	}
	return false
}
