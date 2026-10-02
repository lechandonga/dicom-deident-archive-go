package dicom

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"time"
)

// Stable rejection reasons. Callers and tests branch on these strings.
const (
	ReasonNotDICOM          = "NOT_DICOM_PREAMBLE"
	ReasonTruncated         = "TRUNCATED_FILE"
	ReasonBadEncoding       = "BAD_VR_ENCODING"
	ReasonUnsupportedSyntax = "UNSUPPORTED_TRANSFER_SYNTAX"
	ReasonMissingMeta       = "MISSING_FILE_META"
	ReasonMissingPatientID  = "MISSING_PATIENT_ID"
	ReasonMissingStudyUID   = "MISSING_STUDY_INSTANCE_UID"
	ReasonMissingSeriesUID  = "MISSING_SERIES_INSTANCE_UID"
	ReasonMissingSOPUID     = "MISSING_SOP_INSTANCE_UID"
	ReasonDuplicateInstance = "DUPLICATE_INSTANCE_CONFLICT"
	ReasonDuplicateImport   = "DUPLICATE_IMPORT_IDEMPOTENT"
	ReasonInvalidDate       = "INVALID_DATE_VALUE"
)

// RejectError carries a stable reason, the offending file/tag and details.
type RejectError struct {
	Reason string
	File   string
	Tag    string
	Detail string
}

func (e *RejectError) Error() string {
	loc := e.File
	if e.Tag != "" {
		loc += " " + e.Tag
	}
	if loc == "" {
		loc = "?"
	}
	return fmt.Sprintf("%s at %s: %s", e.Reason, loc, e.Detail)
}

func reject(reason, file, tag, detail string) *RejectError {
	return &RejectError{Reason: reason, File: file, Tag: tag, Detail: detail}
}

func validVR(v VR) bool {
	switch v {
	case VRAE, VRAS, VRAT, VRCS, VRDA, VRDS, VRDT, VRFD, VRFL, VRIS,
		VRLO, VRLT, VRPN, VRSH, VRSL, VRSQ, VRSS, VRST, VRTM, VRUI,
		VRUL, VRUS, VRUT, VROB, VROW, VROF, VRUN:
		return true
	}
	return false
}

// Transfer syntax UIDs this implementation understands.
const (
	TSExplicitLE       = "1.2.840.10008.1.2.1"
	TSImplicitLE       = "1.2.840.10008.1.2"
	TSExplicitBE       = "1.2.840.10008.1.2.2"
	TSDeflatedExplicit = "1.2.840.10008.1.2.1.99"
)

// Dataset is a parsed DICOM instance.
type Dataset struct {
	TransferSyntax string
	FileMeta       []*Element
	Elements       []*Element
}

// Find returns the first element with the tag, or nil.
func (d *Dataset) Find(t Tag) *Element {
	for _, e := range d.Elements {
		if e.Tag == t {
			return e
		}
	}
	return nil
}

var requiredDatasetTags = []struct {
	tag    Tag
	reason string
}{
	{TagPatientID, ReasonMissingPatientID},
	{TagStudyInstanceUID, ReasonMissingStudyUID},
	{TagSeriesInstanceUID, ReasonMissingSeriesUID},
	{TagSOPInstanceUID, ReasonMissingSOPUID},
}

// validate rejects files missing key identifiers or carrying syntactically
// invalid DA values. It also checks the file-meta SOP instance UID, when
// present, matches the dataset one.
func (d *Dataset) validate(file, metaSOPUID string) error {
	for _, r := range requiredDatasetTags {
		el := d.Find(r.tag)
		if el == nil || strings.TrimSpace(el.First()) == "" {
			return reject(r.reason, file, r.tag.String(),
				"required identifying attribute is absent or empty")
		}
	}
	if metaSOPUID != "" {
		if sop := normalizeUID(d.Find(TagSOPInstanceUID).First()); sop != metaSOPUID {
			return reject(ReasonBadEncoding, file, TagSOPInstanceUID.String(),
				fmt.Sprintf("file meta SOP instance UID %q disagrees with dataset UID %q",
					metaSOPUID, sop))
		}
	}
	for _, t := range append(append([]Tag{}, dateTags...), birthDateTag) {
		el := d.Find(t)
		if el == nil {
			continue
		}
		for _, v := range el.Values {
			if v == "" {
				continue
			}
			if _, err := time.Parse("20060102", v); err != nil {
				return reject(ReasonInvalidDate, file, t.String(),
					fmt.Sprintf("date %q is not YYYYMMDD: %v", v, err))
			}
		}
	}
	return nil
}

