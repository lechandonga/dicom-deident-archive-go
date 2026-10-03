// Package dicom implements a minimal, dependency-free DICOM P10 parser and a
// streaming de-identification rewriter.
//
// The parser is deliberately lenient for uncommon but legal encodings while
// staying strict about structural integrity:
//
//   - Implicit VR LE, Explicit VR LE and Explicit VR BE datasets.
//   - Any transfer syntax UID: uncompressed datasets are indexed directly;
//     compressed/encapsulated pixel data (JPEG, JPEG-LS, JPEG 2000, RLE,
//     deflated, ...) is copied byte-for-byte without decoding.
//   - Undefined-length sequences/items and elements.
//   - A missing or wrong file-meta group length declaration.
//   - Unusual but well-formed VRs.
//
// Structural validation always covers the WHOLE file, regardless of where a
// truncation or corruption occurs. Metadata values are buffered only when
// small; pixel data and large opaque values are streamed, so files of
// hundreds of megabytes stay bounded in memory.
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
	ErrCorrupt             = errors.New("dicom: structurally corrupt element")
	ErrUnsupportedTransfer = errors.New("dicom: unsupported transfer syntax")
	ErrMissingTag          = errors.New("dicom: mandatory tag missing")
)

// Tag identifies a DICOM data element.
type Tag struct{ Group, Element uint16 }

var (
	TagFileMetaInfoGroupLength = Tag{0x0002, 0x0000}
	TagMediaStorageSOPClassUID = Tag{0x0002, 0x0002}
	TagMediaStorageSOPInstance = Tag{0x0002, 0x0003}
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
)

// Private marker tags injected by the de-identification rewriter. They live
// in a private block with creator "GSB-DEIDENT" so re-import can recognise
// already de-identified output and never shift/pseudonymise a second time.
const DeidentPrivateCreator = "GSB-DEIDENT"

var (
	tagPrivateCreator         = Tag{0x0099, 0x0010}
	TagDeidentFingerprint     = Tag{0x0099, 0x1010} // LO: fingerprint of the key
	TagDeidentOriginalSOPUID  = Tag{0x0099, 0x1011} // UI: original SOPInstanceUID
	tagDeidentMethod          = Tag{0x0099, 0x1012} // CS: method/version
	tagPatientIdentityRemoved = Tag{0x0012, 0x0062} // standard CS: YES
	tagDeidentificationMethod = Tag{0x0012, 0x0063} // standard LO
)

