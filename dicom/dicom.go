// Package dicom implements a minimal, dependency-free DICOM P10 parser that
// extracts identity and hierarchy metadata needed by the archive.
//
// Supported transfer syntaxes:
//   - 1.2.840.10008.1.2   Implicit VR Little Endian
//   - 1.2.840.10008.1.2.1 Explicit VR Little Endian
//   - 1.2.840.10008.1.2.2 Explicit VR Big Endian
//
// Parsing is strict: truncated files, missing DICM prefix, unsupported
// transfer syntaxes and missing mandatory tags are reported with distinct,
// inspectable errors. Pixel data is never read or modified.
package dicom

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Transfer syntax UIDs.
const (
	TSImplicitVRLittleEndian = "1.2.840.10008.1.2"
	TSExplicitVRLittleEndian = "1.2.840.10008.1.2.1"
	TSExplicitVRBigEndian    = "1.2.840.10008.1.2.2"
)

// Distinct, classifiable rejection reasons.
var (
	ErrNotDICOM            = errors.New("dicom: missing 128-byte preamble or DICM prefix")
	ErrTruncated           = errors.New("dicom: file truncated or element length exceeds remaining data")
	ErrUnsupportedTransfer = errors.New("dicom: unsupported transfer syntax")
	ErrMissingTag          = errors.New("dicom: mandatory tag missing")
)

// Tag identifies a DICOM data element.
type Tag struct{ Group, Element uint16 }

var (
	TagTransferSyntaxUID = Tag{0x0002, 0x0010}
	TagSOPClassUID       = Tag{0x0008, 0x0016}
	TagSOPInstanceUID    = Tag{0x0008, 0x0018}
	TagStudyDate         = Tag{0x0008, 0x0020}
	TagModality          = Tag{0x0008, 0x0060}
	TagPatientName       = Tag{0x0010, 0x0010}
	TagPatientID         = Tag{0x0010, 0x0020}
	TagPatientBirthDate  = Tag{0x0010, 0x0030}
	TagPatientSex        = Tag{0x0010, 0x0040}
	TagStudyInstanceUID  = Tag{0x0020, 0x000D}
	TagSeriesInstanceUID = Tag{0x0020, 0x000E}
	TagStudyID           = Tag{0x0020, 0x0010}
	TagSeriesNumber      = Tag{0x0020, 0x0011}
	TagInstanceNumber    = Tag{0x0020, 0x0013}
	TagPixelData         = Tag{0x7FE0, 0x0010}
)

// Meta holds the extracted patient/study/series/instance metadata.
type Meta struct {
	TransferSyntaxUID string
	SOPClassUID       string
	SOPInstanceUID    string
	StudyDate         string // DICOM DA: YYYYMMDD
	Modality          string
	PatientName       string
	PatientID         string
	PatientBirthDate  string
	PatientSex        string
	StudyInstanceUID  string
	SeriesInstanceUID string
	StudyID           string
	SeriesNumber      string
	InstanceNumber    string
}

// mandatoryTags must be present for a file to be archivable.
var mandatoryTags = []Tag{
	TagSOPInstanceUID, TagStudyInstanceUID, TagSeriesInstanceUID, TagPatientID,
}

func tagName(t Tag) string {
	names := map[Tag]string{
		TagSOPInstanceUID:    "SOPInstanceUID(0008,0018)",
		TagStudyInstanceUID:  "StudyInstanceUID(0020,000D)",
		TagSeriesInstanceUID: "SeriesInstanceUID(0020,000E)",
		TagPatientID:         "PatientID(0010,0020)",
	}
	if n, ok := names[t]; ok {
		return n
	}
	return fmt.Sprintf("(%04X,%04X)", t.Group, t.Element)
}

// MissingTagError pinpoints which mandatory tag is absent.
type MissingTagError struct{ Tag Tag }

func (e *MissingTagError) Error() string {
	return fmt.Sprintf("%v: %s", ErrMissingTag, tagName(e.Tag))
}
func (e *MissingTagError) Unwrap() error { return ErrMissingTag }

// UnsupportedTransferError records the offending transfer syntax UID.
type UnsupportedTransferError struct{ UID string }

func (e *UnsupportedTransferError) Error() string {
	return fmt.Sprintf("%v: %q", ErrUnsupportedTransfer, e.UID)
}
func (e *UnsupportedTransferError) Unwrap() error { return ErrUnsupportedTransfer }

// longVRs use a 2-byte reserved field followed by a 4-byte length in explicit VR.
var longVRs = map[string]bool{"OB": true, "OD": true, "OF": true, "OL": true, "OW": true, "SQ": true, "UC": true, "UR": true, "UT": true, "UN": true}

type syntax struct {
	explicit bool
	order    binary.ByteOrder
}

