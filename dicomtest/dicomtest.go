// Package dicomtest builds synthetic DICOM P10 files for tests and sample
// data generation. It covers uncompressed syntaxes, encapsulated pixel
// data (JPEG lossless/RLE-like), undefined-length sequences and files
// without a file-meta group length declaration.
package dicomtest

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
)

const (
	TSImplicitLE = "1.2.840.10008.1.2"
	TSExplicitLE = "1.2.840.10008.1.2.1"
	TSExplicitBE = "1.2.840.10008.1.2.2"

	// TSJPEGLossless: Explicit VR LE, encapsulated (1.2.840.10008.1.2.70).
	TSJPEGLossless = "1.2.840.10008.1.2.70"
	// TSRLE: RLE Lossless, encapsulated (1.2.840.10008.1.2.5).
	TSRLE = "1.2.840.10008.1.2.5"
	// TSJPEGBaseline: encapsulated (1.2.840.10008.1.2.4.50).
	TSJPEGBaseline = "1.2.840.10008.1.2.4.50"
)

// Fields carries the metadata written into a synthetic file.
type Fields struct {
	PatientID    string
	PatientName  string
	BirthDate    string
	Sex          string
	StudyUID     string
	StudyDate    string
	StudyID      string
	SeriesUID    string
	SeriesNumber string
	Modality     string
	SOPClassUID  string
	SOPUID       string
	InstanceNo   string
}

// Options selects uncommon-but-legal encodings.
type Options struct {
	// EncapsulatedPixel renders pixel data as undefined-length items
	// (basic offset table + one fragment), as used by JPEG/RLE transfers.
	EncapsulatedPixel bool
	// PixelPayload is the exact fragment payload; defaults to PixelBytes.
	PixelPayload []byte
	// UndefinedSQ emits an undefined-length ReferencedSOPSequence whose
	// item contains UID fields (explicit VR only controls SQ header form;
	// implicit VR files sniff the item tag).
	UndefinedSQ bool
	// RefSOPUID is the UID stored inside UndefinedSQ's item; defaults to
	// an unrelated fixed UID. Set it to an instance/series hierarchy UID
	// to exercise nested UID remapping.
	RefSOPUID string
	// NoMetaGroupLength omits (0002,0000) entirely.
	NoMetaGroupLength bool
	// Extra is appended raw after the pixel data element.
	Extra []byte
}

// PixelBytes lets tests distinguish file content independently of metadata.
var PixelBytes = []byte{0xDE, 0xAD, 0xBE, 0xEF}

// Build renders a DICOM P10 file in the given transfer syntax.
func Build(ts string, f Fields) []byte {
	return BuildWithOptions(ts, f, Options{})
}

// BuildWithOptions renders a P10 file exercising the requested variants.
func BuildWithOptions(ts string, f Fields, opt Options) []byte {
	var ord binary.ByteOrder = binary.LittleEndian
	explicit := ts != TSImplicitLE
	if ts == TSExplicitBE {
		ord = binary.BigEndian
	}

	var buf bytes.Buffer
	buf.Write(make([]byte, 128))
	buf.WriteString("DICM")

	// File meta group, always explicit VR LE.
	var meta bytes.Buffer
	wrExplicit(&meta, binary.LittleEndian, 0x0002, 0x0010, "UI", ts)
	wrExplicit(&meta, binary.LittleEndian, 0x0002, 0x0002, "UI", orDefault(f.SOPClassUID, "1.2.840.10008.5.1.4.1.1.2"))
	wrExplicit(&meta, binary.LittleEndian, 0x0002, 0x0003, "UI", f.SOPUID)
	if !opt.NoMetaGroupLength {
		gl := make([]byte, 4)
		binary.LittleEndian.PutUint32(gl, uint32(meta.Len()))
		wrExplicit(&buf, binary.LittleEndian, 0x0002, 0x0000, "UL", string(gl))
	}
	buf.Write(meta.Bytes())

	type el struct {
		g, e uint16
		vr   string
		v    string
	}
	els := []el{
		{0x0008, 0x0016, "UI", orDefault(f.SOPClassUID, "1.2.840.10008.5.1.4.1.1.2")},
		{0x0008, 0x0018, "UI", f.SOPUID},
		{0x0008, 0x0020, "DA", f.StudyDate},
		{0x0008, 0x0060, "CS", f.Modality},
		{0x0010, 0x0010, "PN", f.PatientName},
		{0x0010, 0x0020, "LO", f.PatientID},
		{0x0010, 0x0030, "DA", f.BirthDate},
		{0x0010, 0x0040, "CS", f.Sex},
		{0x0020, 0x000D, "UI", f.StudyUID},
		{0x0020, 0x000E, "UI", f.SeriesUID},
		{0x0020, 0x0010, "SH", f.StudyID},
		{0x0020, 0x0011, "IS", f.SeriesNumber},
		{0x0020, 0x0013, "IS", f.InstanceNo},
	}
	for _, x := range els {
		if explicit {
			wrExplicit(&buf, ord, x.g, x.e, x.vr, x.v)
		} else {
			wrImplicit(&buf, ord, x.g, x.e, x.v)
		}
	}

	if opt.UndefinedSQ {
		writeUndefinedSQ(&buf, ord, explicit, opt.RefSOPUID)
	}

	writePixelData(&buf, ord, explicit, opt)
	if opt.Extra != nil {
		buf.Write(opt.Extra)
	}
	return buf.Bytes()
}