// Meta holds extracted patient/study/series/instance metadata.
type Meta struct {
	TransferSyntaxUID  string
	SOPClassUID        string
	SOPInstanceUID     string
	StudyDate          string // DICOM DA: YYYYMMDD
	Modality           string
	PatientName        string
	PatientID          string
	PatientBirthDate   string
	PatientSex         string
	StudyInstanceUID   string
	SeriesInstanceUID  string
	StudyID            string
	SeriesNumber       string
	InstanceNumber     string
	DeidentFingerprint string // private marker; empty for ordinary inputs
	DeidentOriginalUID string // original SOP UID recorded by the marker
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

// longVRs use a 2-byte reserved field followed by a 4-byte length.
var longVRs = map[string]bool{
	"OB": true, "OD": true, "OF": true, "OL": true, "OV": true, "OW": true,
	"SQ": true, "SV": true, "UC": true, "UR": true, "UT": true, "UN": true, "UV": true,
}

// maxBufferedValue bounds metadata values held in memory.
const maxBufferedValue = 16 << 20 // 16 MiB

const undefinedLength uint32 = 0xFFFFFFFF

var (
	itemTag      = Tag{0xFFFE, 0xE000}
	itemDelimTag = Tag{0xFFFE, 0xE00D}
	seqDelimTag  = Tag{0xFFFE, 0xE0DD}
)

// Parse reads a DICOM P10 stream, validates the WHOLE file structurally and
// returns the extracted metadata. Pixel data (including encapsulated
// JPEG/RLE payloads) is streamed through, never decoded or buffered.
func Parse(r io.Reader) (*Meta, error) {
	br := &reader{r: r}
	if err := br.preamble(); err != nil {
		return nil, err
	}
	metaVals, syn, err := readFileMeta(br)
	if err != nil {
		return nil, err
	}

	wanted := map[Tag]bool{
		TagSOPClassUID: true, TagSOPInstanceUID: true, TagStudyDate: true,
		TagModality: true, TagPatientName: true, TagPatientID: true,
		TagPatientBirthDate: true, TagPatientSex: true,
		TagStudyInstanceUID: true, TagSeriesInstanceUID: true,
		TagStudyID: true, TagSeriesNumber: true, TagInstanceNumber: true,
		TagDeidentFingerprint: true, TagDeidentOriginalSOPUID: true,
	}
	vals := map[Tag]string{}
	collect := &Transform{
		OnValue: func(depth int, e Element, val []byte) (bool, []byte, error) {
			if depth == 0 && wanted[e.Tag] {
				vals[e.Tag] = cleanValue(val)
			}
			return false, nil, nil
		},
	}
	if err := collect.run(br, syn, 0); err != nil {
		return nil, err
	}

	m := &Meta{
		TransferSyntaxUID:  metaVals[TagTransferSyntaxUID],
		SOPClassUID:        firstNonEmpty(vals[TagSOPClassUID], metaVals[TagMediaStorageSOPClassUID]),
		SOPInstanceUID:     firstNonEmpty(vals[TagSOPInstanceUID], metaVals[TagMediaStorageSOPInstance]),
		StudyDate:          vals[TagStudyDate],
		Modality:           vals[TagModality],
		PatientName:        vals[TagPatientName],
		PatientID:          vals[TagPatientID],
		PatientBirthDate:   vals[TagPatientBirthDate],
		PatientSex:         vals[TagPatientSex],
		StudyInstanceUID:   vals[TagStudyInstanceUID],
		SeriesInstanceUID:  vals[TagSeriesInstanceUID],
		StudyID:            vals[TagStudyID],
		SeriesNumber:       vals[TagSeriesNumber],
		InstanceNumber:     vals[TagInstanceNumber],
		DeidentFingerprint: vals[TagDeidentFingerprint],
		DeidentOriginalUID: vals[TagDeidentOriginalSOPUID],
	}
	for _, t := range mandatoryTags {
		var v string
		switch t {
		case TagSOPInstanceUID:
			v = m.SOPInstanceUID
		case TagStudyInstanceUID:
			v = m.StudyInstanceUID
		case TagSeriesInstanceUID:
			v = m.SeriesInstanceUID
		case TagPatientID:
			v = m.PatientID
		}
		if v == "" {
			return nil, &MissingTagError{Tag: t}
		}
	}
	return m, nil
}

type syntax struct {
	explicit bool
	big      bool
	order    binary.ByteOrder
}

func littleSyntax(explicit bool) syntax {
	return syntax{explicit: explicit, order: binary.LittleEndian}
}

// element describes one parsed data element header.
type element struct {
	tag       Tag
	vr        string // "" in implicit VR
	length    uint32
	undefined bool
	rawHeader []byte // exact source bytes of the header (passthrough mode)
}

func cleanValue(b []byte) string {
	s := strings.TrimRight(string(b), "\x00 ")
	return strings.TrimSpace(s)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// reader is a buffered byte reader with pushback support.
type reader struct {
	r      io.Reader
	unread []byte
}

func (br *reader) Read(p []byte) (int, error) {
	if len(br.unread) > 0 {
		n := copy(p, br.unread)
		br.unread = br.unread[n:]
		if n == len(p) {
			return n, nil
		}
		m, err := br.r.Read(p[n:])
		if m > 0 && err == io.EOF {
			err = nil
		}
		return n + m, err
	}
	return br.r.Read(p)
}

func (br *reader) readFull(p []byte) error {
	_, err := io.ReadFull(br, p)
	return err
}

// preamble reads and validates the 128-byte preamble + "DICM".
func (br *reader) preamble() error {
	var header [132]byte
	if err := br.readFull(header[:]); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return fmt.Errorf("%w: preamble", ErrTruncated)
		}
		return err
	}
	if string(header[128:132]) != "DICM" {
		return ErrNotDICOM
	}
	return nil
}
