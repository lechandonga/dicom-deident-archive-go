// Package sample builds synthetic DICOM files for local, fully offline tests
// and demos. Files contain no PHI; the generator emits raw 8-bit grayscale
// pixel data and supports the transfer syntaxes the parser understands.
package sample

import (
	"bytes"
	"encoding/binary"

	"github.com/lechandonga/dicom-deident-archive-go/internal/dicom"
)

// Options describes one synthetic instance.
type Options struct {
	PatientID      string
	PatientName    string
	StudyUID       string
	SeriesUID      string
	SOPUID         string
	SOPClassUID    string
	Modality       string
	StudyDate      string // YYYYMMDD
	SeriesDate     string
	InstanceNumber string
	TransferSyntax string // one of dicom.TS*
	// Rows/Cols define an 8-bit MONOCHROME2 pixel matrix; payload is a
	// deterministic gradient so identical options yield identical content.
	Rows int
	Cols int

	// Mutations applied after building a valid file:
	TruncateBytes int    // drop this many trailing bytes
	BreakPreamble bool   // corrupt the DICM magic
	DropPatientID bool   // remove PatientID element
	DropSOPUID    bool   // remove dataset SOP instance UID
	BadDate       string // replace StudyDate with this invalid value
}

var bo = binary.LittleEndian

func putTag(b *bytes.Buffer, t dicom.Tag) {
	writeTag(b, t, binary.LittleEndian)
}

func writeTag(b *bytes.Buffer, t dicom.Tag, order binary.ByteOrder) {
	var x [4]byte
	order.PutUint16(x[0:], t.Group)
	order.PutUint16(x[2:], t.Element)
	b.Write(x[:])
}

func writeText(b *bytes.Buffer, t dicom.Tag, vr dicom.VR, v string, be bool) {
	var order binary.ByteOrder = bo
	if be {
		order = binary.BigEndian
	}
	writeTag(b, t, order)
	b.WriteString(string(vr))
	payload := []byte(v)
	if len(payload)%2 == 1 {
		if vr == dicom.VRUI {
			payload = append(payload, 0)
		} else {
			payload = append(payload, ' ')
		}
	}
	var l [2]byte
	order.PutUint16(l[:], uint16(len(payload)))
	b.Write(l[:])
	b.Write(payload)
}

func writeUS(b *bytes.Buffer, t dicom.Tag, v uint16, be, implicit bool) {
	var order binary.ByteOrder = bo
	if be {
		order = binary.BigEndian
	}
	var x [4]byte
	order.PutUint16(x[0:], t.Group)
	order.PutUint16(x[2:], t.Element)
	b.Write(x[:])
	if !implicit {
		b.WriteString("US")
		if be {
			b.Write([]byte{0, 2})
		} else {
			b.Write([]byte{2, 0})
		}
	} else {
		var z [4]byte
		bo.PutUint32(z[:], 2)
		b.Write(z[:])
	}
	var val [2]byte
	order.PutUint16(val[:], v)
	b.Write(val[:])
}

func writePixel(b *bytes.Buffer, rows, cols int, be, implicit bool) {
	var order binary.ByteOrder = bo
	if be {
		order = binary.BigEndian
	}
	var x [4]byte
	order.PutUint16(x[0:], dicom.TagPixelData.Group)
	order.PutUint16(x[2:], dicom.TagPixelData.Element)
	b.Write(x[:])
	if !implicit {
		b.WriteString("OB")
		b.Write([]byte{0, 0})
	}
	n := rows * cols
	if implicit {
		var l2 [4]byte
		bo.PutUint32(l2[:], uint32(n))
		b.Write(l2[:])
	} else {
		var l [4]byte
		order.PutUint32(l[:], uint32(n))
		b.Write(l[:])
	}
	pix := make([]byte, n)
	for i := range pix {
		pix[i] = byte((i * 7) % 251) // deterministic gradient
	}
	if n%2 == 1 {
		pix = append(pix, 0)
	}
	b.Write(pix)
}

