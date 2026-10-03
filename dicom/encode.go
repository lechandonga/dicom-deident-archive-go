package dicom

import (
	"fmt"
	"io"
)

// vrForTag returns the standard VR for the identity/date tags the rewriter
// touches, so headers can be re-emitted in implicit VR datasets without a
// full data dictionary. Unknown tags are rewritten only when they already
// carry an explicit VR.
func vrForTag(t Tag) string {
	switch t {
	case TagMediaStorageSOPClassUID, TagMediaStorageSOPInstance, TagTransferSyntaxUID,
		TagSOPClassUID, TagSOPInstanceUID, TagStudyInstanceUID, TagSeriesInstanceUID:
		return "UI"
	case TagPatientID, TagStudyID:
		return "LO"
	case TagPatientName:
		return "PN"
	case TagStudyDate, TagPatientBirthDate:
		return "DA"
	case Tag{0x0008, 0x0021}, Tag{0x0008, 0x0022}, Tag{0x0008, 0x0023}:
		return "DA"
	case Tag{0x0008, 0x1155}, Tag{0x0020, 0x0052}:
		return "UI"
	case Tag{0x0008, 0x0060}:
		return "CS"
	case Tag{0x0010, 0x0040}:
		return "CS"
	case Tag{0x0020, 0x0011}, Tag{0x0020, 0x0013}:
		return "IS"
	case tagPatientIdentityRemoved:
		return "CS"
	case tagDeidentificationMethod:
		return "LO"
	case tagPrivateCreator:
		return "LO"
	case TagDeidentFingerprint:
		return "LO"
	case TagDeidentOriginalSOPUID:
		return "UI"
	case tagDeidentMethod:
		return "CS"
	}
	return ""
}

// padValue pads a value to even length: NUL for UI, space for textual VRs.
func padValue(vr string, b []byte) []byte {
	if len(b)%2 == 0 {
		return b
	}
	if vr == "UI" {
		return append(b, 0)
	}
	return append(b, ' ')
}

// writeElement emits a complete element (header + value) using syn.
func writeElement(w io.Writer, syn syntax, e Element, value []byte) error {
	vr := e.VR
	if vr == "" {
		vr = vrForTag(e.Tag)
	}
	value = padValue(vr, value)
	if err := writeHeader(w, syn, e.Tag, vr, uint32(len(value)), false); err != nil {
		return err
	}
	_, err := w.Write(value)
	return err
}

// writeHeader emits an element header.
func writeHeader(w io.Writer, syn syntax, t Tag, vr string, length uint32, undefined bool) error {
	var tagHdr [4]byte
	syn.order.PutUint16(tagHdr[0:2], t.Group)
	syn.order.PutUint16(tagHdr[2:4], t.Element)
	if _, err := w.Write(tagHdr[:]); err != nil {
		return err
	}
	if !syn.explicit {
		var lb [4]byte
		if undefined {
			length = undefinedLength
		}
		syn.order.PutUint32(lb[:], length)
		_, err := w.Write(lb[:])
		return err
	}
	if _, err := io.WriteString(w, vr); err != nil {
		return err
	}
	if longVRs[vr] {
		var lb [6]byte
		lb[0], lb[1] = 0, 0
		if undefined {
			length = undefinedLength
		}
		syn.order.PutUint32(lb[2:6], length)
		_, err := w.Write(lb[:])
		return err
	}
	if undefined {
		return fmt.Errorf("dicom: cannot emit undefined length for short VR %s %s", vr, tagName(t))
	}
	if length > 0xFFFF {
		return fmt.Errorf("dicom: rewritten value too long for short VR %s %s", vr, tagName(t))
	}
	var lb [2]byte
	syn.order.PutUint16(lb[:], uint16(length))
	_, err := w.Write(lb[:])
	return err
}

// writeSeqHeader emits a sequence header, always undefined length.
func writeSeqHeader(w io.Writer, syn syntax, e Element) error {
	vr := e.VR
	if vr == "" {
		vr = "SQ"
	}
	return writeHeader(w, syn, e.Tag, vr, 0, true)
}

func writeUndefinedItemHeader(w io.Writer, syn syntax) error {
	var b [8]byte
	syn.order.PutUint16(b[0:2], itemTag.Group)
	syn.order.PutUint16(b[2:4], itemTag.Element)
	syn.order.PutUint32(b[4:8], undefinedLength)
	_, err := w.Write(b[:])
	return err
}

func writeDelimiter(w io.Writer, t Tag, syn syntax) error {
	var b [8]byte
	syn.order.PutUint16(b[0:2], t.Group)
	syn.order.PutUint16(b[2:4], t.Element)
	syn.order.PutUint32(b[4:8], 0)
	_, err := w.Write(b[:])
	return err
}
