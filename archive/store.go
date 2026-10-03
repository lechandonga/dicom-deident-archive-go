// Package archive implements a local, concurrency-safe DICOM archive:
// original files are copied verbatim into a patient/study/series layout, a
// de-identified JSON index supports filtered queries, and Export produces
// reproducible, self-checked de-identified datasets that can be re-imported
// into an empty archive without ever shifting or remapping a second time.
package archive

import (
	"bufio"
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

// Distinct rejection reasons, classifiable with errors.Is.
var (
	ErrConflict         = errors.New("archive: conflicting instance content for same SOPInstanceUID")
	ErrParse            = errors.New("archive: file rejected during parsing")
	ErrExportContent    = errors.New("archive: export target file exists with different content")
	ErrResidualIdentity = errors.New("archive: residual original identity found in exported file")
)

// Instance is one de-identified index record.
type Instance struct {
	PatientID    string `json:"patient_id"`
	PatientName  string `json:"patient_name"`
	StudyUID     string `json:"study_uid"`
	SeriesUID    string `json:"series_uid"`
	SOPUID       string `json:"sop_uid"`
	Modality     string `json:"modality"`
	StudyDate    string `json:"study_date"`
	SeriesNumber string `json:"series_number"`
	InstanceNo   string `json:"instance_number"`
	SOPClassUID  string `json:"sop_class_uid"`
	OrigSOPUID   string `json:"orig_sop_uid"`
	SHA256       string `json:"sha256"` // hash of archived original bytes
	ExportSHA256 string `json:"export_sha256,omitempty"`
	Path         string `json:"path"` // archive-relative path of the verbatim copy
	ExportPath   string `json:"export_path,omitempty"`
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
	dir         string
	mapper      deident.Mapper
	fingerprint string
	mu          sync.RWMutex
	byUID       map[string]*Instance
	all         []*Instance
}

// Open loads (or creates) an archive rooted at dir.
func Open(dir string, mapper deident.Mapper) (*Store, error) {
	s := &Store{
		dir:         dir,
		mapper:      mapper,
		fingerprint: mapper.Fingerprint(),
		byUID:       map[string]*Instance{},
	}
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

const streamBufSize = 1 << 20

// Import parses, de-identifies and archives one DICOM file.
//
// Files already carrying a de-identification marker matching THIS key keep
// their pseudonyms/dates verbatim (never shifted twice). Files carrying a
// different key are accepted and their visible pseudonyms are treated as a
// fresh identity set.
//
// Same SOP UID + identical content: idempotent Duplicate. Same SOP UID +
// different content: ErrConflict.
func (s *Store) Import(srcPath string) (*ImportResult, error) {
	f, err := os.Open(srcPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	h := sha256.New()
	br := bufio.NewReaderSize(io.TeeReader(f, h), streamBufSize)
	meta, err := dicom.Parse(br)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrParse, srcPath, err)
	}
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	sum := hex.EncodeToString(h.Sum(nil))

	sameKey := meta.DeidentFingerprint != "" && meta.DeidentFingerprint == s.fingerprint

	var inst *Instance
	if sameKey {
		inst = &Instance{
			PatientID:    meta.PatientID,
			PatientName:  meta.PatientName,
			StudyUID:     meta.StudyInstanceUID,
			SeriesUID:    meta.SeriesInstanceUID,
			SOPUID:       meta.SOPInstanceUID,
			Modality:     meta.Modality,
			StudyDate:    meta.StudyDate,
			SeriesNumber: meta.SeriesNumber,
			InstanceNo:   meta.InstanceNumber,
			SOPClassUID:  meta.SOPClassUID,
			OrigSOPUID:   deident.NormalizeUID(meta.DeidentOriginalUID),
			SHA256:       sum,
		}
		if inst.OrigSOPUID == "" {
			inst.OrigSOPUID = deident.NormalizeUID(meta.SOPInstanceUID)
		}
	} else {
		inst = &Instance{
			PatientID:    s.mapper.PatientID(meta.PatientID),
			PatientName:  s.mapper.PatientName(meta.PatientName),
			StudyUID:     s.mapper.StudyUID(meta.StudyInstanceUID),
			SeriesUID:    s.mapper.SeriesUID(meta.SeriesInstanceUID),
			SOPUID:       s.mapper.SOPUID(meta.SOPInstanceUID),
			Modality:     meta.Modality,
			StudyDate:    s.mapper.ShiftDate(meta.PatientID, meta.StudyDate),
			SeriesNumber: meta.SeriesNumber,
			InstanceNo:   meta.InstanceNumber,
			SOPClassUID:  meta.SOPClassUID,
			OrigSOPUID:   deident.NormalizeUID(meta.SOPInstanceUID),
			SHA256:       sum,
		}
	}
	inst.Path = filepath.Join("data", inst.PatientID, inst.StudyUID, inst.SeriesUID, inst.SOPUID+".dcm")

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
	if err := copyFileAtomic(srcPath, dst); err != nil {
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
	page, _, err := s.queryLocked(q, false)
	return page, err
}

// snapshot returns every instance matching q as a consistent copy.
func (s *Store) snapshot(q Query) []Instance {
	_, all, err := s.queryLocked(q, true)
	if err != nil {
		panic(err)
	}
	return all
}

func (s *Store) queryLocked(q Query, all bool) (Page, []Instance, error) {
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
			return Page{}, nil, fmt.Errorf("archive: invalid cursor %q", q.Cursor)
		}
		offset = n
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	var everything []Instance
	page := Page{Instances: []Instance{}}
	matched := 0
	for _, inst := range s.all {
		if !matches(inst, q) {
			continue
		}
		if all {
			everything = append(everything, *inst)
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
	return page, everything, nil
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
	if a.SeriesNumber != b.SeriesNumber {
		return numLess(a.SeriesNumber, b.SeriesNumber)
	}
	if a.InstanceNo != b.InstanceNo {
		return numLess(a.InstanceNo, b.InstanceNo)
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

func copyFileAtomic(src, dst string) error {
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
	bw := bufio.NewWriterSize(out, streamBufSize)
	if _, err := io.Copy(bw, bufio.NewReaderSize(in, streamBufSize)); err != nil {
		out.Close()
		return err
	}
	if err := bw.Flush(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