// Parse reads and parses a whole DICOM file. Pixel data is retained verbatim.
func Parse(r io.Reader, file string) (*Dataset, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, reject(ReasonTruncated, file, "", err.Error())
	}
	return ParseBytes(raw, file)
}

// ParseBytes is Parse for an in-memory image.
func ParseBytes(raw []byte, file string) (*Dataset, error) {
	if len(raw) < 132 {
		return nil, reject(ReasonTruncated, file, "",
			fmt.Sprintf("file has %d bytes, need at least 132", len(raw)))
	}
	if string(raw[128:132]) != "DICM" {
		return nil, reject(ReasonNotDICOM, file, "",
			"missing 'DICM' magic at offset 128")
	}

	fp := &parser{buf: raw, off: 132, bo: binary.LittleEndian, mode: explicit, file: file}
	meta, err := fp.parseFileMeta()
	if err != nil {
		return nil, err
	}

	ts := ""
	metaSOPUID := ""
	for _, e := range meta {
		switch e.Tag {
		case TagTransferSyntaxUID:
			ts = normalizeUID(e.First())
		case TagMediaStorageSOPInstanceUID:
			metaSOPUID = normalizeUID(e.First())
		}
	}
	if ts == "" {
		return nil, reject(ReasonMissingMeta, file, TagTransferSyntaxUID.String(),
			"transfer syntax UID not present in file meta information")
	}

	body := raw[fp.off:]
	mode := explicit
	var bo binary.ByteOrder = binary.LittleEndian
	switch ts {
	case TSExplicitLE:
	case TSImplicitLE:
		mode = implicit
	case TSExplicitBE:
		bo = binary.BigEndian
	case TSDeflatedExplicit:
		zr, zerr := zlib.NewReader(bytes.NewReader(body))
		if zerr != nil {
			return nil, reject(ReasonTruncated, file, "",
				"cannot open deflated dataset: "+zerr.Error())
		}
		inflated, ierr := io.ReadAll(zr)
		_ = zr.Close()
		if ierr != nil {
			return nil, reject(ReasonTruncated, file, "",
				"deflated dataset is corrupt: "+ierr.Error())
		}
		body = inflated
	default:
		return nil, reject(ReasonUnsupportedSyntax, file, TagTransferSyntaxUID.String(),
			"transfer syntax "+ts)
	}

	bp := &parser{buf: body, bo: bo, mode: mode, file: file}
	elems, err := bp.parseDataset(0x0004)
	if err != nil {
		return nil, err
	}

	ds := &Dataset{TransferSyntax: ts, FileMeta: meta, Elements: elems}
	if err := ds.validate(file, metaSOPUID); err != nil {
		return nil, err
	}
	return ds, nil
}

type syntaxMode int

const (
	explicit syntaxMode = iota
	implicit
)

type parser struct {
	buf  []byte
	off  int
	bo   binary.ByteOrder
	mode syntaxMode
	file string
}

const undefinedLength uint32 = 0xFFFFFFFF

// parseFileMeta parses group 0002; it stops at the first non-0002 element
// (which belongs to the dataset body and is left unconsumed).
func (p *parser) parseFileMeta() ([]*Element, error) {
	var meta []*Element
	for p.off+8 <= len(p.buf) {
		group := binary.LittleEndian.Uint16(p.buf[p.off:])
		if group != 0x0002 {
			return meta, nil
		}
		el, err := p.parseOneElement(explicit)
		if err != nil {
			return nil, err
		}
		meta = append(meta, el)
	}
	return nil, reject(ReasonTruncated, p.file, "", "file meta truncated")
}

// parseDataset parses elements until EOF, a sequence delimiter or an item
// delimiter. minGroup guards against file-meta elements leaking into body.
func (p *parser) parseDataset(minGroup uint16) ([]*Element, error) {
	var out []*Element
	for p.off < len(p.buf) {
		if p.off+8 > len(p.buf) {
			return nil, reject(ReasonTruncated, p.file, "",
				fmt.Sprintf("element header truncated at offset %d", p.off))
		}
		group := p.bo.Uint16(p.buf[p.off:])
		element := p.bo.Uint16(p.buf[p.off+2:])
		tag := Tag{group, element}

		if group == 0xFFFE {
			switch element {
			case 0xE00D, 0xE0DD:
				p.off += 8
				return out, nil
			case 0xE000:
				return nil, reject(ReasonBadEncoding, p.file, tag.String(),
					"unexpected item tag outside sequence")
			}
		}
		if group < minGroup && group != 0x0000 {
			return nil, reject(ReasonBadEncoding, p.file, tag.String(),
				fmt.Sprintf("element of group %04X appears after file meta", group))
		}

		el, err := p.parseOneElement(p.mode)
		if err != nil {
			return nil, err
		}
		out = append(out, el)
	}
	return out, nil
}

