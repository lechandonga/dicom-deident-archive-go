package dicom

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

var (
	errItemDelimHit = fmt.Errorf("dicom: item delimiter hit")
	errLimitEnd     = fmt.Errorf("dicom: declared length boundary reached")
)

// limitEndReader turns a consumed limit into errLimitEnd; genuine mid-read
// truncation still surfaces as ErrTruncated.
type limitEndReader struct{ lr *io.LimitedReader }

func (l limitEndReader) Read(p []byte) (int, error) {
	if l.lr.N <= 0 {
		return 0, errLimitEnd
	}
	n, err := l.lr.Read(p)
	if err == io.EOF {
		if l.lr.N <= 0 {
			// Boundary reached exactly; report both boundary and clean EOF
			// so callers waiting for a declared limit end terminate, while
			// a following zero-byte read signals a normal stream end.
			return n, boundaryEnd{}
		}
		// Underlying stream ended before the declared length was consumed.
		return n, fmt.Errorf("%w: %d more byte(s) expected", ErrTruncated, l.lr.N)
	}
	return n, err
}

// boundaryEnd answers errors.Is for both errLimitEnd (declared-length
// loops stop here) and io.EOF (a stream that ends exactly on a boundary is
// complete, not truncated).
type boundaryEnd struct{}

func (boundaryEnd) Error() string { return "dicom: declared length boundary reached" }
func (boundaryEnd) Is(target error) bool {
	return target == errLimitEnd || target == io.EOF
}

// Parse reads a DICOM P10 stream and extracts metadata. Only header-region
// elements are inspected; pixel data is never read. For a full integrity
// check use ParseVerify.
func Parse(r io.Reader) (*Meta, error) {
	m, _, err := parse(r, false)
	return m, err
}

// ParseVerify extracts metadata and validates the entire stream: defined
// lengths must fit, undefined-length sequences and encapsulated pixel data
// must be properly delimited, and the file ends exactly at the last element.
// Truncation or corruption anywhere (including inside pixel data) is
// rejected. The stream is consumed once, suitable for io.TeeReader hashing.
func ParseVerify(r io.Reader) (*Meta, error) {
	m, sr, err := parse(r, true)
	if err != nil {
		return nil, err
	}
	rest, err := io.ReadAll(io.LimitReader(sr, 2))
	if err != nil {
		return nil, err
	}
	// At most one byte may trail: the pad byte after an odd-length
	// non-pixel element at the very end of the dataset.
	if len(rest) > 1 {
		return nil, fmt.Errorf("%w: trailing bytes after last element", ErrCorrupt)
	}
	return m, nil
}

var tagDatasetPadding = Tag{0xFFFC, 0xFFFC}

func parse(r io.Reader, verify bool) (*Meta, *streamReader, error) {
	header, err := readFull(r, 132, "preamble")
	if err != nil {
		return nil, nil, err
	}
	if string(header[128:132]) != "DICM" {
		return nil, nil, ErrNotDICOM
	}

	sr := &streamReader{r: r}
	le := syntax{explicit: true, order: binary.LittleEndian}
	meta, err := readMetaGroup(sr, le)
	if err != nil {
		return nil, nil, err
	}
	var tsUID string
	for _, e := range meta {
		if e.Tag == TagTransferSyntaxUID {
			tsUID = cleanValue(e.Value)
		}
	}
	syn, err := syntaxFor(tsUID)
	if err != nil {
		return nil, nil, err
	}

	m := &Meta{TransferSyntaxUID: tsUID, MetaElements: meta, Tags: map[Tag]string{}}
	for _, e := range meta {
		switch e.Tag {
		case Tag{0x0002, 0x0002}:
			m.SOPClassUID = cleanValue(e.Value)
		case TagMediaStorageSOPUID:
			m.SOPInstanceUID = cleanValue(e.Value)
		}
	}

	vals := map[Tag]string{}
	if err := walkInto(sr, syn, verify, vals); err != nil && !errors.Is(err, errItemDelimHit) {
		return nil, nil, err
	}
	m.StudyDate = vals[TagStudyDate]
	m.Modality = vals[TagModality]
	m.PatientName = vals[TagPatientName]
	m.PatientID = vals[TagPatientID]
	m.PatientBirthDate = vals[TagPatientBirthDate]
	m.PatientSex = vals[TagPatientSex]
	if m.SOPInstanceUID == "" {
		m.SOPInstanceUID = vals[TagSOPInstanceUID]
	}
	if m.SOPClassUID == "" {
		m.SOPClassUID = vals[TagSOPClassUID]
	}
	m.StudyInstanceUID = vals[TagStudyInstanceUID]
	m.SeriesInstanceUID = vals[TagSeriesInstanceUID]
	m.StudyID = vals[TagStudyID]
	m.SeriesNumber = vals[TagSeriesNumber]
	m.InstanceNumber = vals[TagInstanceNumber]
	m.Tags = vals
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
		if strings.TrimSpace(v) == "" {
			return nil, nil, &MissingTagError{Tag: t}
		}
	}
	return m, sr, nil
}

