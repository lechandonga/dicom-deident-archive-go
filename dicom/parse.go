// Package dicom implements a minimal, dependency-free DICOM P10 parser,
// verifier and rewriter.
//
// Supported dataset encodings:
//   - 1.2.840.10008.1.2    Implicit VR Little Endian
//   - 1.2.840.10008.1.2.1  Explicit VR Little Endian
//   - 1.2.840.10008.1.2.2  Explicit VR Big Endian
//   - any encapsulated (compressed) Explicit VR LE syntax, e.g. JPEG,
//     JPEG-LS, JPEG 2000, RLE; pixel data is never decoded.
//
// Undefined-length sequences/items and a file meta group without an explicit
// group length are accepted. Parsing is streaming: pixel data is copied,
// never buffered whole. Truncation or structural corruption at any position
// is reported with a distinct, inspectable error.
package dicom

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Transfer syntax UIDs.
const (
	TSImplicitVRLittleEndian         = "1.2.840.10008.1.2"
	TSExplicitVRLittleEndian         = "1.2.840.10008.1.2.1"
	TSExplicitVRBigEndian            = "1.2.840.10008.1.2.2"
	TSDeflatedExplicitVRLittleEndian = "1.2.840.10008.1.2.1.99"
)

// Distinct, classifiable rejection reasons.
var (
	ErrNotDICOM            = errors.New("dicom: missing 128-byte preamble or DICM prefix")
	ErrTruncated           = errors.New("dicom: file truncated or element length exceeds remaining data")
	ErrCorrupt             = errors.New("dicom: structurally invalid element")
	ErrUnsupportedTransfer = errors.New("dicom: unsupported transfer syntax")
	ErrMissingTag          = errors.New("dicom: mandatory tag missing")
)

// Tag identifies a DICOM data element.
type Tag struct{ Group, Element uint16 }

func (t Tag) String() string { return fmt.Sprintf("(%04X,%04X)", t.Group, t.Element) }

var (
	TagFileMetaInfoGroupLength = Tag{0x0002, 0x0000}
	TagMediaStorageSOPUID      = Tag{0x0002, 0x0003}
	TagTransferSyntaxUID       = Tag{0x0002, 0x0010}
	TagSOPClassUID             = Tag{0x0008, 0x0016}
	TagSOPInstanceUID          = Tag{0x0008, 0x0018}
	TagStudyDate               = Tag{0x0008, 0x0020}
	TagModality                = Tag{0x0008, 0x0060}
	TagPatientName             = Tag{0x0010, 0x0010}
	TagPatientID               = Tag{0x0010, 0x0020}
	TagPatientBirthDate        = Tag{0x0010, 0x0030}
	TagPatientSex              = Tag{0x0010, 0x0040}
	TagStudyInstanceUID        = Tag{0x0020, 0x000D}
	TagSeriesInstanceUID       = Tag{0x0020, 0x000E}
	TagStudyID                 = Tag{0x0020, 0x0010}
	TagSeriesNumber            = Tag{0x0020, 0x0011}
	TagInstanceNumber          = Tag{0x0020, 0x0013}
	TagPixelData               = Tag{0x7FE0, 0x0010}

	tagItem          = Tag{0xFFFE, 0xE000}
	tagItemDelim     = Tag{0xFFFE, 0xE00D}
	tagSequenceDelim = Tag{0xFFFE, 0xE0DD}
)

// MetaElement is one raw file-meta-group element preserved for rewriting.
type MetaElement struct {
	Tag   Tag
	VR    string
	Value []byte
}

