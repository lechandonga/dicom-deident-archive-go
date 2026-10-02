package archive

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lechandonga/dicom-deident-archive-go/internal/config"
	"github.com/lechandonga/dicom-deident-archive-go/internal/deident"
	"github.com/lechandonga/dicom-deident-archive-go/internal/dicom"
)

// Archive is the local de-identification archive.
//
// Layout under root:
//
//	index.json                 - queryable catalog (atomic rewrite)
//	mapping.json               - original -> de-identified crosswalk
//	originals/ab/cd/<sha>.dcm  - byte-exact copies of source files
//	studies/<patient>/<study>/<series>/<sop>.dcm
//	                           - de-identified PS3.10 instances (Explicit LE)
//	rejects.log                - append-only rejection audit (optional)
type Archive struct {
	root     string
	cfg      *config.Compiled
	mapping  *deident.Mapping
	idx      *Index
	writer   *dicom.Writer
	importMu chan struct{} // serializes imports; readers use RWMutex
}

func Open(root string, cfg *config.Compiled) (*Archive, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	for _, sub := range []string{"originals", "studies"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			return nil, err
		}
	}
	m, err := deident.NewMapping(
		filepath.Join(root, "mapping.json"),
		cfg.Raw.Salt, cfg.Raw.OrgUID, cfg.Raw.DateShiftDays)
	if err != nil {
		return nil, err
	}
	idx := newIndex(root)
	if err := idx.load(); err != nil {
		return nil, err
	}
	return &Archive{
		root:     root,
		cfg:      cfg,
		mapping:  m,
		idx:      idx,
		writer:   dicom.NewWriter(),
		importMu: make(chan struct{}, 1),
	}, nil
}

// ImportOutcome reports what happened to one input file.
type ImportOutcome struct {
	File       string
	Accepted   bool
	Idempotent bool // true if an identical file was already archived
	Instance   *Instance
	Reason     string
	Detail     string
}

// ImportFile parses, de-identifies and archives one local file.
//
// Imports are serialized by a single-slot semaphore, while queries hold only
// the index read lock. The index/persistence commit happens under the write
// lock as the last step, so concurrent queries see only complete instances
// and never duplicates or gaps.
func (a *Archive) ImportFile(path string) ImportOutcome {
	a.importMu <- struct{}{}
	defer func() { <-a.importMu }()

	out := ImportOutcome{File: path}

	raw, err := os.ReadFile(path)
	if err != nil {
		out.Reason = dicom.ReasonTruncated
		out.Detail = err.Error()
		return out
	}

	ds, perr := dicom.ParseBytes(raw, filepath.Base(path))
	if perr != nil {
		var re *dicom.RejectError
		if errors.As(perr, &re) {
			out.Reason = re.Reason
			out.Detail = re.Error()
		} else {
			out.Reason = dicom.ReasonBadEncoding
			out.Detail = perr.Error()
		}
		return out
	}

	hash := contentHash(raw)

	a.mapping.Lock()
	defer a.mapping.Unlock()

	a.idx.mu.Lock()
	if existingSOP, ok := a.idx.byContent[hash]; ok {
		in := a.idx.instances[existingSOP]
		a.idx.mu.Unlock()
		out.Accepted = true
		out.Idempotent = true
		out.Instance = in
		out.Reason = dicom.ReasonDuplicateImport
		out.Detail = "identical content already archived as " + existingSOP
		return out
	}
	a.idx.mu.Unlock()

	tr, terr := deident.NewTransformer(a.cfg, a.mapping).Apply(ds)
	if terr != nil {
		var re *dicom.RejectError
		if errors.As(terr, &re) {
			out.Reason = re.Reason
			out.Detail = re.Error()
		} else {
			out.Reason = dicom.ReasonBadEncoding
			out.Detail = terr.Error()
		}
		return out
	}

	a.idx.mu.Lock()
	defer a.idx.mu.Unlock()

	if existing, ok := a.idx.instances[tr.SOPUID]; ok {
		out.Reason = dicom.ReasonDuplicateInstance
		out.Detail = fmt.Sprintf(
			"SOP instance %s already archived with different content (existing sha %s, incoming %s)",
			tr.SOPUID, existing.ContentSHA256, hash)
		return out
	}

	// 1) Persist the byte-exact original (never modified).
	origPath, err := a.writeOriginal(raw, hash)
	if err != nil {
		out.Reason = "IO_ERROR"
		out.Detail = err.Error()
		return out
	}

	// 2) Persist the de-identified copy (pixel payload unchanged).
	deidBytes := a.writer.WriteFile(tr.Meta, tr.Elements)
	deidPath := filepath.Join("studies", tr.PatientID, tr.StudyUID, tr.SeriesUID, tr.SOPUID+".dcm")
	if err := writeFileAll(filepath.Join(a.root, deidPath), deidBytes); err != nil {
		out.Reason = "IO_ERROR"
		out.Detail = err.Error()
		return out
	}

	in := &Instance{
		PatientID:        tr.PatientID,
		Modality:         tr.Modality,
		StudyDate:        tr.StudyDate,
		StudyUID:         tr.StudyUID,
		SeriesUID:        tr.SeriesUID,
		SOPUID:           tr.SOPUID,
		InstanceNumber:   ds.Find(dicom.TagInstanceNumber).First(),
		OriginalFile:     origPath,
		DeidentifiedFile: deidPath,
		OriginalBytes:    int64(len(raw)),
		ContentSHA256:    hash,
		ImportedAt:       nowUTC(),
	}

	// 3) Commit index + mapping atomically-ish: index rename is atomic,
	// mapping rename is atomic; the two files are each consistent on their
	// own and readers only consume index.json.
	a.idx.instances[in.SOPUID] = in
	a.idx.byContent[hash] = in.SOPUID
	if err := a.idx.saveLocked(); err != nil {
		out.Reason = "IO_ERROR"
		out.Detail = err.Error()
		delete(a.idx.instances, in.SOPUID)
		delete(a.idx.byContent, hash)
		return out
	}
	if err := a.mapping.Save(); err != nil {
		out.Reason = "IO_ERROR"
		out.Detail = err.Error()
		delete(a.idx.instances, in.SOPUID)
		delete(a.idx.byContent, hash)
		return out
	}

	out.Accepted = true
	out.Instance = in
	return out
}

func (a *Archive) writeOriginal(raw []byte, hash string) (string, error) {
	rel := filepath.Join("originals", hash[:2], hash[2:4], hash+".dcm")
	full := filepath.Join(a.root, rel)
	if _, err := os.Stat(full); err == nil {
		return rel, nil // already exists; identical sha => identical bytes
	}
	if err := writeFileAll(full, raw); err != nil {
		return "", err
	}
	return rel, nil
}

func writeFileAll(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Search runs a paginated query against the committed index.
func (a *Archive) Search(q Query) Page { return a.idx.Search(q) }

// InstanceCount returns the number of committed instances.
func (a *Archive) InstanceCount() int {
	a.idx.mu.RLock()
	defer a.idx.mu.RUnlock()
	return len(a.idx.instances)
}

// MappingPath exposes the crosswalk location (for audit tooling).
func (a *Archive) MappingPath() string { return filepath.Join(a.root, "mapping.json") }
