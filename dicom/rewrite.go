package dicom

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"sort"
)

// DateTags covers every standard DA element that can carry a patient-
// relative date. All are shifted by the same per-patient offset, including
// dates nested inside sequences (e.g. acquisition/verification dates).
var DateTags = map[Tag]bool{
	{0x0008, 0x0020}: true, // StudyDate
	{0x0008, 0x0021}: true, // SeriesDate
	{0x0008, 0x0022}: true, // AcquisitionDate
	{0x0008, 0x0023}: true, // ContentDate
	{0x0008, 0x0024}: true, // OverlayDate
	{0x0008, 0x0025}: true, // CurveDate
	{0x0010, 0x0030}: true, // PatientBirthDate
	{0x0018, 0x1012}: true, // DateOfSecondaryCapture
	{0x0018, 0x1200}: true, // DateOfLastCalibration
	{0x0040, 0x2004}: true, // IssueDateOfImagingServiceRequest
	{0x0040, 0xA121}: true, // Date (procedure)
}

// UIDTags are UID elements that may reference patient/study/series/instance
// hierarchy and must be rewritten consistently.
var UIDTags = map[Tag]bool{
	TagSOPInstanceUID:          true,
	TagStudyInstanceUID:        true,
	TagSeriesInstanceUID:       true,
	{0x0008, 0x1155}:           true, // Referenced SOP Instance UID
	{0x0020, 0x0052}:           true, // Frame of Reference UID
	{0x0020, 0x0200}:           true, // Synchronization Frame of Reference UID
	{0x3006, 0x00C2}:           true, // Related Frame of Reference UID
	TagMediaStorageSOPInstance: true,
	TagDeidentOriginalSOPUID:   true,
}

// Identity describes the original and pseudo identifiers for one instance.
type Identity struct {
	OrigPatientID   string
	OrigPatientName string
	PseudoPatientID string
	PseudoName      string
	UIDs            map[string]string // original UID -> pseudo UID (any level)
}

// Plan is the per-instance de-identification plan.
type Plan struct {
	Ident   Identity
	ShiftDA func(da string) string // already bound to this patient
	// Fingerprint and OriginalSOPUID are embedded as private markers.
	Fingerprint    string
	OriginalSOPUID string
	Method         string
}

// RewriteStream copies a valid P10 stream from r to w with identity-bearing
// metadata rewritten per plan. Everything else — including compressed or
// encapsulated pixel data — is copied byte-for-byte. The whole stream is
// validated; a structural error aborts the output.
func RewriteStream(r io.Reader, w io.Writer, plan Plan) error {
	br := &reader{r: r}
	if err := br.preamble(); err != nil {
		return err
	}
	if _, err := w.Write(make([]byte, 128)); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "DICM"); err != nil {
		return err
	}

	metaVals, syn, err := readFileMeta(br)
	if err != nil {
		return err
	}
	if err := writeFileMeta(w, metaVals, plan); err != nil {
		return err
	}

	inject := buildInjection(syn, plan)
	tr := &Transform{
		W:      w,
		Inject: inject,
		OnValue: func(depth int, e Element, val []byte) (bool, []byte, error) {
			rb, rv := rewriteValue(e, val, plan)
			return rb, rv, nil
		},
	}
	return tr.run(br, syn, 0)
}

// rewriteValue maps identity-bearing values. Any value that exactly equals
// a known original UID is replaced by its pseudo; PN/LO identity tags and DA
// tags are rewritten tag-wise.
func rewriteValue(e Element, val []byte, plan Plan) (bool, []byte) {
	t := e.Tag
	s := cleanValue(val)

	switch {
	// Drop every pre-existing marker: injection re-adds a fresh copy with
	// exactly the right values (this also prevents the generic UID mapping
	// below from remapping the provenance UID on re-export).
	case t == tagPrivateCreator || t == TagDeidentFingerprint ||
		t == TagDeidentOriginalSOPUID || t == tagDeidentMethod ||
		t == tagPatientIdentityRemoved || t == tagDeidentificationMethod:
		return true, nil
	case t == TagPatientID:
		return true, []byte(plan.Ident.PseudoPatientID)
	case t == TagPatientName:
		return true, []byte(plan.Ident.PseudoName)
	case DateTags[t]:
		return true, []byte(plan.ShiftDA(s))
	case UIDTags[t]:
		if p, ok := plan.Ident.UIDs[s]; ok {
			return true, []byte(p)
		}
		return false, nil
	default:
		// Any UI value matching a known original UID is rewritten.
		if e.VR == "UI" || (e.VR == "" && vrForTag(t) == "UI") {
			if p, ok := plan.Ident.UIDs[s]; ok {
				return true, []byte(p)
			}
		}
		return false, nil
	}
}

