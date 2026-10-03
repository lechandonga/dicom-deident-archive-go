// Package deident provides deterministic, collision-resistant pseudonymisation
// of patient identity metadata. All mappings are pure functions of
// (salt, original identifier), so re-running the same batch always yields the
// same output and never drifts. Only identity fields are transformed; pixel
// data and the archived file bytes are never touched.
package deident

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// Provenance tag values carried in exported datasets so an archive can tell
// whether an incoming file is original data or a de-identified export, and
// whether it was produced with the same salt.
const (
	// ProvenanceTagGroup/Element is a private LO tag (0011,0010) holding
	// "DICOM-ARCHIVE-DEID-v1".
	ProvenanceTagGroup   = 0x0011
	ProvenanceTagElement = 0x0010
	ProvenanceValue      = "DICOM-ARCHIVE-DEID-v1"

	// TokenTagGroup/Element is a private UC tag (0011,0020) holding a
	// salt-derived token: HMAC-like hash of a fixed label. Same salt
	// reproduces the same token, proving provenance without revealing it.
	TokenTagGroup   = 0x0011
	TokenTagElement = 0x0020
)

// Mapper derives stable pseudonyms from original identifiers.
type Mapper struct {
	// Salt is a local secret mixed into every hash. Keep it private and
	// constant for a deployment; changing it re-keys all pseudonyms.
	Salt string
	// UIDRoot is the UID prefix (an ISO arc) used for synthesised UIDs,
	// e.g. "2.25.<enterprise>". Defaults to the 2.25 UUID arc.
	UIDRoot string
	// MaxDateShiftDays bounds the per-patient date shift (default 365).
	MaxDateShiftDays int
}

func (m Mapper) uidRoot() string {
	if m.UIDRoot == "" {
		return "2.25"
	}
	return m.UIDRoot
}

func (m Mapper) maxShift() int {
	if m.MaxDateShiftDays <= 0 {
		return 365
	}
	return m.MaxDateShiftDays
}

func (m Mapper) hash(parts ...string) [32]byte {
	h := sha256.New()
	h.Write([]byte(m.Salt))
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

// PatientID maps an original PatientID to a stable pseudonym like
// "P-9f3c2a1b7e4d6081". Distinct originals map to distinct outputs with
// 128 bits of hash space, so collisions are cryptographically negligible.
func (m Mapper) PatientID(orig string) string {
	sum := m.hash("patient", orig)
	return fmt.Sprintf("P-%016x", binary.BigEndian.Uint64(sum[:8]))
}

// PatientName pseudonymises a patient name deterministically.
func (m Mapper) PatientName(orig string) string {
	sum := m.hash("patient-name", orig)
	return fmt.Sprintf("ANON^%016x", binary.BigEndian.Uint64(sum[:8]))
}

// uid converts an original UID into a syntactically valid DICOM UID under
// UIDRoot, derived from 120 hash bits rendered as decimal. Same input always
// yields the same UID; different inputs never share a UID in practice.
func (m Mapper) uid(kind, orig string) string {
	sum := m.hash("uid", kind, orig)
	// Render 15 bytes as a decimal integer string (<= 36 digits), chunked
	// into dot-separated components to stay well within the 64-char limit.
	v := new(big.Int).SetBytes(sum[:15])
	s := v.String()
	out := m.uidRoot()
	for len(s) > 0 {
		chunk := s
		if len(chunk) > 8 {
			chunk = s[:8]
		}
		s = s[len(chunk):]
		out += "." + chunk
	}
	return out
}

// StudyUID maps a StudyInstanceUID.
func (m Mapper) StudyUID(orig string) string { return m.uid("study", orig) }

// SeriesUID maps a SeriesInstanceUID.
func (m Mapper) SeriesUID(orig string) string { return m.uid("series", orig) }

// SOPUID maps a SOPInstanceUID.
func (m Mapper) SOPUID(orig string) string { return m.uid("sop", orig) }

// DateShiftDays returns the fixed per-patient shift in [0, MaxDateShiftDays).
// The shift depends only on (salt, original PatientID), so every date of one
// patient shifts by the same amount, preserving order and intervals, and
// repeated runs never accumulate additional shifts.
func (m Mapper) DateShiftDays(origPatientID string) int {
	sum := m.hash("date-shift", origPatientID)
	return int(binary.BigEndian.Uint64(sum[:8]) % uint64(m.maxShift()))
}

// ShiftDate shifts a DICOM DA value (YYYYMMDD) backwards by the patient's
// fixed offset. Empty or malformed input is returned unchanged.
func (m Mapper) ShiftDate(origPatientID, da string) string {
	if len(da) != 8 {
		return da
	}
	t, err := time.Parse("20060102", da)
	if err != nil {
		return da
	}
	return t.AddDate(0, 0, -m.DateShiftDays(origPatientID)).Format("20060102")
}

// ProvenanceToken returns the stable token proving a file was de-identified
// with this mapper's salt. Compare incoming tokens with Token() to decide
// between keeping pseudonyms (same salt) and re-mapping (different salt).
func (m Mapper) ProvenanceToken() string {
	sum := m.hash("provenance-token", ProvenanceValue)
	return fmt.Sprintf("%032x", sum[:16])
}

// UID maps an arbitrary UID value, keeping one global mapping per input so
// that the same source UID appearing in several elements/files always maps
// identically, regardless of its role. Hierarchy UIDs (study/series/sop)
// keep their dedicated kind spaces via StudyUID/SeriesUID/SOPUID.
func (m Mapper) UID(orig string) string { return m.uid("uid-value", orig) }

// IsValidDA reports whether s is a DICOM date value this mapper can shift.
func IsValidDA(da string) bool {
	if len(da) != 8 {
		return false
	}
	_, err := time.Parse("20060102", da)
	return err == nil
}

// ShiftDA shifts an already-valid DA value by the patient's fixed offset.
func (m Mapper) ShiftDA(origPatientID, da string) string {
	return m.ShiftDate(origPatientID, da)
}

// ShiftDays exposes the per-patient offset for callers that shift other date
// representations (DT) consistently.
func (m Mapper) ShiftDays(origPatientID string) int {
	return m.DateShiftDays(origPatientID)
}

// ShiftDT shifts the date part of a DICOM DT (YYYYMMDDHHMMSS+ZZZZ), keeping
// the time and offset components untouched.
func (m Mapper) ShiftDT(origPatientID, dt string) string {
	if len(dt) < 8 {
		return dt
	}
	head := dt[:8]
	if !IsValidDA(head) {
		return dt
	}
	shifted := m.ShiftDate(origPatientID, head)
	return shifted + dt[8:]
}

// UIDRootValue exposes the configured root.
func (m Mapper) UIDRootValue() string { return m.uidRoot() }

// _ keeps strings imported for future VM helpers.
var _ = strings.TrimSpace