// Build produces a synthetic DICOM file per opts.
func Build(o Options) []byte {
	if o.SOPClassUID == "" {
		o.SOPClassUID = "1.2.840.10008.5.1.4.1.1.7" // Secondary Capture
	}
	if o.TransferSyntax == "" {
		o.TransferSyntax = dicom.TSExplicitLE
	}
	if o.Rows == 0 {
		o.Rows = 4
	}
	if o.Cols == 0 {
		o.Cols = 4
	}

	var out bytes.Buffer
	out.Write(make([]byte, 128))
	if o.BreakPreamble {
		out.WriteString("XXXX")
	} else {
		out.WriteString("DICM")
	}

	// File meta (always explicit LE).
	metaElems := []struct {
		t   dicom.Tag
		vr  dicom.VR
		val string
	}{
		{dicom.TagMediaStorageSOPClassUID, dicom.VRUI, o.SOPClassUID},
		{dicom.TagMediaStorageSOPInstanceUID, dicom.VRUI, o.SOPUID},
		{dicom.TagTransferSyntaxUID, dicom.VRUI, o.TransferSyntax},
		{dicom.TagImplementationClassUID, dicom.VRUI, "1.2.840.10008.99.1"},
	}
	var meta bytes.Buffer
	for _, e := range metaElems {
		writeText(&meta, e.t, e.vr, e.val, false)
	}
	putTag(&out, dicom.Tag{Group: 0x0002})
	out.WriteString("UL")
	out.Write([]byte{4, 0}) // UL value length
	var glv [4]byte
	bo.PutUint32(glv[:], uint32(meta.Len()))
	out.Write(glv[:])
	out.Write(meta.Bytes())

	// Dataset. Big-endian files are written explicit VR BE (no deflate).
	be := o.TransferSyntax == dicom.TSExplicitBE
	implicit := o.TransferSyntax == dicom.TSImplicitLE

	add := func(t dicom.Tag, vr dicom.VR, v string) {
		if implicit {
			writeImplicitText(&out, t, vr, v)
		} else {
			writeText(&out, t, vr, v, be)
		}
	}

	writeTextCS := add
	_ = writeTextCS

	if !o.DropPatientID {
		add(dicom.TagPatientID, dicom.VRLO, o.PatientID)
	}
	if o.PatientName != "" {
		add(dicom.TagPatientName, dicom.VRPN, o.PatientName)
	}
	add(dicom.TagPatientSex, dicom.VRCS, "O")
	dateVal := o.StudyDate
	if o.BadDate != "" {
		dateVal = o.BadDate
	}
	if dateVal != "" {
		add(dicom.TagStudyDate, dicom.VRDA, dateVal)
	}
	if o.SeriesDate != "" {
		add(dicom.TagSeriesDate, dicom.VRDA, o.SeriesDate)
	}
	if o.Modality != "" {
		add(dicom.TagModality, dicom.VRCS, o.Modality)
	}
	add(dicom.TagSOPClassUID, dicom.VRUI, o.SOPClassUID)
	if !o.DropSOPUID {
		add(dicom.TagSOPInstanceUID, dicom.VRUI, o.SOPUID)
	}
	add(dicom.TagStudyInstanceUID, dicom.VRUI, o.StudyUID)
	add(dicom.TagSeriesInstanceUID, dicom.VRUI, o.SeriesUID)
	if o.InstanceNumber != "" {
		add(dicom.TagInstanceNumber, dicom.VRIS, o.InstanceNumber)
	}
	writeUS(&out, dicom.Tag{Group: 0x0028, Element: 0x0010}, uint16(o.Rows), be, implicit)
	writeUS(&out, dicom.Tag{Group: 0x0028, Element: 0x0011}, uint16(o.Cols), be, implicit)
	writeUS(&out, dicom.Tag{Group: 0x0028, Element: 0x0002}, 1, be, implicit)
	writeUS(&out, dicom.Tag{Group: 0x0028, Element: 0x0100}, 8, be, implicit)
	writePixel(&out, o.Rows, o.Cols, be, implicit)

	b := out.Bytes()
	if o.TruncateBytes > 0 && o.TruncateBytes < len(b) {
		b = b[:len(b)-o.TruncateBytes]
	}
	return b
}

// writeImplicitText writes a string element without VR bytes (Implicit LE).
func writeImplicitText(b *bytes.Buffer, t dicom.Tag, vr dicom.VR, v string) {
	var x [4]byte
	bo.PutUint16(x[0:], t.Group)
	bo.PutUint16(x[2:], t.Element)
	b.Write(x[:])
	payload := []byte(v)
	if len(payload)%2 == 1 {
		if vr == dicom.VRUI {
			payload = append(payload, 0)
		} else {
			payload = append(payload, ' ')
		}
	}
	var l [4]byte
	bo.PutUint32(l[:], uint32(len(payload)))
	b.Write(l[:])
	b.Write(payload)
}
