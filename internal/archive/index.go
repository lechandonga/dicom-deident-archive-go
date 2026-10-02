package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Instance is one de-identified, archived image.
type Instance struct {
	PatientID        string `json:"patientId"`
	Modality         string `json:"modality"`
	StudyDate        string `json:"studyDate"`
	StudyUID         string `json:"studyUid"`
	SeriesUID        string `json:"seriesUid"`
	SOPUID           string `json:"sopUid"`
	InstanceNumber   string `json:"instanceNumber"`
	OriginalFile     string `json:"originalFile"`
	DeidentifiedFile string `json:"deidentifiedFile"`
	OriginalBytes    int64  `json:"originalBytes"`
	ContentSHA256    string `json:"contentSha256"`
	ImportedAt       string `json:"importedAt"`
}

// Index is the queryable catalog. It is held entirely in memory behind a
// RWMutex and persisted atomically after every successful import. Readers
// therefore never observe a partially imported instance.
type Index struct {
	mu sync.RWMutex

	instances map[string]*Instance // key = SOP UID (de-identified)
	byContent map[string]string    // content sha256 -> SOP UID

	// idMapping stores the de-identification crosswalk file location for
	// audit; the actual mapping lives in deident.Mapping.
	root string
}

func newIndex(root string) *Index {
	return &Index{
		instances: map[string]*Instance{},
		byContent: map[string]string{},
		root:      root,
	}
}

type indexSnapshot struct {
	Instances []*Instance `json:"instances"`
}

func (idx *Index) path() string { return filepath.Join(idx.root, "index.json") }

func (idx *Index) load() error {
	b, err := os.ReadFile(idx.path())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var snap indexSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return fmt.Errorf("index.json corrupt: %w", err)
	}
	for _, in := range snap.Instances {
		idx.instances[in.SOPUID] = in
		idx.byContent[in.ContentSHA256] = in.SOPUID
	}
	return nil
}

func (idx *Index) saveLocked() error {
	snap := indexSnapshot{}
	for _, in := range idx.instances {
		snap.Instances = append(snap.Instances, in)
	}
	sort.Slice(snap.Instances, func(i, j int) bool {
		return sortKey(snap.Instances[i]) < sortKey(snap.Instances[j])
	})
	b, err := json.MarshalIndent(&snap, "", "  ")
	if err != nil {
		return err
	}
	tmp := idx.path() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, idx.path())
}

// sortKey is the globally stable ordering (study, series, instance number,
// sop UID) used both on disk and for paginated queries.
func sortKey(in *Instance) string {
	return strings.Join([]string{
		in.PatientID, in.StudyDate, in.StudyUID,
		in.SeriesUID, padInstanceNumber(in.InstanceNumber), in.SOPUID,
	}, "|")
}

func padInstanceNumber(s string) string {
	if s == "" {
		return "0000000000"
	}
	if len(s) < 10 {
		return strings.Repeat("0", 10-len(s)) + s
	}
	return s
}

// Query filters instances. Empty fields match anything.
type Query struct {
	PatientID     string
	Modality      string
	StudyDateFrom string // inclusive YYYYMMDD
	StudyDateTo   string // inclusive YYYYMMDD
	StudyUID      string
	SeriesUID     string

	// Offset/Limit provide stable keyset-independent offset pagination over
	// the fixed sort order; Limit <= 0 means 50.
	Offset int
	Limit  int
}

// Page is one query result page plus the total number of matches.
type Page struct {
	Total     int         `json:"total"`
	Offset    int         `json:"offset"`
	Limit     int         `json:"limit"`
	Instances []*Instance `json:"instances"`
}

func (q Query) matches(in *Instance) bool {
	if q.PatientID != "" && in.PatientID != q.PatientID {
		return false
	}
	if q.Modality != "" && !strings.EqualFold(in.Modality, q.Modality) {
		return false
	}
	if q.StudyUID != "" && in.StudyUID != q.StudyUID {
		return false
	}
	if q.SeriesUID != "" && in.SeriesUID != q.SeriesUID {
		return false
	}
	if q.StudyDateFrom != "" && in.StudyDate < q.StudyDateFrom {
		return false
	}
	if q.StudyDateTo != "" && in.StudyDate > q.StudyDateTo {
		return false
	}
	return true
}

// Search returns a stable, sorted, paginated snapshot. It takes the read
// lock only while copying pointers, so concurrent imports never block long.
func (idx *Index) Search(q Query) Page {
	idx.mu.RLock()
	matched := make([]*Instance, 0, len(idx.instances))
	for _, in := range idx.instances {
		if q.matches(in) {
			matched = append(matched, in)
		}
	}
	idx.mu.RUnlock()

	sort.Slice(matched, func(i, j int) bool {
		return sortKey(matched[i]) < sortKey(matched[j])
	})

	total := len(matched)
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	page := Page{Total: total, Offset: q.Offset, Limit: limit}
	if q.Offset > total {
		return page
	}
	end := q.Offset + limit
	if end > total {
		end = total
	}
	page.Instances = matched[q.Offset:end]
	return page
}

func contentHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }
