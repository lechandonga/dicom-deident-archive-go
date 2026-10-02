package dicom

import (
	"bytes"
	"encoding/binary"
	"strings"
)

// Writer serializes a Dataset to a PS3.10 byte stream.
//
// Output is always written with explicit VR little endian
// (1.2.840.10008.1.2.1). Deflated/big-endian inputs are therefore normalized
// on de-identified export. Group-length elements are dropped and recomputed
// for group 0002 only.
type Writer struct {
	bo binary.ByteOrder
}

func NewWriter() *Writer { return &Writer{bo: binary.LittleEndian} }

// WriteFile builds a complete DICOM file: 128-byte preamble, DICM, file
// meta and dataset. meta is the transformed group-0002 element list.
func (w *Writer) WriteFile(meta, body []*Element) []byte {
	var out bytes.Buffer
	out.Write(make([]byte, 128))
	out.WriteString("DICM")

	meta = w.withoutGroupLengths(meta)
	metaBytes := w.elements(meta, true)
	// (0002,0000) File Meta Information Group Length = length of the rest
	// of group 0002.
	gl := w.element(Tag{0x0002, 0x0000}, VRUL,
		[]byte{byte(len(metaBytes)), byte(len(metaBytes) >> 8),
			byte(len(metaBytes) >> 16), byte(len(metaBytes) >> 24)},
		nil, nil, false, true)
	out.Write(gl)
	out.Write(metaBytes)

	body = w.withoutGroupLengths(body)
	out.Write(w.elements(body, true))
	return out.Bytes()
}

func (w *Writer) withoutGroupLengths(els []*Element) []*Element {
	out := els[:0:0]
	for _, e := range els {
		if e.Tag.Element == 0x0000 {
			continue
		}
		out = append(out, e)
	}
	return out
}

func (w *Writer) elements(els []*Element, explicitVR bool) []byte {
	var buf bytes.Buffer
	for _, e := range els {
		buf.Write(w.serialize(e, explicitVR))
	}
	return buf.Bytes()
}

func (w *Writer) serialize(e *Element, explicitVR bool) []byte {
	switch {
	case e.VR == VRSQ:
		return w.serializeSequence(e)
	case len(e.Fragments) > 0:
		return w.serializeEncapsulated(e)
	default:
		return w.serializePrimitive(e)
	}
}

func (w *Writer) tagHead(t Tag, vr VR, vl uint32, explicitVR bool, longHeader bool) []byte {
	var b bytes.Buffer
	var tag [4]byte
	w.bo.PutUint16(tag[0:], t.Group)
	w.bo.PutUint16(tag[2:], t.Element)
	b.Write(tag[:])
	if explicitVR {
		b.WriteString(string(vr))
		if longHeader {
			b.Write([]byte{0, 0})
			var l [4]byte
			w.bo.PutUint32(l[:], vl)
			b.Write(l[:])
		} else {
			var l [2]byte
			w.bo.PutUint16(l[:], uint16(vl))
			b.Write(l[:])
		}
	} else {
		var l [4]byte
		w.bo.PutUint32(l[:], vl)
		b.Write(l[:])
	}
	return b.Bytes()
}

func padByte(vr VR) byte {
	if vr == VRUI {
		return 0
	}
	return ' '
}

func (w *Writer) element(t Tag, vr VR, raw []byte, values []string,
	frags [][]byte, undef bool, explicitVR bool) []byte {

	longHeader := longHeaderVRs[vr]
	var payload []byte
	switch {
	case vr == VRSQ:
		return nil // handled separately
	case frags != nil:
		var b bytes.Buffer
		for _, f := range frags {
			b.Write(w.itemTag(uint32(len(f))))
			b.Write(f)
		}
		b.Write(w.seqDelimTag())
		var head bytes.Buffer
		head.Write(w.tagHead(t, vr, undefinedLength, explicitVR, true))
		head.Write(b.Bytes())
		return head.Bytes()
	default:
		if values != nil {
			payload = []byte(strings.Join(values, "\\"))
		} else {
			payload = raw
		}
		vl := len(payload)
		if vl%2 == 1 {
			payload = append(payload, padByte(vr))
			vl++
		}
		var b bytes.Buffer
		b.Write(w.tagHead(t, vr, uint32(vl), explicitVR, longHeader))
		b.Write(payload)
		return b.Bytes()
	}
}

func (w *Writer) serializePrimitive(e *Element) []byte {
	if e.IsText() {
		return w.element(e.Tag, e.VR, nil, e.Values, nil, false, true)
	}
	return w.element(e.Tag, e.VR, e.Raw, nil, nil, false, true)
}

func (w *Writer) itemTag(vl uint32) []byte {
	var b [8]byte
	w.bo.PutUint16(b[0:], 0xFFFE)
	w.bo.PutUint16(b[2:], 0xE000)
	w.bo.PutUint32(b[4:], vl)
	return b[:]
}

func (w *Writer) itemDelimTag() []byte {
	var b [8]byte
	w.bo.PutUint16(b[0:], 0xFFFE)
	w.bo.PutUint16(b[2:], 0xE0DD)
	return b[:]
}

func (w *Writer) seqDelimTag() []byte {
	var b [8]byte
	w.bo.PutUint16(b[0:], 0xFFFE)
	w.bo.PutUint16(b[2:], 0xE00D)
	return b[:]
}

func (w *Writer) serializeSequence(e *Element) []byte {
	var b bytes.Buffer
	var tag [4]byte
	w.bo.PutUint16(tag[0:], e.Tag.Group)
	w.bo.PutUint16(tag[2:], e.Tag.Element)
	b.Write(tag[:])
	b.WriteString("SQ")
	b.Write([]byte{0, 0})
	var l [4]byte
	w.bo.PutUint32(l[:], undefinedLength)
	b.Write(l[:])

	for _, item := range e.Items {
		els := w.withoutGroupLengths(item.Elements)
		payload := w.elements(els, true)
		b.Write(w.itemTag(uint32(len(payload))))
		b.Write(payload)
	}
	b.Write(w.seqDelimTag())
	return b.Bytes()
}

func (w *Writer) serializeEncapsulated(e *Element) []byte {
	return w.element(e.Tag, e.VR, nil, nil, e.Fragments, true, true)
}