func (p *parser) parseOneElement(mode syntaxMode) (*Element, error) {
	tag := Tag{p.bo.Uint16(p.buf[p.off:]), p.bo.Uint16(p.buf[p.off+2:])}
	p.off += 4

	var vr VR
	var vl uint32

	if mode == explicit {
		if p.off+2 > len(p.buf) {
			return nil, reject(ReasonTruncated, p.file, tag.String(), "missing VR")
		}
		vr = VR(p.buf[p.off : p.off+2])
		p.off += 2
		if longHeaderVRs[vr] {
			if p.off+6 > len(p.buf) {
				return nil, reject(ReasonTruncated, p.file, tag.String(), "short extended header")
			}
			if p.buf[p.off] != 0 || p.buf[p.off+1] != 0 {
				return nil, reject(ReasonBadEncoding, p.file, tag.String(),
					fmt.Sprintf("reserved bytes after VR %s are 0x%02X%02X",
						vr, p.buf[p.off], p.buf[p.off+1]))
			}
			p.off += 2
			vl = p.bo.Uint32(p.buf[p.off:])
			p.off += 4
		} else {
			if !validVR(vr) {
				return nil, reject(ReasonBadEncoding, p.file, tag.String(),
					"unknown explicit VR "+string(vr))
			}
			if p.off+2 > len(p.buf) {
				return nil, reject(ReasonTruncated, p.file, tag.String(), "missing value length")
			}
			vl = uint32(p.bo.Uint16(p.buf[p.off:]))
			p.off += 2
		}
	} else {
		if p.off+4 > len(p.buf) {
			return nil, reject(ReasonTruncated, p.file, tag.String(), "missing implicit length")
		}
		vl = p.bo.Uint32(p.buf[p.off:])
		p.off += 4
		vr = implicitVR(tag)
	}

	el := &Element{Tag: tag, VR: vr}
	switch {
	case vr == VRSQ:
		if err := p.parseSequence(el, vl); err != nil {
			return nil, err
		}
	case tag == TagPixelData && vl == undefinedLength:
		if err := p.parseEncapsulated(el); err != nil {
			return nil, err
		}
	case vl == undefinedLength:
		if vr != VRUN {
			return nil, reject(ReasonBadEncoding, p.file, tag.String(),
				"undefined length on non-sequence VR "+string(vr))
		}
		raw, err := p.readUndefinedRaw()
		if err != nil {
			return nil, err
		}
		el.Raw = raw
	default:
		if uint64(p.off)+uint64(vl) > uint64(len(p.buf)) {
			return nil, reject(ReasonTruncated, p.file, tag.String(),
				fmt.Sprintf("value length %d exceeds remaining %d bytes",
					vl, len(p.buf)-p.off))
		}
		data := p.buf[p.off : p.off+int(vl)]
		p.off += int(vl)
		p.decodeValue(el, data)
	}
	return el, nil
}

func (p *parser) decodeValue(el *Element, data []byte) {
	if !el.IsText() {
		el.Raw = append([]byte(nil), data...)
		return
	}
	s := strings.TrimRight(string(data), "\x00")
	if !strings.Contains(s, "\\") {
		el.Values = []string{strings.TrimSpace(s)}
		return
	}
	parts := strings.Split(s, "\\")
	for i, v := range parts {
		parts[i] = strings.TrimSpace(strings.TrimRight(v, "\x00"))
	}
	el.Values = parts
}

// parseSequence parses an SQ value of explicit or undefined length.
func (p *parser) parseSequence(el *Element, vl uint32) error {
	el.UndefinedLength = vl == undefinedLength
	if vl != undefinedLength {
		end := p.off + int(vl)
		if end > len(p.buf) {
			return reject(ReasonTruncated, p.file, el.Tag.String(), "sequence runs past EOF")
		}
		sub := &parser{buf: p.buf[:end], off: p.off, bo: p.bo, mode: p.mode, file: p.file}
		items, err := sub.parseItems(false)
		if err != nil {
			return err
		}
		el.Items = items
		p.off = end
		return nil
	}
	items, err := p.parseItems(true)
	if err != nil {
		return err
	}
	el.Items = items
	return nil
}

