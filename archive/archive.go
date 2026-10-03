// Package archive implements a local, concurrency-safe DICOM archive:
// original files are copied verbatim into a patient/study/series directory
// layout, while a de-identified JSON index supports filtered, stably
// paginated queries and reproducible de-identified dataset exports.
// Import, query and export may run concurrently; readers never observe
// partially imported instances and exports see one consistent snapshot.
package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"

	"github.com/lechandonga/dicom-deident-archive-go/deident"
	"github.com/lechandonga/dicom-deident-archive-go/dicom"
)

// Distinct import rejection reasons, classifiable with errors.Is.
var (
	ErrConflict = errors.New("archive: conflicting instance content for same SOPInstanceUID")
	ErrParse    = errors.New("archive: file rejected during parsing")
)

// Instance is one de-identified index record.
type Instance struct {
	PatientID         string `json:"patient_id"`
	PatientName       string `json:"patient_name"`
	StudyUID          string `json:"study_uid"`
	SeriesUID         string `json:"series_uid"`
	SOPUID            string `json:"sop_uid"`
	Modality          string `json:"modality"`
	StudyDate         string `json:"study_date"`
	SeriesNumber      string `json:"series_number"`
	InstanceNo        string `json:"instance_number"`
	SOPClassUID       string `json:"sop_class_uid"`
	TransferSyntaxUID string `json:"transfer_syntax_uid"`
	SHA256            string `json:"sha256"`
	Path              string `json:"path"`
	ReImported        bool   `json:"reimported,omitempty"`
}

// ImportResult reports the outcome of importing one file.
type ImportResult struct {
	SourcePath string
	Instance   *Instance
	Duplicate  bool
}

// Query filters instances; zero fields match everything.
type Query struct {
	PatientID string
	Modality  string
	DateFrom  string
	DateTo    string
	Cursor    string
	Limit     int
}

// Page is one stable page of results.
type Page struct {
	Instances  []Instance
	NextCursor string
}

type indexFile struct {
	Instances []Instance `json:"instances"`
}

// Store is a concurrency-safe archive rooted at a directory.
type Store struct {
	dir    string
	mapper deident.Mapper
	mu     sync.RWMutex
	byUID  map[string]*Instance
	all    []*Instance
}

// Open loads (or creates) an archive rooted at dir.
func Open(dir string, mapper deident.Mapper) (*Store, error) {
	s := &Store{dir: dir, mapper: mapper, byUID: map[string]*Instance{}}
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(s.indexPath())
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f indexFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("archive: corrupt index %s: %w", s.indexPath(), err)
	}
	for i := range f.Instances {
		inst := f.Instances[i]
		s.byUID[inst.SOPUID] = &inst
		s.all = append(s.all, &inst)
	}
	sort.Slice(s.all, func(i, j int) bool { return less(s.all[i], s.all[j]) })
	return s, nil
}

func (s *Store) indexPath() string { return filepath.Join(s.dir, "index.json") }

// Mapper exposes the archive's mapper (used by the exporter).
func (s *Store) Mapper() deident.Mapper { return s.mapper }

// Dir exposes the archive root (exporter resolves data paths read-only).
func (s *Store) Dir() string { return s.dir }

// Import parses, validates (whole-file), de-identifies and archives one file.
func (s *Store) Import(srcPath string) (*ImportResult, error) {
	f, err := os.Open(srcPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	h := sha256.New()
	meta, err := dicom.ParseVerify(io.TeeReader(f, h))
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrParse, srcPath, err)
	}
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	sum := hex.EncodeToString(h.Sum(nil))

	inst := s.buildInstance(meta, sum)

	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.byUID[inst.SOPUID]; ok {
		if existing.SHA256 == sum {
			return &ImportResult{SourcePath: srcPath, Duplicate: true}, nil
		}
		return nil, fmt.Errorf("%w: %s (uid %s, existing hash %s, new hash %s)",
			ErrConflict, srcPath, meta.SOPInstanceUID, existing.SHA256[:12], sum[:12])
	}

	dst := filepath.Join(s.dir, inst.Path)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return nil, err
	}
	if err := copyFile(srcPath, dst); err != nil {
		return nil, err
	}

	s.byUID[inst.SOPUID] = inst
	pos := sort.Search(len(s.all), func(i int) bool { return !less(s.all[i], inst) })
	s.all = append(s.all, nil)
	copy(s.all[pos+1:], s.all[pos:])
	s.all[pos] = inst
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	return &ImportResult{SourcePath: srcPath, Instance: inst}, nil
}

