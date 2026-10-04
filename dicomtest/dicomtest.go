// Package dicomtest builds synthetic DICOM P10 files for tests and sample
// generation. It supports the three uncompressed syntaxes, encapsulated
// (compressed) pixel payloads, undefined-length sequences and file meta
// groups without an explicit group length.
package dicomtest

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
)

const (
	TSImplicitLE = "1.2.840.10008.1.2"
	TSExplicitLE = "1.2.840.10008.1.2.1"
	TSExplicitBE = "1.2.840.10008.1.2.2"

	// TSJPEGLossless and TSRLE are representative encapsulated syntaxes.
	// Their pixel data is written as opaque fragments, never decoded.
	TSJPEGLossless = "1.2.840.10008.1.2.4.70"
	TSRLE          = "1.2.840.10008.1.2.5"
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
	FrameRefUID  string // optional (0020,0052)
	Rows         uint16
	Columns      uint16
}

// Options controls variant encodings for edge-case samples.
type Options struct {
	// Pixel is the pixel payload; nil uses PixelBytes. For encapsulated
	// transfer syntaxes it becomes one data fragment; BOT is empty.
	Pixel []byte
	// Encapsulated forces Item-wrapped pixel data even with an uncompressed
	// transfer syntax (rare but legal writers do this).
	Encapsulated bool
	// UndefLengthSequence includes a nested undefined-length SQ containing
	// identity UIDs, exercising parser/rewriter recursion.
	UndefLengthSequence bool
	// DefinedLengthSequence includes a defined-length SQ.
	DefinedLengthSequence bool
	// NoMetaGroupLength omits (0002,0000) entirely.
	NoMetaGroupLength bool
}

// PixelBytes lets tests distinguish file content independently of metadata.
var PixelBytes = []byte{0xDE, 0xAD, 0xBE, 0xEF}

// Build renders a DICOM P10 file in the given transfer syntax.
func Build(ts string, f Fields) []byte {
	return BuildOptions(ts, f, Options{})
}