// readMetaGroup reads file meta elements. With (0002,0000) present, the
// declared group length bounds reading; without it, elements are read until
// the first non-0002 element, which is pushed back unread.
func readMetaGroup(r *streamReader, syn syntax) ([]MetaElement, error) {
	h, err := readHeader(r, syn, "file meta")
	if err != nil {
		return nil, err
	}
	var declared int64 = -1
	if h.tag == TagFileMetaInfoGroupLength {
		val, err := readValue(r, h)
		if err != nil {
			return nil, err
		}
		if len(val) != 4 {
			return nil, fmt.Errorf("%w: invalid meta group length field of %d bytes", ErrCorrupt, len(val))
		}
		declared = int64(binary.LittleEndian.Uint32(val))
	} else {
		r.unread(h.hdr)
	}

	var out []MetaElement
	var in io.Reader = r
	var lim *io.LimitedReader
	if declared >= 0 {
		lim = &io.LimitedReader{R: r, N: declared}
		in = limitEndReader{lim}
	}
	for {
		h, err := readHeader(in, syn, "file meta element")
		if err != nil {
			if errors.Is(err, errCleanEOF) && declared >= 0 {
				return nil, fmt.Errorf("%w: file meta group shorter than declared length", ErrTruncated)
			}
			if errors.Is(err, errLimitEnd) {
				return out, nil
			}
			if declared < 0 && err == io.EOF {
				return out, nil
			}
			return nil, err
		}
		if declared < 0 && h.tag.Group != 0x0002 {
			r.unread(h.hdr)
			return out, nil
		}
		val, err := readValue(in, h)
		if err != nil {
			return nil, err
		}
		if h.length%2 != 0 && h.tag != TagPixelData {
			return nil, fmt.Errorf("%w: odd length %d for file meta element %s", ErrCorrupt, h.length, h.tag)
		}
		out = append(out, MetaElement{Tag: h.tag, VR: h.vr, Value: val})
	}
}

// readValue reads the value bytes of a simple (non-sequence) element.
func readValue(r io.Reader, h elementHeader) ([]byte, error) {
	if h.undefined {
		return nil, fmt.Errorf("%w: undefined length for non-sequence %s", ErrCorrupt, h.tag)
	}
	buf, err := readFull(r, int(h.length), "value of "+h.tag.String())
	if errors.Is(err, errCleanEOF) {
		return nil, fmt.Errorf("%w: value of %s", ErrTruncated, h.tag)
	}
	if errors.Is(err, errLimitEnd) {
		return nil, fmt.Errorf("%w: value of %s crosses declared length", ErrTruncated, h.tag)
	}
	return buf, err
}