// writeUndefinedSQ emits ReferencedSOPSequence (0008,1199) as SQ with
// undefined length, one undefined item containing a UID, delimiters.
func writeUndefinedSQ(buf *bytes.Buffer, ord binary.ByteOrder, explicit bool, refSOPUID string) {
	writeTag32(buf, ord, 0x0008, 0x1199, "SQ", explicit, true, 0)
	// item, undefined length
	writeItemHeader(buf, ord, 0xFFFFFFFF)
	if explicit {
		wrExplicit(buf, ord, 0x0008, 0x1155, "UI",
			orDefault(refSOPUID, "1.9.9.9.refsop"))
	} else {
		wrImplicit(buf, ord, 0x0008, 0x1155,
			orDefault(refSOPUID, "1.9.9.9.refsop"))
	}
	writeDelim(buf, ord, 0xFFFE, 0xE00D) // item delim
	writeDelim(buf, ord, 0xFFFE, 0xE0DD) // seq delim
}

func writePixelData(buf *bytes.Buffer, ord binary.ByteOrder, explicit bool, opt Options) {
	payload := opt.PixelPayload
	if payload == nil {
		payload = PixelBytes
	}
	if opt.EncapsulatedPixel {
		writeTag32(buf, ord, 0x7FE0, 0x0010, "OB", explicit, true, 0)
		// Basic offset table item: length 0 (no offsets).
		writeItemHeader(buf, ord, 0)
		// Fragment item with payload.
		writeItemHeader(buf, ord, uint32(len(payload)))
		buf.Write(payload)
		writeDelim(buf, ord, 0xFFFE, 0xE0DD)
		return
	}
	writeTag32(buf, ord, 0x7FE0, 0x0010, "OW", explicit, false, uint32(len(payload)))
	buf.Write(payload)
}

func writeTag32(buf *bytes.Buffer, ord binary.ByteOrder, g, e uint16, vr string, explicit, undefined bool, length uint32) {
	binary.Write(buf, ord, g)
	binary.Write(buf, ord, e)
	if !explicit {
		if undefined {
			length = 0xFFFFFFFF
		}
		binary.Write(buf, ord, length)
		return
	}
	buf.WriteString(vr)
	if vr == "SQ" || vr == "OB" || vr == "OW" || vr == "UN" {
		buf.WriteString("\x00\x00")
		if undefined {
			length = 0xFFFFFFFF
		}
		binary.Write(buf, ord, length)
	} else {
		binary.Write(buf, ord, uint16(length))
	}
}

func writeItemHeader(buf *bytes.Buffer, ord binary.ByteOrder, length uint32) {
	binary.Write(buf, ord, uint16(0xFFFE))
	binary.Write(buf, ord, uint16(0xE000))
	binary.Write(buf, ord, length)
}

func writeDelim(buf *bytes.Buffer, ord binary.ByteOrder, g, e uint16) {
	binary.Write(buf, ord, g)
	binary.Write(buf, ord, e)
	binary.Write(buf, ord, uint32(0))
}

// WriteFile writes a synthetic DICOM file to dir/name and returns its path.
func WriteFile(dir, name, ts string, f Fields) string {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		panic(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, Build(ts, f), 0o644); err != nil {
		panic(err)
	}
	return p
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func pad(v string) []byte {
	b := []byte(v)
	if len(b)%2 == 1 {
		b = append(b, 0)
	}
	return b
}

func wrExplicit(buf *bytes.Buffer, ord binary.ByteOrder, g, e uint16, vr, v string) {
	binary.Write(buf, ord, g)
	binary.Write(buf, ord, e)
	buf.WriteString(vr)
	b := pad(v)
	vrb := []byte{vr[0], vr[1]}
	if isLong(string(vrb)) {
		buf.WriteString("\x00\x00")
		binary.Write(buf, ord, uint32(len(b)))
	} else {
		binary.Write(buf, ord, uint16(len(b)))
	}
	buf.Write(b)
}

func isLong(vr string) bool {
	switch vr {
	case "OB", "OD", "OF", "OL", "OW", "SQ", "UC", "UR", "UT", "UN":
		return true
	}
	return false
}

func wrImplicit(buf *bytes.Buffer, ord binary.ByteOrder, g, e uint16, v string) {
	binary.Write(buf, ord, g)
	binary.Write(buf, ord, e)
	b := pad(v)
	binary.Write(buf, ord, uint32(len(b)))
	buf.Write(b)
}