// BuildOptions renders a file with the requested variant encodings.
func BuildOptions(ts string, f Fields, opt Options) []byte {
	pixel := opt.Pixel
	if pixel == nil {
		pixel = PixelBytes
	}
	encap := opt.Encapsulated || isEncapsulatedTS(ts)

	var buf bytes.Buffer
	buf.Write(make([]byte, 128))
	buf.WriteString("DICM")

	// File meta group, always explicit VR LE.
	var meta bytes.Buffer
	// File Meta Information Version (0002,0001), OB, exactly 2 bytes.
	wrExplicitBin(&meta, binary.LittleEndian, 0x0002, 0x0001, "OB", []byte{0, 1})
	wrExplicit(&meta, binary.LittleEndian, 0x0002, 0x0002, "UI", orDefault(f.SOPClassUID, "1.2.840.10008.5.1.4.1.1.2"))
	wrExplicit(&meta, binary.LittleEndian, 0x0002, 0x0003, "UI", f.SOPUID)
	wrExplicit(&meta, binary.LittleEndian, 0x0002, 0x0010, "UI", ts)
	wrExplicit(&meta, binary.LittleEndian, 0x0002, 0x0012, "UI", "1.2.826.0.1.3680043.9.7.0")
	wrExplicit(&meta, binary.LittleEndian, 0x0002, 0x0013, "SH", "DICOM-ARCHIVE-SAMPLE")
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
		{0x0008, 0x0008, "CS", "ORIGINAL\\PRIMARY"},
		{0x0008, 0x0016, "UI", orDefault(f.SOPClassUID, "1.2.840.10008.5.1.4.1.1.2")},
		{0x0008, 0x0018, "UI", f.SOPUID},
		{0x0008, 0x0020, "DA", f.StudyDate},
		{0x0008, 0x0030, "TM", "120000"},
		{0x0008, 0x0060, "CS", f.Modality},
		{0x0008, 0x0070, "LO", "DICOM-ARCHIVE-SAMPLES"},
		{0x0008, 0x103E, "LO", "Synthetic sample"},
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
	if f.FrameRefUID != "" {
		els = append(els, el{0x0020, 0x0052, "UI", f.FrameRefUID})
	}
	rows, cols := f.Rows, f.Columns
	if rows == 0 {
		rows = 2
	}
	if cols == 0 {
		cols = 2
	}
	els = append(els,
		el{0x0028, 0x0002, "US", u16(1)}, // SamplesPerPixel
		el{0x0028, 0x0004, "CS", "MONOCHROME2"},
		el{0x0028, 0x0008, "IS", "1"}, // Frames
		el{0x0028, 0x0010, "US", u16(rows)},
		el{0x0028, 0x0011, "US", u16(cols)},
		el{0x0028, 0x0030, "DS", "1.0\\1.0"},
		el{0x0028, 0x0100, "US", u16(8)}, // BitsAllocated
		el{0x0028, 0x0101, "US", u16(8)}, // BitsStored
		el{0x0028, 0x0102, "US", u16(7)}, // HighBit
		el{0x0028, 0x0103, "US", u16(0)}, // PixelRepresentation
	)
	var ord binary.ByteOrder = binary.LittleEndian
	if ts == TSExplicitBE {
		ord = binary.BigEndian
	}
	for _, e := range els {
		switch {
		case ts == TSImplicitLE:
			if isBinaryVR(e.vr) {
				wrImplicitBin(&buf, ord, e.g, e.e, e.vr, usBytes(e.v, ord))
			} else {
				wrImplicit(&buf, binary.LittleEndian, e.g, e.e, e.v)
			}
		default:
			if isBinaryVR(e.vr) {
				wrExplicitBin(&buf, ord, e.g, e.e, e.vr, usBytes(e.v, ord))
			} else {
				wrExplicit(&buf, ord, e.g, e.e, e.vr, e.v)
			}
		}
	}
	if opt.UndefLengthSequence {
		writeRefSequence(&buf, ts, ord, true, f)
	}
	if opt.DefinedLengthSequence {
		writeRefSequence(&buf, ts, ord, false, f)
	}
	writePixelData(&buf, ts, ord, pixel, encap)
	return buf.Bytes()
}

func isEncapsulatedTS(ts string) bool {
	return ts == TSJPEGLossless || ts == TSRLE ||
		(len(ts) >= 19 && ts[:19] == "1.2.840.10008.1.2.4") || ts == "1.2.840.10008.1.2.5"
}

// writeRefSequence writes ReferencedStudySequence (0008,1110) with one item
// containing the study UID, letting tests verify UID rewriting inside SQs.
func writeRefSequence(buf *bytes.Buffer, ts string, ord binary.ByteOrder, undef bool, f Fields) {
	var item bytes.Buffer
	if ts == TSImplicitLE {
		wrImplicit(&item, binary.LittleEndian, 0x0008, 0x1155, f.StudyUID)
	} else {
		wrExplicit(&item, ord, 0x0008, 0x1155, "UI", f.StudyUID)
	}
	if undef {
		if ts == TSImplicitLE {
			writeTagU32(buf, binary.LittleEndian, 0x0008, 0x1110, 0xFFFFFFFF)
		} else {
			writeSQUndef(buf, ord, 0x0008, 0x1110)
		}
		writeItemUndef(buf, ord)
		buf.Write(item.Bytes())
		writeItemDelim(buf, ord)
		writeSeqDelim(buf, ord)
		return
	}
	if ts == TSImplicitLE {
		writeTagU32(buf, binary.LittleEndian, 0x0008, 0x1110, uint32(8+item.Len()))
	} else {
		writeSQDefined(buf, ord, 0x0008, 0x1110, uint32(8+item.Len()))
	}
	writeItemDefined(buf, ord, item.Bytes())
}