// walkInto walks dataset elements, recursing into sequences. Header-region
// simple values are collected into vals. verify=false stops after pixel
// data; verify=true validates and drains everything. An Item/Sequence
// Delimiter at the top of the walk returns errItemDelimHit.
func walkInto(r io.Reader, syn syntax, verify bool, vals map[Tag]string) error {
	for {
		h, err := readHeader(r, syn, "element")
		if err != nil {
			if err == io.EOF || errors.Is(err, errLimitEnd) || errors.Is(err, errCleanEOF) {
				return nil
			}
			return err
		}
		switch h.tag {
		case tagItemDelim, tagSequenceDelim:
			return errItemDelimHit
		}
		if h.tag == TagPixelData {
			if err := drainPixelData(r, syn, h, verify); err != nil {
				return err
			}
			if !verify {
				return nil
			}
			// Pixel data must be the last dataset element apart from
			// (FFFC,FFFC) Data Set Trailing Padding. Drain any padding
			// elements structurally; anything else or a truncated tail is
			// rejected below.
			for {
				ph, perr := readHeader(r, syn, "element after pixel data")
				if perr == io.EOF || errors.Is(perr, errCleanEOF) || errors.Is(perr, errLimitEnd) {
					return nil
				}
				if perr != nil {
					return perr
				}
				if ph.tag != tagDatasetPadding {
					return fmt.Errorf("%w: element %s after Pixel Data", ErrCorrupt, ph.tag)
				}
				if ph.undefined {
					return fmt.Errorf("%w: undefined length for Data Set Trailing Padding", ErrCorrupt)
				}
				if err := copyN(io.Discard, r, int64(ph.length)); err != nil {
					return err
				}
			}
		}
		pb := &streamReader{r: r}
		isSeq, err := isSequence(pb, syn, h)
		if err != nil {
			return err
		}
		if isSeq {
			if err := drainSequence(pb, syn, h, verify, vals); err != nil {
				return err
			}
			continue
		}
		val, err := readValue(pb, h)
		if err != nil {
			return err
		}
		if verify && h.length%2 != 0 {
			return fmt.Errorf("%w: odd length %d for element %s", ErrCorrupt, h.length, h.tag)
		}
		if _, ok := vals[h.tag]; !ok {
			vals[h.tag] = cleanValue(val)
		}
	}
}

// isSequence classifies an element as SQ. Explicit VR trusts the VR (UN +
// undefined length counts). Implicit VR uses undefined length or an Item-tag
// prefix.
func isSequence(r *streamReader, syn syntax, h elementHeader) (bool, error) {
	if syn.explicit {
		return h.vr == "SQ" || (h.vr == "UN" && h.undefined), nil
	}
	if h.undefined {
		return true, nil
	}
	if h.length == 0 {
		return false, nil
	}
	n := int(h.length)
	if n > 8 {
		n = 8
	}
	peek := make([]byte, n)
	if _, err := io.ReadFull(r, peek); err != nil {
		if err == io.EOF {
			return false, fmt.Errorf("%w: value of %s", ErrTruncated, h.tag)
		}
		return false, err
	}
	r.unread(peek)
	if n < 4 {
		return false, nil
	}
	return Tag{syn.order.Uint16(peek[0:2]), syn.order.Uint16(peek[2:4])} == tagItem, nil
}

// drainSequence validates and drains a sequence, recursing into items.
func drainSequence(r io.Reader, syn syntax, h elementHeader, verify bool, vals map[Tag]string) error {
	if h.undefined {
		for {
			ih, err := readHeader(r, syn, "sequence item")
			if err != nil {
				if err == io.EOF || errors.Is(err, errCleanEOF) {
					return fmt.Errorf("%w: sequence %s missing Sequence Delimiter", ErrTruncated, h.tag)
				}
				return err
			}
			switch ih.tag {
			case tagSequenceDelim:
				return nil
			case tagItem:
				if err := drainItem(r, syn, ih, verify, vals); err != nil {
					return err
				}
			default:
				return fmt.Errorf("%w: expected Item, got %s", ErrCorrupt, ih.tag)
			}
		}
	}
	// Defined-length sequence: bounded region containing items exactly.
	lr := limitEndReader{&io.LimitedReader{R: r, N: int64(h.length)}}
	for {
		ih, err := readHeader(lr, syn, "sequence item")
		if err != nil {
			if errors.Is(err, errLimitEnd) {
				return nil
			}
			return err
		}
		if ih.tag != tagItem {
			return fmt.Errorf("%w: expected Item, got %s", ErrCorrupt, ih.tag)
		}
		if err := drainItem(lr, syn, ih, verify, vals); err != nil {
			return err
		}
	}
}

