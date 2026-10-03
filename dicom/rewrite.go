package dicom

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// RewriteSpec controls de-identified rewriting of a DICOM P10 stream.
type RewriteSpec struct {
	// MapUID maps an original (or already pseudonymised) UID value to its
	// output value for hierarchy/instance identity UIDs.
	MapUID func(kind, orig string) string
	// MapPatientID / MapPatientName transform the matching simple elements.
	MapPatientID   func(orig string) string
	MapPatientName func(orig string) string
	// ShiftDate shifts any DA element (study date, birth date, ...) using one
	// fixed per-patient offset, preserving order and intervals.
	ShiftDate func(origDA string) string
	// Markers are inserted right before pixel data (or at dataset end).
	Markers []Marker
	// AuditSecrets lists values that must never survive inside textual
	// header elements (original ID/name/UIDs/dates). Any residual occurrence
	// is reported with its element tag.
	AuditSecrets []string
}

// Marker is one inserted provenance element.
type Marker struct {
	Tag   Tag
	VR    string
	Value string
}

// AuditFinding names an element where an original value survived.
type AuditFinding struct {
	Tag   Tag
	Value string
}

// RewriteResult reports what the rewriter observed.
type RewriteResult struct {
	AuditFindings []AuditFinding
}

// Rewrite streams a fully validated DICOM file from r to w with identity
// metadata transformed per spec. Dataset encoding (VR, byte order, transfer
// syntax) and every pixel-data byte are preserved; only identity simple
// elements change, sequences are re-emitted canonically (undefined length),
// and the file meta group is rebuilt. The stream is consumed once.
func Rewrite(r io.Reader, w io.Writer, spec RewriteSpec) (*RewriteResult, error) {
	hdr, err := readFull(r, 132, "preamble")
	if err != nil {
		return nil, err
	}
	if string(hdr[128:132]) != "DICM" {
		return nil, ErrNotDICOM
	}
	if _, err := w.Write(hdr); err != nil {
		return nil, err
	}

	sr := &streamReader{r: r}
	le := syntax{explicit: true, order: binary.LittleEndian}
	meta, err := readMetaGroup(sr, le)
	if err != nil {
		return nil, err
	}
	var tsUID string
	for _, e := range meta {
		if e.Tag == TagTransferSyntaxUID {
			tsUID = cleanValue(e.Value)
		}
	}
	syn, err := syntaxFor(tsUID)
	if err != nil {
		return nil, err
	}

	// Rebuild file meta group: recompute group length, map the media-storage
	// SOP instance UID, keep transfer syntax and other meta elements verbatim.
	var body bytes.Buffer
	for _, e := range meta {
		if e.Tag == TagFileMetaInfoGroupLength {
			continue
		}
		val := e.Value
		if e.Tag == TagMediaStorageSOPUID {
			val = padUID(spec.MapUID("sop", cleanValue(val)))
		}
		if err := writeSimpleElement(&body, le, e.Tag, e.VR, val); err != nil {
			return nil, err
		}
	}
	group := body.Bytes()
	if err := writeSimpleElement(w, le, TagFileMetaInfoGroupLength, "UL", uint32Bytes(uint32(len(group)))); err != nil {
		return nil, err
	}
	if _, err := w.Write(group); err != nil {
		return nil, err
	}

	rw := &rewriter{spec: spec, findings: []AuditFinding{}}
	if err := rw.rewriteDataset(sr, syn, w); err != nil {
		return nil, err
	}
	var one [1]byte
	if _, err := io.ReadFull(sr, one[:]); err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, fmt.Errorf("%w: trailing bytes after last element", ErrCorrupt)
	}
	return &RewriteResult{AuditFindings: rw.findings}, nil
}

type rewriter struct {
	spec     RewriteSpec
	findings []AuditFinding
	markers  bool
}

func (rw *rewriter) writeMarkers(w io.Writer, syn syntax) error {
	for _, mk := range rw.spec.Markers {
		if err := writeSimpleElement(w, syn, mk.Tag, mk.VR, padString(mk.VR, mk.Value)); err != nil {
			return err
		}
	}
	return nil
}