// Meta holds the extracted patient/study/series/instance metadata.
type Meta struct {
	TransferSyntaxUID string
	MetaElements      []MetaElement
	// Tags holds the cleaned value of every simple header element (the first
	// occurrence per tag), including private markers, for callers that need
	// tags beyond the named fields.
	Tags              map[Tag]string
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

// ExtensionValues returns the values of the given tags if present.
func (m *Meta) ExtensionValues(tags ...Tag) []string {
	var out []string
	for _, t := range tags {
		if v, ok := m.Tags[t]; ok {
			out = append(out, v)
		}
	}
	return out
}

// mandatoryTags must be present for a file to be archivable.
var mandatoryTags = []Tag{
	TagSOPInstanceUID, TagStudyInstanceUID, TagSeriesInstanceUID, TagPatientID,
}

// MissingTagError pinpoints which mandatory tag is absent.
type MissingTagError struct{ Tag Tag }

func (e *MissingTagError) Error() string { return fmt.Sprintf("%v: %s", ErrMissingTag, e.Tag) }
func (e *MissingTagError) Unwrap() error { return ErrMissingTag }

// UnsupportedTransferError records the offending transfer syntax UID.
type UnsupportedTransferError struct{ UID string }

func (e *UnsupportedTransferError) Error() string {
	return fmt.Sprintf("%v: %q", ErrUnsupportedTransfer, e.UID)
}
func (e *UnsupportedTransferError) Unwrap() error { return ErrUnsupportedTransfer }

// longVRs use a 2-byte reserved field followed by a 4-byte length.
var longVRs = map[string]bool{
	"OB": true, "OD": true, "OF": true, "OL": true, "OV": true, "OW": true,
	"SQ": true, "UC": true, "UR": true, "UT": true, "UN": true,
}

// knownVRs lists every DICOM value representation this parser recognises.
var knownVRs = map[string]bool{
	"AE": true, "AS": true, "AT": true, "CS": true, "DA": true, "DT": true,
	"DS": true, "FD": true, "FL": true, "IS": true, "LO": true, "LT": true,
	"OB": true, "OD": true, "OF": true, "OL": true, "OV": true, "OW": true,
	"PN": true, "SH": true, "SL": true, "SQ": true, "SS": true, "ST": true,
	"SV": true, "TM": true, "UC": true, "UI": true, "UL": true, "UN": true,
	"UR": true, "US": true, "UT": true, "UV": true,
}

type syntax struct {
	explicit bool
	order    binary.ByteOrder
}

// streamReader is an io.Reader with a prepend buffer, enough to push back a
// parsed element header (max 12 bytes) while structuring the read.
type streamReader struct {
	r   io.Reader
	buf []byte
}

func (s *streamReader) Read(p []byte) (int, error) {
	if len(s.buf) > 0 {
		n := copy(p, s.buf)
		if n == len(s.buf) {
			s.buf = nil
		} else {
			s.buf = s.buf[n:]
		}
		return n, nil
	}
	return s.r.Read(p)
}

func (s *streamReader) unread(p []byte) {
	s.buf = append(append([]byte{}, p...), s.buf...)
}

func readFull(r io.Reader, n int, what string) ([]byte, error) {
	buf := make([]byte, n)
	got := 0
	for got < n {
		m, err := r.Read(buf[got:])
		got += m
		if err != nil {
			if errors.Is(err, errLimitEnd) {
				return nil, errLimitEnd
			}
			if errors.Is(err, errItemEnd) {
				return nil, errItemEnd
			}
			if err == io.EOF {
				return nil, fmt.Errorf("%w: %s", ErrTruncated, what)
			}
			return nil, err
		}
	}
	return buf, nil
}

// elementHeader describes one parsed data element header.
type elementHeader struct {
	tag       Tag
	vr        string // empty in implicit VR
	length    uint32
	undefined bool
	hdr       []byte // raw header bytes, verbatim
}

func readHeader(r io.Reader, syn syntax, what string) (elementHeader, error) {
	var h elementHeader
	raw, err := readFull(r, 4, "tag of "+what)
	if err != nil {
		return h, err
	}
	h.tag = Tag{syn.order.Uint16(raw[0:2]), syn.order.Uint16(raw[2:4])}
	hdr := append([]byte{}, raw...)
	// Item / delimiter tags carry only a 4-byte length, never a VR.
	if h.tag.Group == 0xFFFE {
		lb, err := readFull(r, 4, "length of "+h.tag.String())
		if err != nil {
			return h, err
		}
		hdr = append(hdr, lb...)
		h.length = syn.order.Uint32(lb)
		h.undefined = h.length == 0xFFFFFFFF
		h.hdr = hdr
		return h, nil
	}

	if syn.explicit {
		vrb, err := readFull(r, 2, "VR of "+h.tag.String())
		if err != nil {
			return h, err
		}
		h.vr = string(vrb)
		hdr = append(hdr, vrb...)
		if !knownVRs[h.vr] {
			return h, fmt.Errorf("%w: unknown VR %q for %s", ErrCorrupt, h.vr, h.tag)
		}
		if longVRs[h.vr] {
			lb, err := readFull(r, 6, "length of "+h.tag.String())
			if err != nil {
				return h, err
			}
			hdr = append(hdr, lb...)
			h.length = syn.order.Uint32(lb[2:6])
		} else {
			lb, err := readFull(r, 2, "length of "+h.tag.String())
			if err != nil {
				return h, err
			}
			hdr = append(hdr, lb...)
			h.length = uint32(syn.order.Uint16(lb))
		}
	} else {
		lb, err := readFull(r, 4, "length of "+h.tag.String())
		if err != nil {
			return h, err
		}
		hdr = append(hdr, lb...)
		h.length = syn.order.Uint32(lb)
	}
	h.undefined = h.length == 0xFFFFFFFF
	h.hdr = hdr
	return h, nil
}

func syntaxFor(tsUID string) (syntax, error) {
	switch tsUID {
	case TSImplicitVRLittleEndian:
		return syntax{explicit: false, order: binary.LittleEndian}, nil
	case TSExplicitVRLittleEndian, TSExplicitVRBigEndian:
		if tsUID == TSExplicitVRBigEndian {
			return syntax{explicit: true, order: binary.BigEndian}, nil
		}
		return syntax{explicit: true, order: binary.LittleEndian}, nil
	case TSDeflatedExplicitVRLittleEndian:
		return syntax{}, &UnsupportedTransferError{UID: tsUID}
	case "":
		return syntax{}, &MissingTagError{Tag: TagTransferSyntaxUID}
	default:
		// Encapsulated compressed syntaxes are explicit VR little endian.
		// Pixel data stays untouched, so the exact codec is irrelevant.
		return syntax{explicit: true, order: binary.LittleEndian}, nil
	}
}