// buildInstance computes the index record based on provenance markers.
func (s *Store) buildInstance(meta *dicom.Meta, sum string) *Instance {
	provenance, token := extractProvenance(meta)
	sameKey := provenance && token == s.mapper.ProvenanceToken()

	pid, name := meta.PatientID, meta.PatientName
	study, series, sop := meta.StudyInstanceUID, meta.SeriesInstanceUID, meta.SOPInstanceUID
	date := meta.StudyDate

	if !(provenance && sameKey) {
		pid = s.mapper.PatientID(meta.PatientID)
		name = s.mapper.PatientName(meta.PatientName)
		study = s.mapper.StudyUID(meta.StudyInstanceUID)
		series = s.mapper.SeriesUID(meta.SeriesInstanceUID)
		sop = s.mapper.SOPUID(meta.SOPInstanceUID)
		date = s.mapper.ShiftDate(meta.PatientID, meta.StudyDate)
	}

	inst := &Instance{
		PatientID:         pid,
		PatientName:       name,
		StudyUID:          study,
		SeriesUID:         series,
		SOPUID:            sop,
		Modality:          meta.Modality,
		StudyDate:         date,
		SeriesNumber:      meta.SeriesNumber,
		InstanceNo:        meta.InstanceNumber,
		SOPClassUID:       meta.SOPClassUID,
		TransferSyntaxUID: meta.TransferSyntaxUID,
		SHA256:            sum,
		ReImported:        provenance && sameKey,
	}
	inst.Path = filepath.Join("data", inst.PatientID, inst.StudyUID, inst.SeriesUID, inst.SOPUID+".dcm")
	return inst
}

// persistLocked rewrites the index atomically.
func (s *Store) persistLocked() error {
	f := indexFile{Instances: make([]Instance, 0, len(s.all))}
	for _, inst := range s.all {
		f.Instances = append(f.Instances, *inst)
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.indexPath() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.indexPath())
}

// Query returns a stable page of matching instances.
func (s *Store) Query(q Query) (Page, error) {
	if q.Limit <= 0 {
		q.Limit = 50
	}
	if q.Limit > 500 {
		q.Limit = 500
	}
	offset := 0
	if q.Cursor != "" {
		n, err := strconv.Atoi(q.Cursor)
		if err != nil || n < 0 {
			return Page{}, fmt.Errorf("archive: invalid cursor %q", q.Cursor)
		}
		offset = n
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	page := Page{Instances: []Instance{}}
	matched := 0
	for _, inst := range s.all {
		if !matches(inst, q) {
			continue
		}
		if matched < offset {
			matched++
			continue
		}
		if len(page.Instances) == q.Limit {
			page.NextCursor = strconv.Itoa(offset + q.Limit)
			break
		}
		page.Instances = append(page.Instances, *inst)
		matched++
	}
	return page, nil
}

// snapshotLocked returns a value-copy of all matching instances.
func (s *Store) snapshotLocked(q Query) []Instance {
	out := []Instance{}
	for _, inst := range s.all {
		if matches(inst, q) {
			out = append(out, *inst)
		}
	}
	return out
}

func matches(inst *Instance, q Query) bool {
	if q.PatientID != "" && inst.PatientID != q.PatientID {
		return false
	}
	if q.Modality != "" && inst.Modality != q.Modality {
		return false
	}
	if q.DateFrom != "" && inst.StudyDate < q.DateFrom {
		return false
	}
	if q.DateTo != "" && inst.StudyDate > q.DateTo {
		return false
	}
	return true
}

func less(a, b *Instance) bool {
	if a.StudyDate != b.StudyDate {
		return a.StudyDate < b.StudyDate
	}
	if a.StudyUID != b.StudyUID {
		return a.StudyUID < b.StudyUID
	}
	if numLess(a.SeriesNumber, b.SeriesNumber) {
		return true
	}
	if a.SeriesNumber != b.SeriesNumber {
		return false
	}
	if numLess(a.InstanceNo, b.InstanceNo) {
		return true
	}
	if a.InstanceNo != b.InstanceNo {
		return false
	}
	return a.SOPUID < b.SOPUID
}

func numLess(a, b string) bool {
	na, ea := strconv.Atoi(a)
	nb, eb := strconv.Atoi(b)
	if ea == nil && eb == nil {
		return na < nb
	}
	return false
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	buf := make([]byte, 256*1024)
	if _, err := io.CopyBuffer(out, in, buf); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