// rewriteDataset streams dataset elements: identity simple elements are
// transformed, sequences recursed, then pixel data and everything after it
// is copied byte-for-byte.
func (rw *rewriter) rewriteDataset(r *streamReader, syn syntax, w io.Writer) error {
	for {
		h, err := readHeader(r, syn, "element")
		if err != nil {
			if err == io.EOF || errors.Is(err, errLimitEnd) || errors.Is(err, errItemEnd) {
				if !rw.markers {
					if err := rw.writeMarkers(w, syn); err != nil {
						return err
					}
				}
				return nil
			}
			return err
		}
		if h.tag == TagPixelData {
			if !rw.markers {
				if err := rw.writeMarkers(w, syn); err != nil {
					return err
				}
				rw.markers = true
			}
			return copyPixelAndTrailer(r, syn, h, w)
		}
		isSeq, err := isSequence(r, syn, h)
		if err != nil {
			return err
		}
		if isSeq {
			if err := rw.rewriteSequence(r, syn, h, w); err != nil {
				return err
			}
			continue
		}
		val, err := readValue(r, h)
		if err != nil {
			return err
		}
		if outVal, vr, handled := rw.transformSimple(syn, h, val); handled {
			if err := writeSimpleElement(w, syn, h.tag, vr, outVal); err != nil {
				return err
			}
		} else {
			if _, err := w.Write(h.hdr); err != nil {
				return err
			}
			if _, err := w.Write(val); err != nil {
				return err
			}
		}
	}
}

// transformSimple returns the replacement for a simple element. handled=false
// means the element must be copied verbatim.
func (rw *rewriter) transformSimple(syn syntax, h elementHeader, val []byte) (out []byte, vr string, handled bool) {
	vr = h.vr
	if !syn.explicit {
		vr = implicitVR(h.tag)
	}
	switch h.tag {
	case TagPatientID:
		out := rw.spec.MapPatientID(cleanValue(val))
		rw.auditText(h.tag, cleanValue(val), out)
		return padString(vr, out), vr, true
	case TagPatientName:
		out := rw.spec.MapPatientName(cleanValue(val))
		rw.auditText(h.tag, cleanValue(val), out)
		return padString(vr, out), vr, true
	case TagStudyDate, TagPatientBirthDate:
		orig := cleanValue(val)
		out := rw.spec.ShiftDate(orig)
		rw.auditText(h.tag, orig, out)
		return padString(vr, out), vr, true
	case TagSOPInstanceUID, TagStudyInstanceUID, TagSeriesInstanceUID:
		kind := "sop"
		switch h.tag {
		case TagStudyInstanceUID:
			kind = "study"
		case TagSeriesInstanceUID:
			kind = "series"
		}
		orig := cleanValue(val)
		out := rw.spec.MapUID(kind, orig)
		rw.auditText(h.tag, orig, out)
		return padUID(out), "UI", true
	}
	if kind := uidKindForTag(h.tag, vr); kind != "" {
		orig := cleanValue(val)
		out := rw.spec.MapUID(kind, orig)
		rw.auditText(h.tag, orig, out)
		return padUID(out), "UI", true
	}
	if vr == "DA" {
		orig := cleanValue(val)
		out := rw.spec.ShiftDate(orig)
		rw.auditText(h.tag, orig, out)
		return padString(vr, out), vr, true
	}
	if isTextVR(vr) {
		rw.auditText(h.tag, cleanValue(val), cleanValue(val))
	}
	return nil, vr, false
}

// auditText reports when the OUTPUT value still contains an original
// secret, catching identity/broken mappings while allowing correctly
// transformed elements to pass.
func (rw *rewriter) auditText(t Tag, orig, output string) {
	for _, secret := range rw.spec.AuditSecrets {
		if secret != "" && containsSecret(output, secret) && output != "" {
			rw.findings = append(rw.findings, AuditFinding{Tag: t, Value: output})
		}
	}
}

