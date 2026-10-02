package dicom

import "strings"

// Item is one dataset inside an SQ sequence.
type Item struct {
	Elements []*Element
}

// Element is a parsed DICOM data element.
//
// Exactly one of Value (single/multi-valued text), Raw (opaque bytes) or
// Items (sequence) is populated. Encapsulated pixel data is represented by
// Raw per fragment in Fragments.
type Element struct {
	Tag Tag
	VR  VR

	// Values holds backslash-free text values for string VRs (UI, LO, PN,
	// CS, DA, TM, IS, ...). Multi-valued elements have several entries.
	Values []string

	// Raw is the exact value bytes for non-sequence, non-text VRs (OB/OW/
	// UN/US/UL/...). Pixel data and unknown tags round-trip byte-for-byte.
	Raw []byte

	// Items is populated for VR SQ.
	Items []Item

	// Fragments holds encapsulated pixel data fragments, each starting with
	// the first payload byte after the item tag/length header.
	Fragments [][]byte

	// UndefinedLength marks elements/sequences encoded with 0xFFFFFFFF length.
	UndefinedLength bool
}

// First returns the first text value, or "".
func (e *Element) First() string {
	if e == nil || len(e.Values) == 0 {
		return ""
	}
	return e.Values[0]
}

// IsText reports whether Values may be used for this VR.
func (e *Element) IsText() bool {
	switch e.VR {
	case VRAE, VRAS, VRCS, VRDA, VRDS, VRDT, VRFL, VRFD, VRIS,
		VRLO, VRLT, VRPN, VRSH, VRSS, VRST, VRTM, VRUI, VRUT:
		return true
	}
	return false
}

// SetValues replaces text values and clears raw storage.
func (e *Element) SetValues(vs ...string) {
	e.Values = vs
	e.Raw = nil
}

// normalizeUID strips trailing padding and whitespace per DICOM PS3.5.
func normalizeUID(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, "\x00")
	return strings.TrimSpace(s)
}