// parseItems parses sequence items. When untilDelim is false the caller has
// established a fixed-length window (p.buf ends at the sequence end); when
// true, parsing stops at (FFFE,E00D) sequence delimitation.
func (p *parser) parseItems(untilDelim bool) ([]Item, error) {
	var items []Item
	for p.off+8 <= len(p.buf) {
		group := p.bo.Uint16(p.buf[p.off:])
		elem := p.bo.Uint16(p.buf[p.off+2:])
		ilen := p.bo.Uint32(p.buf[p.off+4:])

		if group == 0xFFFE && elem == 0xE00D {
			p.off += 8
			return items, nil
		}
		if group != 0xFFFE || elem != 0xE000 {
			return nil, reject(ReasonBadEncoding, p.file,
				fmt.Sprintf("(%04X,%04X)", group, elem), "expected sequence item tag")
		}
		p.off += 8

		if ilen != undefinedLength {
			iend := p.off + int(ilen)
			if iend > len(p.buf) {
				return nil, reject(ReasonTruncated, p.file, "", "item runs past its window")
			}
			sub := &parser{buf: p.buf[:iend], off: p.off, bo: p.bo, mode: p.mode, file: p.file}
			els, err := sub.parseDataset(0x0000)
			if err != nil {
				return nil, err
			}
			items = append(items, Item{Elements: els})
			p.off = iend
			continue
		}

		// Undefined-length item: parse until the item delimitation item,
		// which parseDataset consumes.
		sub := &parser{buf: p.buf, off: p.off, bo: p.bo, mode: p.mode, file: p.file}
		els, err := sub.parseDataset(0x0000)
		if err != nil {
			return nil, err
		}
		items = append(items, Item{Elements: els})
		p.off = sub.off
	}
	if untilDelim {
		return nil, reject(ReasonTruncated, p.file, "", "sequence missing delimitation item")
	}
	return items, nil
}

// parseEncapsulated parses encapsulated pixel data: basic offset table item
// followed by one or more fragment items, terminated by sequence delimitation.
func (p *parser) parseEncapsulated(el *Element) error {
	for p.off+8 <= len(p.buf) {
		group := p.bo.Uint16(p.buf[p.off:])
		elem := p.bo.Uint16(p.buf[p.off+2:])
		flen := p.bo.Uint32(p.buf[p.off+4:])
		if group == 0xFFFE && elem == 0xE00D {
			p.off += 8
			return nil
		}
		if group != 0xFFFE || elem != 0xE000 {
			return reject(ReasonBadEncoding, p.file, TagPixelData.String(),
				fmt.Sprintf("expected encapsulated fragment item, got (%04X,%04X)", group, elem))
		}
		p.off += 8
		if flen == undefinedLength {
			return reject(ReasonBadEncoding, p.file, TagPixelData.String(),
				"fragment item must have explicit length")
		}
		end := p.off + int(flen)
		if end > len(p.buf) {
			return reject(ReasonTruncated, p.file, TagPixelData.String(),
				"fragment runs past EOF")
		}
		// The first item is the basic offset table; keep it as a fragment
		// so the written stream is byte-equivalent.
		frag := append([]byte(nil), p.buf[p.off:end]...)
		el.Fragments = append(el.Fragments, frag)
		p.off = end
	}
	return reject(ReasonTruncated, p.file, TagPixelData.String(),
		"encapsulated pixel data missing sequence delimitation")
}

// readUndefinedRaw handles VR=UN with undefined length: concatenation of item
// payloads (PS3.5 Annex A.5).
func (p *parser) readUndefinedRaw() ([]byte, error) {
	var raw []byte
	for p.off+8 <= len(p.buf) {
		group := p.bo.Uint16(p.buf[p.off:])
		elem := p.bo.Uint16(p.buf[p.off+2:])
		ilen := p.bo.Uint32(p.buf[p.off+4:])
		if group == 0xFFFE && elem == 0xE00D {
			p.off += 8
			return raw, nil
		}
		if group != 0xFFFE || elem != 0xE000 {
			return nil, reject(ReasonBadEncoding, p.file, "",
				"expected UN item tag")
		}
		p.off += 8
		if ilen == undefinedLength {
			return nil, reject(ReasonBadEncoding, p.file, "", "UN item must have explicit length")
		}
		end := p.off + int(ilen)
		if end > len(p.buf) {
			return nil, reject(ReasonTruncated, p.file, "", "UN item runs past EOF")
		}
		raw = append(raw, p.buf[p.off:end]...)
		p.off = end
	}
	return nil, reject(ReasonTruncated, p.file, "", "UN value missing delimitation")
}