// rewriteSequence validates a sequence and re-emits it canonically
// (undefined length); each item's contents are transformed recursively.
func (rw *rewriter) rewriteSequence(r io.Reader, syn syntax, h elementHeader, w io.Writer) error {
	if err := writeSQHeader(w, syn, h.tag); err != nil {
		return err
	}
	emitItem := func(item io.Reader) error {
		var body bytes.Buffer
		sub := &rewriter{spec: rw.spec, findings: rw.findings}
		if err := sub.rewriteDataset(&streamReader{r: item}, syn, &body); err != nil {
			return fmt.Errorf("item body: %w", err)
		}
		rw.findings = sub.findings
		return writeItem(w, syn, body.Bytes())
	}
	if h.undefined {
		for {
			ih, err := readHeader(r, syn, "sequence item")
			if err != nil {
				if err == io.EOF {
					return fmt.Errorf("%w: sequence %s missing Sequence Delimiter", ErrTruncated, h.tag)
				}
				return fmt.Errorf("after item in undef SQ %s: %w", h.tag, err)
			}
			switch ih.tag {
			case tagSequenceDelim:
				return writeTag(w, syn, tagSequenceDelim)
			case tagItem:
				if ih.undefined {
					if err := emitItem(newDelimReader(r, syn)); err != nil {
						return err
					}
				} else {
					if err := emitItem(limitEndReader{&io.LimitedReader{R: r, N: int64(ih.length)}}); err != nil {
						return err
					}
				}
			default:
				return fmt.Errorf("%w: expected Item, got %s", ErrCorrupt, ih.tag)
			}
		}
	}
	lr := limitEndReader{&io.LimitedReader{R: r, N: int64(h.length)}}
	for {
		ih, err := readHeader(lr, syn, "sequence item")
		if err != nil {
			if errors.Is(err, errLimitEnd) {
				return writeTag(w, syn, tagSequenceDelim)
			}
			return fmt.Errorf("%w: sequence %s shorter than declared", ErrTruncated, h.tag)
		}
		if ih.tag != tagItem {
			return fmt.Errorf("%w: expected Item, got %s", ErrCorrupt, ih.tag)
		}
		if ih.undefined {
			if err := emitItem(newDelimReader(lr, syn)); err != nil {
				return err
			}
		} else {
			if err := emitItem(limitEndReader{&io.LimitedReader{R: lr, N: int64(ih.length)}}); err != nil {
				return err
			}
		}
	}
}

// copyPixelAndTrailer writes the pixel header, validates/copies pixel data
// byte-for-byte, then copies any elements after pixel data verbatim.
func copyPixelAndTrailer(r io.Reader, syn syntax, h elementHeader, w io.Writer) error {
	if _, err := w.Write(h.hdr); err != nil {
		return err
	}
	if err := copyPixelVerbatim(r, syn, h, w); err != nil {
		return err
	}
	if _, err := io.Copy(w, r); err != nil {
		return err
	}
	return nil
}

// copyPixelVerbatim validates and copies pixel data unchanged.
func copyPixelVerbatim(r io.Reader, syn syntax, h elementHeader, w io.Writer) error {
	if !h.undefined {
		if _, err := io.CopyN(w, r, int64(h.length)); err != nil && err != io.EOF {
			return fmt.Errorf("%w: pixel data: %v", ErrTruncated, err)
		}
		return nil
	}
	for {
		ih, err := readHeader(r, syn, "pixel fragment")
		if err != nil {
			if err == io.EOF {
				return fmt.Errorf("%w: pixel data missing Sequence Delimiter", ErrTruncated)
			}
			return err
		}
		if _, err := w.Write(ih.hdr); err != nil {
			return err
		}
		switch ih.tag {
		case tagSequenceDelim:
			return nil
		case tagItem:
			if ih.undefined {
				return fmt.Errorf("%w: undefined-length pixel fragment", ErrCorrupt)
			}
			if _, err := io.CopyN(w, r, int64(ih.length)); err != nil && err != io.EOF {
				return fmt.Errorf("%w: pixel fragment: %v", ErrTruncated, err)
			}
		default:
			return fmt.Errorf("%w: expected pixel fragment Item, got %s", ErrCorrupt, ih.tag)
		}
	}
}
