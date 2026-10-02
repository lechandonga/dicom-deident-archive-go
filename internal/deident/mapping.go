package deident

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Mapping is the persistent, deterministic crosswalk between original and
// de-identified values. Persistence makes repeated batch runs idempotent:
// no second offset or pseudonym drift ever occurs.
type Mapping struct {
	PatientIDs    map[string]string `json:"patientIds"`    // original PatientID -> pseudo
	PatientNames  map[string]string `json:"patientNames"`  // original PN -> pseudo
	StudyUIDs     map[string]string `json:"studyUIDs"`     // original StudyInstanceUID -> new
	SeriesUIDs    map[string]string `json:"seriesUIDs"`    // original SeriesInstanceUID -> new
	SOPUIDs       map[string]string `json:"sopUIDs"`       // original SOPInstanceUID -> new
	PatientToBase map[string]string `json:"patientToBase"` // original PatientID -> base study date (anchor)
	PatientShift  map[string]int    `json:"patientShift"`  // original PatientID -> shift in days

	mu      sync.Mutex
	path    string
	salt    string
	orgUID  string
	maxDays int
}

// mappingFile is the JSON representation of Mapping (locks and runtime
// parameters excluded).
type mappingFile Mapping

// NewMapping loads (or creates) a mapping file at path.
func NewMapping(path, salt, orgUID string, maxShiftDays int) (*Mapping, error) {
	m := &Mapping{
		PatientIDs:    map[string]string{},
		PatientNames:  map[string]string{},
		StudyUIDs:     map[string]string{},
		SeriesUIDs:    map[string]string{},
		SOPUIDs:       map[string]string{},
		PatientToBase: map[string]string{},
		PatientShift:  map[string]int{},
		path:          path,
		salt:          salt,
		orgUID:        orgUID,
		maxDays:       maxShiftDays,
	}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, m); err != nil {
			return nil, fmt.Errorf("mapping file %s corrupt: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return m, nil
}

// Lock/Unlock guard multi-step transformations against concurrent importers.
func (m *Mapping) Lock()   { m.mu.Lock() }
func (m *Mapping) Unlock() { m.mu.Unlock() }

func (m *Mapping) mac(purpose, value string) []byte {
	h := hmac.New(sha256.New, []byte(m.salt+"|"+purpose))
	h.Write([]byte(value))
	return h.Sum(nil)
}

// MapPatientID returns the stable pseudonymous patient ID, e.g.
// "P-7f3a9c1b2d4e8f01". Collisions between distinct originals are checked
// and resolved by extending the hash (practically impossible with SHA-256).
func (m *Mapping) MapPatientID(original string) string {
	if v, ok := m.PatientIDs[original]; ok {
		return v
	}
	sum := m.mac("patient-id", original)
	for {
		cand := "P-" + hex.EncodeToString(sum[:8])
		collision := false
		for k, v := range m.PatientIDs {
			if k != original && v == cand {
				collision = true
				break
			}
		}
		if !collision {
			m.PatientIDs[original] = cand
			return cand
		}
		x := binary.BigEndian.Uint32(sum[28:])
		x++
		binary.BigEndian.PutUint32(sum[28:], x)
	}
}

// MapPatientName returns a stable anonymized PN bound to the original name.
func (m *Mapping) MapPatientName(original string) string {
	if original == "" {
		return ""
	}
	if v, ok := m.PatientNames[original]; ok {
		return v
	}
	sum := m.mac("patient-name", original)
	v := "ANON^" + hex.EncodeToString(sum[:6])
	m.PatientNames[original] = v
	return v
}

// MapUID maps any DICOM UID deterministically under the organization root.
// scope distinguishes study/series/sop namespaces (a UID reused at two
// levels is not supposed to cross levels).
func (m *Mapping) MapUID(scope, original string, target map[string]string) string {
	if v, ok := target[original]; ok {
		return v
	}
	sum := m.mac("uid:"+scope, original)
	n := new(big.Int).SetBytes(sum[:16])
	// 128-bit suffix as decimal digits.
	suffix := n.String()
	root := m.orgUID
	maxSuffix := 63 - len(root)
	if len(suffix) > maxSuffix {
		suffix = suffix[:maxSuffix]
	}
	cand := root + "." + suffix
	used := func(x string) bool {
		for _, v := range target {
			if v == x {
				return true
			}
		}
		return false
	}
	// Collision resolution: if two originals ever mapped to the same new
	// UID (truncation), extend with a discriminator.
	for k := 1; used(cand); k++ {
		extra := fmt.Sprintf(".%d", k)
		cand = root + "." + suffix[:max(0, maxSuffix-len(extra))] + extra
	}
	target[original] = cand
	return cand
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (m *Mapping) MapStudyUID(v string) string  { return m.MapUID("study", v, m.StudyUIDs) }
func (m *Mapping) MapSeriesUID(v string) string { return m.MapUID("series", v, m.SeriesUIDs) }
func (m *Mapping) MapSOPUID(v string) string    { return m.MapUID("sop", v, m.SOPUIDs) }

// DateShift returns the fixed whole-day offset for a patient. It is derived
// deterministically from the patient identity (HMAC) and bounded by
// maxDays. The same patient always gets the same shift across all files,
// studies and reruns; distinct patients get independent shifts.
func (m *Mapping) DateShift(originalPatientID string) int {
	if v, ok := m.PatientShift[originalPatientID]; ok {
		return v
	}
	if m.maxDays <= 0 {
		m.PatientShift[originalPatientID] = 0
		return 0
	}
	sum := m.mac("date-shift", originalPatientID)
	n := binary.BigEndian.Uint64(sum)
	shift := int(n%uint64(m.maxDays)) + 1
	m.PatientShift[originalPatientID] = shift
	return shift
}

// ShiftDate applies the patient's fixed offset to a YYYYMMDD date. Empty
// input stays empty; invalid input returns an error (parser normally
// rejects invalid dates earlier).
func (m *Mapping) ShiftDate(patientID, date string) (string, error) {
	if date == "" {
		return "", nil
	}
	t, err := time.Parse("20060102", date)
	if err != nil {
		return "", err
	}
	shift := m.DateShift(patientID)
	return t.AddDate(0, 0, shift).Format("20060102"), nil
}

// Save atomically writes the mapping (temp file + rename).
func (m *Mapping) Save() error {
	if m.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent((*mappingFile)(m), "", "  ")
	if err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}