// writeFileMeta re-emits group 0002 in explicit VR LE with a recomputed
// group length, rewriting MediaStorageSOPInstanceUID via the UID map.
func writeFileMeta(w io.Writer, vals map[Tag]string, plan Plan) error {
	var body bytes.Buffer
	le := binary.LittleEndian
	keys := make([]Tag, 0, len(vals))
	for t := range vals {
		keys = append(keys, t)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Group != keys[j].Group {
			return keys[i].Group < keys[j].Group
		}
		return keys[i].Element < keys[j].Element
	})
	for _, t := range keys {
		v := vals[t]
		if t == TagFileMetaInfoGroupLength {
			continue
		}
		out := v
		if t == TagMediaStorageSOPInstance {
			if p, ok := plan.Ident.UIDs[v]; ok {
				out = p
			}
		}
		vr := "UI"
		if x := vrForTag(t); x != "" {
			vr = x
		}
		if err := writeElement(&body, littleSyntax(true), Element{Tag: t, VR: vr}, []byte(out)); err != nil {
			return err
		}
	}

	// (0002,0000) UL group length (UL is a short VR: tag + "UL" + u16 len
	// + 4-byte value).
	var gl [12]byte
	le.PutUint16(gl[0:2], 0x0002)
	le.PutUint16(gl[2:4], 0x0000)
	gl[4] = 'U'
	gl[5] = 'L'
	le.PutUint16(gl[6:8], 4)
	le.PutUint32(gl[8:12], uint32(body.Len()))
	if _, err := w.Write(gl[:]); err != nil {
		return err
	}
	_, err := w.Write(body.Bytes())
	return err
}

// buildInjection renders the de-identification marker elements in dataset
// syntax. They are inserted at the top of the dataset and are themselves
// free of original values.
func buildInjection(syn syntax, plan Plan) []byte {
	var buf bytes.Buffer
	put := func(t Tag, vr string, v string) {
		if err := writeElement(&buf, syn, Element{Tag: t, VR: vr}, []byte(v)); err != nil {
			panic(err)
		}
	}
	put(tagPrivateCreator, "LO", DeidentPrivateCreator)
	put(TagDeidentFingerprint, "LO", plan.Fingerprint)
	put(TagDeidentOriginalSOPUID, "UI", plan.OriginalSOPUID)
	put(tagDeidentMethod, "CS", plan.Method)
	put(tagPatientIdentityRemoved, "CS", "YES")
	put(tagDeidentificationMethod, "LO", plan.Method)
	return buf.Bytes()
}

// TextValue is one extracted textual metadata value with its location tag.
type TextValue struct {
	Tag   Tag
	Depth int
	Value string
}

// ScanMetadata walks the whole stream and returns every small textual
// metadata value up to pixel data and beyond (sequences included). Used for
// the post-export residual-identity self-check.
func ScanMetadata(r io.Reader) ([]TextValue, error) {
	br := &reader{r: r}
	if err := br.preamble(); err != nil {
		return nil, err
	}
	metaVals, syn, err := readFileMeta(br)
	if err != nil {
		return nil, err
	}
	var out []TextValue
	for t, v := range metaVals {
		out = append(out, TextValue{Tag: t, Depth: -1, Value: v})
	}
	tr := &Transform{
		OnValue: func(depth int, e Element, val []byte) (bool, []byte, error) {
			out = append(out, TextValue{Tag: e.Tag, Depth: depth, Value: cleanValue(val)})
			return false, nil, nil
		},
	}
	if err := tr.run(br, syn, 0); err != nil {
		return nil, err
	}
	return out, nil
}

var _ = fmt.Sprintf