// drainItem drains one sequence item.
func drainItem(r io.Reader, syn syntax, ih elementHeader, verify bool, vals map[Tag]string) error {
	if ih.undefined {
		err := walkInto(r, syn, verify, vals)
		if errors.Is(err, errItemDelimHit) {
			return nil
		}
		if err == nil || errors.Is(err, errCleanEOF) || err == io.EOF {
			return fmt.Errorf("%w: item missing Item Delimiter", ErrTruncated)
		}
		return err
	}
	if ih.length%2 != 0 {
		return fmt.Errorf("%w: odd item length %d", ErrCorrupt, ih.length)
	}
	lr := limitEndReader{&io.LimitedReader{R: r, N: int64(ih.length)}}
	err := walkInto(lr, syn, verify, vals)
	if errors.Is(err, errItemDelimHit) {
		// Delimiters are only legal inside undefined-length items.
		return fmt.Errorf("%w: unexpected Item/Sequence Delimiter inside defined-length item", ErrCorrupt)
	}
	if err != nil {
		return err
	}
	return nil
}

// drainPixelData validates pixel data without buffering it. When verify is
// false the value is skipped without reading the payload.
func drainPixelData(r io.Reader, syn syntax, h elementHeader, verify bool) error {
	if !h.undefined {
		if !verify {
			return nil
		}
		if err := copyN(io.Discard, r, int64(h.length)); err != nil {
			return err
		}
		if h.length%2 != 0 {
			// Pixel Data may carry exactly one trailing pad byte.
			var pad [1]byte
			if _, err := io.ReadFull(r, pad[:]); err != nil {
				return fmt.Errorf("%w: pixel data missing trailing pad byte", ErrTruncated)
			}
		}
		return nil
	}
	// Encapsulated: optional BOT fragment, data fragments, Sequence Delim.
	if !verify {
		return nil
	}
	for {
		ih, err := readHeader(r, syn, "pixel fragment")
		if err != nil {
			if err == io.EOF || errors.Is(err, errCleanEOF) {
				return fmt.Errorf("%w: pixel data missing Sequence Delimiter", ErrTruncated)
			}
			return err
		}
		switch ih.tag {
		case tagSequenceDelim:
			return nil
		case tagItem:
			if ih.undefined {
				return fmt.Errorf("%w: undefined-length pixel fragment", ErrCorrupt)
			}
			if ih.length%2 != 0 {
				return fmt.Errorf("%w: odd pixel fragment length %d", ErrCorrupt, ih.length)
			}
			if err := copyN(io.Discard, r, int64(ih.length)); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: expected pixel fragment Item, got %s", ErrCorrupt, ih.tag)
		}
	}
}

// copyN drains exactly n bytes; a short read (including io.EOF before the
// count is met) is truncation, never silently accepted.
func copyN(w io.Writer, r io.Reader, n int64) error {
	written, err := io.CopyN(w, r, n)
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return fmt.Errorf("%w: %d of %d payload bytes missing", ErrTruncated, n-written, n)
	}
	if err != nil {
		return fmt.Errorf("%w: %v", ErrTruncated, err)
	}
	return nil
}

func cleanValue(b []byte) string {
	s := strings.TrimRight(string(b), "\x00 ")
	return strings.TrimSpace(s)
}
