// Package dicomtest builds synthetic DICOM P10 files for tests and sample
// data generation. It supports all three uncompressed transfer syntaxes.
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

// PixelBytes lets tests distinguish file content independently of metadata.
var PixelBytes = []byte{0xDE, 0xAD, 0xBE, 0xEF}

// Build renders a DICOM P10 file in the given transfer syntax.
func Build(ts string, f Fields) []byte {
	var buf bytes.Buffer
	buf.Write(make([]byte, 128))
	buf.WriteString("DICM")

	// File meta group, always explicit VR LE.
	var meta bytes.Buffer
	wrExplicit(&meta, binary.LittleEndian, 0x0002, 0x0010, "UI", ts)
	wrExplicit(&meta, binary.LittleEndian, 0x0002, 0x0002, "UI", orDefault(f.SOPClassUID, "1.2.840.10008.5.1.4.1.1.2"))
	wrExplicit(&meta, binary.LittleEndian, 0x0002, 0x0003, "UI", f.SOPUID)
	gl := make([]byte, 4)
	binary.LittleEndian.PutUint32(gl, uint32(meta.Len()))
	wrExplicit(&buf, binary.LittleEndian, 0x0002, 0x0000, "UL", string(gl))
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
	wr := func(g, e uint16, vr, v string) {
		switch ts {
		case TSImplicitLE:
			wrImplicit(&buf, binary.LittleEndian, g, e, v)
		case TSExplicitLE:
			wrExplicit(&buf, binary.LittleEndian, g, e, vr, v)
		case TSExplicitBE:
			wrExplicit(&buf, binary.BigEndian, g, e, vr, v)
		}
	}
	for _, e := range els {
		wr(e.g, e.e, e.vr, e.v)
	}
	// Pixel data marker so the parser stops here.
	switch ts {
	case TSExplicitBE:
		binary.Write(&buf, binary.BigEndian, uint16(0x7FE0))
		binary.Write(&buf, binary.BigEndian, uint16(0x0010))
	default:
		binary.Write(&buf, binary.LittleEndian, uint16(0x7FE0))
		binary.Write(&buf, binary.LittleEndian, uint16(0x0010))
	}
	buf.Write(PixelBytes)
	return buf.Bytes()
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
		b = append(b, ' ')
	}
	return b
}

func wrExplicit(buf *bytes.Buffer, ord binary.ByteOrder, g, e uint16, vr, v string) {
	binary.Write(buf, ord, g)
	binary.Write(buf, ord, e)
	buf.WriteString(vr)
	b := pad(v)
	binary.Write(buf, ord, uint16(len(b)))
	buf.Write(b)
}

func wrImplicit(buf *bytes.Buffer, ord binary.ByteOrder, g, e uint16, v string) {
	binary.Write(buf, ord, g)
	binary.Write(buf, ord, e)
	b := pad(v)
	binary.Write(buf, ord, uint32(len(b)))
	buf.Write(b)
}

var _ = fmt.Sprintf