func writePixelData(buf *bytes.Buffer, ts string, ord binary.ByteOrder, pixel []byte, encap bool) {
	padded := pixel
	if len(padded)%2 == 1 {
		padded = append(append([]byte{}, padded...), 0)
	}
	switch {
	case encap:
		// Explicit VR only in practice: OB + undefined length.
		writeTagU16(buf, ord, 0x7FE0, 0x0010)
		buf.WriteString("OB")
		buf.Write([]byte{0, 0})
		writeU32(buf, ord, 0xFFFFFFFF)
		// Empty BOT fragment.
		writeItemFragment(buf, ord, nil)
		writeItemFragment(buf, ord, padded)
		writeSeqDelim(buf, ord)
	case ts == TSImplicitLE:
		writeTagU32(buf, binary.LittleEndian, 0x7FE0, 0x0010, uint32(len(pixel)))
		buf.Write(padded)
	default:
		// OW is a long VR: tag + "OW" + 2 reserved bytes + 32-bit length.
		// Writing the 16-bit short form here corrupts every uncompressed
		// explicit-VR sample whose payload exceeds ~64 KiB (strict parsers
		// read the first four payload bytes as the declared length).
		writeTagU16(buf, ord, 0x7FE0, 0x0010)
		buf.WriteString("OW")
		buf.Write([]byte{0, 0})
		writeU32(buf, ord, uint32(len(padded)))
		buf.Write(padded)
	}
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

// WriteFileOptions is the Options-aware variant of WriteFile.
func WriteFileOptions(dir, name, ts string, f Fields, opt Options) string {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		panic(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, BuildOptions(ts, f, opt), 0o644); err != nil {
		panic(err)
	}
	return p
}

// VariantSpec describes one gensample -variants edge-case file.
type VariantSpec struct {
	Name string
	TS   string
	Opt  Options
	SOP  string
}

// VariantFields are the deterministic metadata shared by -variants outputs.
func VariantFields(sop string) Fields {
	return Fields{
		PatientID: "VAR-1", PatientName: "Variant^Case", BirthDate: "19901231",
		Sex: "F", StudyUID: "9.9.1", StudyDate: "20240505", StudyID: "V",
		SeriesUID: "9.9.2", SeriesNumber: "3", Modality: "CT",
		SOPUID: sop, InstanceNo: "1",
	}
}

// VariantSpecs returns the five edge-case variants emitted by
// `gensample -variants`; pixel is the deterministic 300 KiB compressed
// payload shared by the encapsulated variants.
func VariantSpecs(pixel []byte) []VariantSpec {
	return []VariantSpec{
		{"jpeg-lossless.dcm", TSJPEGLossless, Options{Pixel: pixel}, "9.9.3.1"},
		{"rle.dcm", TSRLE, Options{Pixel: pixel}, "9.9.3.2"},
		{"undef-sequence.dcm", TSExplicitLE, Options{UndefLengthSequence: true}, "9.9.3.3"},
		{"defined-sequence.dcm", TSExplicitLE, Options{DefinedLengthSequence: true}, "9.9.3.4"},
		{"no-meta-group-length.dcm", TSExplicitLE, Options{NoMetaGroupLength: true}, "9.9.3.5"},
	}
}

// DeterministicPixel returns the 300 KiB deterministic fill used by the
// encapsulated gensample variants.
func DeterministicPixel() []byte {
	pixel := make([]byte, 300*1024)
	for i := range pixel {
		pixel[i] = byte(i * 7)
	}
	return pixel
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
		b = append(b, ' ')
	}
	return b
}

func wrExplicit(buf *bytes.Buffer, ord binary.ByteOrder, g, e uint16, vr, v string) {
	writeTagU16(buf, ord, g, e)
	buf.WriteString(vr)
	b := pad(v)
	writeU16(buf, ord, uint16(len(b)))
	buf.Write(b)
}