// Parse reads a DICOM P10 stream and returns extracted metadata.
// It stops as soon as all wanted tags are seen or pixel data begins.
func Parse(r io.Reader) (*Meta, error) {
	header := make([]byte, 132)
	if _, err := io.ReadFull(r, header); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, fmt.Errorf("%w: preamble", ErrTruncated)
		}
		return nil, err
	}
	if string(header[128:132]) != "DICM" {
		return nil, ErrNotDICOM
	}

	// File meta group (0002) is always Explicit VR Little Endian. Its first
	// element (0002,0000) declares the byte length of the rest of the group,
	// which lets us bound the read without consuming dataset bytes.
	le := syntax{explicit: true, order: binary.LittleEndian}
	meta, err := readMetaGroup(r, le)
	if err != nil {
		return nil, err
	}
	tsUID := meta[TagTransferSyntaxUID]
	var syn syntax
	switch tsUID {
	case TSImplicitVRLittleEndian, "":
		if tsUID == "" {
			return nil, &MissingTagError{Tag: TagTransferSyntaxUID}
		}
		syn = syntax{explicit: false, order: binary.LittleEndian}
	case TSExplicitVRLittleEndian:
		syn = syntax{explicit: true, order: binary.LittleEndian}
	case TSExplicitVRBigEndian:
		syn = syntax{explicit: true, order: binary.BigEndian}
	default:
		return nil, &UnsupportedTransferError{UID: tsUID}
	}

	wanted := map[Tag]bool{
		TagSOPClassUID: true, TagSOPInstanceUID: true, TagStudyDate: true,
		TagModality: true, TagPatientName: true, TagPatientID: true,
		TagPatientBirthDate: true, TagPatientSex: true,
		TagStudyInstanceUID: true, TagSeriesInstanceUID: true,
		TagStudyID: true, TagSeriesNumber: true, TagInstanceNumber: true,
	}
	vals := map[Tag]string{}
	for {
		t, val, done, err := readElement(r, syn)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if done { // pixel data reached
			break
		}
		if wanted[t] {
			vals[t] = cleanValue(val)
			if len(vals) == len(wanted) {
				break
			}
		}
	}

	m := &Meta{
		TransferSyntaxUID: tsUID,
		SOPClassUID:       vals[TagSOPClassUID],
		SOPInstanceUID:    vals[TagSOPInstanceUID],
		StudyDate:         vals[TagStudyDate],
		Modality:          vals[TagModality],
		PatientName:       vals[TagPatientName],
		PatientID:         vals[TagPatientID],
		PatientBirthDate:  vals[TagPatientBirthDate],
		PatientSex:        vals[TagPatientSex],
		StudyInstanceUID:  vals[TagStudyInstanceUID],
		SeriesInstanceUID: vals[TagSeriesInstanceUID],
		StudyID:           vals[TagStudyID],
		SeriesNumber:      vals[TagSeriesNumber],
		InstanceNumber:    vals[TagInstanceNumber],
	}
	for _, t := range mandatoryTags {
		if vals[t] == "" {
			return nil, &MissingTagError{Tag: t}
		}
	}
	return m, nil
}

// readMetaGroup parses the file meta group using its declared group length.
func readMetaGroup(r io.Reader, syn syntax) (map[Tag]string, error) {
	// First element must be (0002,0000) UL with the group length.
	t, val, done, err := readElement(r, syn)
	if err != nil {
		if err == io.EOF {
			return nil, fmt.Errorf("%w: empty file meta group", ErrTruncated)
		}
		return nil, err
	}
	if done || t != (Tag{0x0002, 0x0000}) {
		return nil, fmt.Errorf("dicom: file meta group must start with (0002,0000), got %s", tagName(t))
	}
	if len(val) != 4 {
		return nil, fmt.Errorf("dicom: invalid meta group length field of %d bytes", len(val))
	}
	groupLen := binary.LittleEndian.Uint32(val)
	limited := io.LimitReader(r, int64(groupLen))
	out := map[Tag]string{}
	for {
		t, val, done, err := readElement(limited, syn)
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if done {
			return out, nil
		}
		out[t] = cleanValue(val)
	}
}

// readElement reads one data element. done=true means pixel data was hit and
// parsing should stop. Returns io.EOF at a clean stream end.
func readElement(r io.Reader, syn syntax) (Tag, []byte, bool, error) {
	var t Tag
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		if err == io.EOF {
			return t, nil, false, io.EOF
		}
		return t, nil, false, fmt.Errorf("%w: tag header", ErrTruncated)
	}
	t.Group = syn.order.Uint16(hdr[0:2])
	t.Element = syn.order.Uint16(hdr[2:4])
	if t == TagPixelData {
		return t, nil, true, nil
	}

	var length uint32
	var vr string
	if syn.explicit {
		var vrh [2]byte
		if _, err := io.ReadFull(r, vrh[:]); err != nil {
			return t, nil, false, fmt.Errorf("%w: VR of %s", ErrTruncated, tagName(t))
		}
		vr = string(vrh[:])
		if longVRs[vr] {
			var lb [6]byte // 2 reserved + 4 length
			if _, err := io.ReadFull(r, lb[:]); err != nil {
				return t, nil, false, fmt.Errorf("%w: length of %s", ErrTruncated, tagName(t))
			}
			length = syn.order.Uint32(lb[2:6])
		} else {
			var lb [2]byte
			if _, err := io.ReadFull(r, lb[:]); err != nil {
				return t, nil, false, fmt.Errorf("%w: length of %s", ErrTruncated, tagName(t))
			}
			length = uint32(syn.order.Uint16(lb[:]))
		}
	} else {
		var lb [4]byte
		if _, err := io.ReadFull(r, lb[:]); err != nil {
			return t, nil, false, fmt.Errorf("%w: length of %s", ErrTruncated, tagName(t))
		}
		length = syn.order.Uint32(lb[:])
	}
	if length == 0xFFFFFFFF { // undefined length (sequences); not needed to index
		return t, nil, false, fmt.Errorf("dicom: undefined-length element %s unsupported in header region", tagName(t))
	}
	if length > 1<<24 { // header fields are small; guard against corrupt lengths
		return t, nil, false, fmt.Errorf("%w: implausible length %d for %s", ErrTruncated, length, tagName(t))
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return t, nil, false, fmt.Errorf("%w: value of %s", ErrTruncated, tagName(t))
	}
	return t, buf, false, nil
}

func cleanValue(b []byte) string {
	s := strings.TrimRight(string(b), "\x00 ")
	return strings.TrimSpace(s)
}