// wrExplicitBin writes a binary VR element (US/OB/OW...). US uses the short
// header form; OB/OW/UN and friends use the reserved+32-bit long form.
func wrExplicitBin(buf *bytes.Buffer, ord binary.ByteOrder, g, e uint16, vr string, b []byte) {
	writeTagU16(buf, ord, g, e)
	buf.WriteString(vr)
	switch vr {
	case "US", "SS", "AT":
		writeU16(buf, ord, uint16(len(b)))
	default:
		buf.Write([]byte{0, 0})
		writeU32(buf, ord, uint32(len(b)))
	}
	buf.Write(b)
}

func wrImplicitBin(buf *bytes.Buffer, ord binary.ByteOrder, g, e uint16, vr string, b []byte) {
	writeTagU32(buf, ord, g, e, uint32(len(b)))
	buf.Write(b)
}

func isBinaryVR(vr string) bool {
	switch vr {
	case "US", "SS", "AT", "OB", "OW", "OD", "OF", "OL", "OV", "UN":
		return true
	}
	return false
}

// usBytes decodes the decimal string placed in a US-valued el and encodes
// it as binary (2 bytes, target byte order).
func usBytes(s string, ord binary.ByteOrder) []byte {
	v := uint16(0)
	for _, c := range s {
		if c >= '0' && c <= '9' {
			v = v*10 + uint16(c-'0')
		}
	}
	b := make([]byte, 2)
	ord.PutUint16(b, v)
	return b
}

// u16 renders n for the string-typed element table used by usBytes.
func u16(n uint16) string { return fmt.Sprintf("%d", n) }

func wrImplicit(buf *bytes.Buffer, ord binary.ByteOrder, g, e uint16, v string) {
	writeTagU32(buf, ord, g, e, uint32(len(pad(v))))
	buf.Write(pad(v))
}

func writeTagU16(buf *bytes.Buffer, ord binary.ByteOrder, g, e uint16) {
	var b [4]byte
	ord.PutUint16(b[0:2], g)
	ord.PutUint16(b[2:4], e)
	buf.Write(b[:])
}

func writeTagU32(buf *bytes.Buffer, ord binary.ByteOrder, g, e uint16, n uint32) {
	writeTagU16(buf, ord, g, e)
	writeU32(buf, ord, n)
}

func writeU16(buf *bytes.Buffer, ord binary.ByteOrder, v uint16) {
	var b [2]byte
	ord.PutUint16(b[:], v)
	buf.Write(b[:])
}

func writeU32(buf *bytes.Buffer, ord binary.ByteOrder, v uint32) {
	var b [4]byte
	ord.PutUint32(b[:], v)
	buf.Write(b[:])
}

func writeSQUndef(buf *bytes.Buffer, ord binary.ByteOrder, g, e uint16) {
	writeTagU16(buf, ord, g, e)
	buf.WriteString("SQ")
	buf.Write([]byte{0, 0})
	writeU32(buf, ord, 0xFFFFFFFF)
}

func writeSQDefined(buf *bytes.Buffer, ord binary.ByteOrder, g, e uint16, n uint32) {
	writeTagU16(buf, ord, g, e)
	buf.WriteString("SQ")
	buf.Write([]byte{0, 0})
	writeU32(buf, ord, n)
}

func writeItemUndef(buf *bytes.Buffer, ord binary.ByteOrder) {
	writeTagU32(buf, ord, 0xFFFE, 0xE000, 0xFFFFFFFF)
}

func writeItemDefined(buf *bytes.Buffer, ord binary.ByteOrder, content []byte) {
	writeTagU32(buf, ord, 0xFFFE, 0xE000, uint32(len(content)))
	buf.Write(content)
}

func writeItemFragment(buf *bytes.Buffer, ord binary.ByteOrder, frag []byte) {
	writeTagU32(buf, ord, 0xFFFE, 0xE000, uint32(len(frag)))
	buf.Write(frag)
}

func writeItemDelim(buf *bytes.Buffer, ord binary.ByteOrder) {
	writeTagU32(buf, ord, 0xFFFE, 0xE00D, 0)
}

func writeSeqDelim(buf *bytes.Buffer, ord binary.ByteOrder) {
	writeTagU32(buf, ord, 0xFFFE, 0xE0DD, 0)
}
