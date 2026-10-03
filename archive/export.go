package archive

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/lechandonga/dicom-deident-archive-go/deident"
	"github.com/lechandonga/dicom-deident-archive-go/dicom"
)

// ManifestEntry describes one exported instance.
type ManifestEntry struct {
	SourcePath      string `json:"source_path"`   // archive-relative path of original bytes
	SourceSHA256    string `json:"source_sha256"` // hash of original bytes
	OrigPatientID   string `json:"orig_patient_id,omitempty"`
	OrigPatientName string `json:"orig_patient_name,omitempty"`
	OrigSOPUID      string `json:"orig_sop_uid"`
	PatientID       string `json:"patient_id"`   // pseudo
	PatientName     string `json:"patient_name"` // pseudo
	StudyUID        string `json:"study_uid"`
	SeriesUID       string `json:"series_uid"`
	SOPUID          string `json:"sop_uid"`
	Modality        string `json:"modality"`
	StudyDate       string `json:"study_date"`    // shifted
	FileSHA256      string `json:"file_sha256"`   // hash of exported file
	RelativePath    string `json:"relative_path"` // relative to export dir
}

// Manifest is the published, byte-for-byte reproducible export manifest.
type Manifest struct {
	Schema      int             `json:"schema"`
	Method      string          `json:"method"`
	Fingerprint string          `json:"fingerprint"` // key fingerprint, not the key
	Entries     []ManifestEntry `json:"entries"`
}

const manifestName = "manifest.json"

// Export writes a de-identified copy of every instance matching q into
// targetDir/data/... and writes manifest.json.
//
// Reproducible: identical inputs always produce identical file bytes and
// manifest bytes. Idempotent: reruns skip identical files, never duplicate
// or modify them. Safe: an existing same-named file with different content
// aborts with ErrExportContent; a mid-run failure leaves only hidden staging
// files and never overwrites prior good output. The archive is only read.
func (s *Store) Export(q Query, targetDir string) (*Manifest, error) {
	instances := s.snapshot(q)
	if err := os.MkdirAll(filepath.Join(targetDir, "data"), 0o755); err != nil {
		return nil, err
	}

	sort.Slice(instances, func(i, j int) bool { return less(&instances[i], &instances[j]) })

	// Stage into a unique hidden directory; publish only on full success.
	staging, err := os.MkdirTemp(targetDir, ".export-staging-*")
	if err != nil {
		return nil, err
	}
	stagingOK := false
	defer func() {
		if !stagingOK {
			os.RemoveAll(staging)
		}
	}()

	manifest := Manifest{
		Schema:      1,
		Method:      deident.MethodVersion,
		Fingerprint: s.fingerprint,
		Entries:     make([]ManifestEntry, 0, len(instances)),
	}

	for _, inst := range instances {
		entry, err := s.exportOne(inst, targetDir, staging)
		if err != nil {
			return nil, err
		}
		manifest.Entries = append(manifest.Entries, entry)
	}

	// Render manifest deterministically and compare with any existing one.
	mb, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	mb = append(mb, '\n')
	manifestDst := filepath.Join(targetDir, manifestName)
	if err := publishBytes(manifestDst, mb); err != nil {
		return nil, err
	}

	if err := os.RemoveAll(staging); err != nil {
		return nil, err
	}
	stagingOK = true
	return &manifest, nil
}

// exportOne rewrites one instance into staging, verifies it, then publishes
// the file into targetDir with conflict detection.
func (s *Store) exportOne(inst Instance, targetDir, staging string) (ManifestEntry, error) {
	src := filepath.Join(s.dir, inst.Path)
	in, err := os.Open(src)
	if err != nil {
		return ManifestEntry{}, err
	}
	defer in.Close()

	// Re-parse the ORIGINAL to recover identity values and discover any
	// extra UID/date fields outside the index.
	origMeta, err := dicom.Parse(bufio.NewReaderSize(in, streamBufSize))
	if err != nil {
		return ManifestEntry{}, fmt.Errorf("%w: reparse archived %s: %w", ErrParse, inst.Path, err)
	}
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		return ManifestEntry{}, err
	}
	origTexts, err := dicom.ScanMetadata(bufio.NewReaderSize(in, streamBufSize))
	if err != nil {
		return ManifestEntry{}, fmt.Errorf("%w: scan archived %s: %w", ErrParse, inst.Path, err)
	}
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		return ManifestEntry{}, err
	}

	plan, originals, err := s.buildPlan(origMeta, inst, origTexts)
	if err != nil {
		return ManifestEntry{}, err
	}

	relPath := filepath.Join("data", inst.PatientID, inst.StudyUID, inst.SeriesUID, inst.SOPUID+".dcm")
	stagePath := filepath.Join(staging, filepath.Base(relPath)+".staged")
	sf, err := os.OpenFile(stagePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return ManifestEntry{}, err
	}
	hw := sha256.New()
	mw := io.MultiWriter(sf, hw)
	bw := bufio.NewWriterSize(mw, streamBufSize)
	if err := dicom.RewriteStream(bufio.NewReaderSize(in, streamBufSize), bw, plan); err != nil {
		sf.Close()
		return ManifestEntry{}, fmt.Errorf("%w: rewrite %s: %w", ErrParse, inst.Path, err)
	}
	if err := bw.Flush(); err != nil {
		sf.Close()
		return ManifestEntry{}, err
	}
	if err := sf.Close(); err != nil {
		return ManifestEntry{}, err
	}

	// Self-check: the rewritten file must contain no original identity.
	exportedHash := hex.EncodeToString(hw.Sum(nil))
	if err := s.checkNoResidual(stagePath, originals); err != nil {
		return ManifestEntry{}, err
	}

	// Publish with same-name/different-content protection.
	dst := filepath.Join(targetDir, relPath)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return ManifestEntry{}, err
	}
	if err := publishFile(stagePath, dst); err != nil {
		return ManifestEntry{}, err
	}

	return ManifestEntry{
		SourcePath:      inst.Path,
		SourceSHA256:    inst.SHA256,
		OrigPatientID:   origMeta.PatientID,
		OrigPatientName: origMeta.PatientName,
		OrigSOPUID:      originals.sop,
		PatientID:       inst.PatientID,
		PatientName:     inst.PatientName,
		StudyUID:        inst.StudyUID,
		SeriesUID:       inst.SeriesUID,
		SOPUID:          inst.SOPUID,
		Modality:        inst.Modality,
		StudyDate:       inst.StudyDate,
		FileSHA256:      exportedHash,
		RelativePath:    relPath,
	}, nil
}

// identityPassThrough reports whether the archived file is already a final
// de-identified product made with THIS key. In that case export rewrites
// nothing identity-bearing (it still normalises meta and keeps the marker),
// which makes re-export byte-stable and guarantees no second date shift.

type originalValues struct {
	patientID   string
	patientName string
	sop         string
	forbidden   map[string]bool // values forbidden in identity/uid fields
	origDates   map[string]bool // original DA values forbidden in DA fields
}

// buildPlan constructs the per-instance rewrite plan from original metadata,
// its full textual scan, and the already-computed pseudo values.
func (s *Store) buildPlan(m *dicom.Meta, inst Instance, texts []dicom.TextValue) (dicom.Plan, originalValues, error) {
	uids := map[string]string{
		deident.NormalizeUID(m.SOPInstanceUID):    inst.SOPUID,
		deident.NormalizeUID(m.StudyInstanceUID):  inst.StudyUID,
		deident.NormalizeUID(m.SeriesInstanceUID): inst.SeriesUID,
	}
	origSOPForMarker := deident.NormalizeUID(m.SOPInstanceUID)
	if m.DeidentFingerprint != "" && m.DeidentOriginalUID != "" {
		// Re-export of our own de-identified product: provenance already
		// records the genuine original SOP; preserve it.
		origSOPForMarker = deident.NormalizeUID(m.DeidentOriginalUID)
	}

	forbidden := map[string]bool{}
	origDates := map[string]bool{}
	addForbidden := func(v, pseudo string) {
		if v != "" && v != pseudo {
			forbidden[v] = true
		}
	}
	addForbidden(m.PatientID, inst.PatientID)
	addForbidden(m.PatientName, inst.PatientName)

	// Any UID-valued element in the source file either matches one of the
	// hierarchy UIDs (mapped) or is an unrelated UID left untouched. To make
	// the residual check meaningful, every observed UI value that maps is
	// forbidden in the output, and original DA values are forbidden too.
	for _, tv := range texts {
		if tv.Value == "" {
			continue
		}
		if dicom.DateTags[tv.Tag] && len(tv.Value) == 8 && m.DeidentFingerprint != s.fingerprint {
			origDates[tv.Value] = true
		}
	}
	for o := range uids {
		// Re-exporting an already-deidentified archive: the "original" is
		// itself a pseudonym and maps to itself. Only genuinely different
		// originals can leak.
		if o != uids[o] {
			forbidden[o] = true
		}
	}

	plan := dicom.Plan{
		Ident: dicom.Identity{
			OrigPatientID:   m.PatientID,
			OrigPatientName: m.PatientName,
			PseudoPatientID: inst.PatientID,
			PseudoName:      inst.PatientName,
			UIDs:            uids,
		},
		ShiftDA: func(da string) string {
			if m.DeidentFingerprint == s.fingerprint {
				return da // already final; never shift a second time
			}
			return s.mapper.ShiftDate(m.PatientID, da)
		},
		Fingerprint:    s.fingerprint,
		OriginalSOPUID: origSOPForMarker,
		Method:         deident.MethodVersion,
	}

	ov := originalValues{
		patientID:   m.PatientID,
		patientName: m.PatientName,
		sop:         deident.NormalizeUID(m.SOPInstanceUID),
		forbidden:   forbidden,
		origDates:   origDates,
	}
	return plan, ov, nil
}

// checkNoResidual scans the rewritten file for any original identity value.
// Matching is exact-value against textual metadata fields (PN/LO/DA/UI and
// any small text field), so unrelated coincidental substrings never trigger.
func (s *Store) checkNoResidual(path string, ov originalValues) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	texts, err := dicom.ScanMetadata(bufio.NewReaderSize(f, streamBufSize))
	if err != nil {
		return fmt.Errorf("%w: rescan %s: %w", ErrParse, path, err)
	}
	for _, tv := range texts {
		v := tv.Value
		if v == "" {
			continue
		}
		if tv.Tag == dicom.TagDeidentOriginalSOPUID {
			continue // provenance marker intentionally records it
		}
		if tv.Depth < 0 {
			continue // file meta group: rewritten and reconciled by the writer
		}
		bad := false
		if dicom.DateTags[tv.Tag] {
			bad = ov.origDates[v]
		} else if tv.Tag.Group == 0x0002 || isTextTag(tv.Tag) {
			bad = ov.forbidden[v]
		}
		if bad {
			return fmt.Errorf("%w: file %s tag %s still contains %q",
				ErrResidualIdentity, path, tagLabel(tv.Tag), v)
		}
	}
	return nil
}

// isTextTag reports tags whose value is textual identity metadata (PN/LO/UI
// and related person/identifier fields). UID fields are checked separately
// via ScanMetadata's tag info at the caller.
func isTextTag(t dicom.Tag) bool {
	switch t {
	case dicom.TagPatientID, dicom.TagPatientName,
		dicom.TagSOPInstanceUID, dicom.TagStudyInstanceUID, dicom.TagSeriesInstanceUID,
		dicom.TagMediaStorageSOPInstance:
		return true
	}
	return false
}
func tagLabel(t dicom.Tag) string {
	return fmt.Sprintf("(%04X,%04X)", t.Group, t.Element)
}

// publishFile atomically installs stagePath as dst. Identical existing
// content is kept untouched; differing content is an error.
func publishFile(stagePath, dst string) error {
	if existing, err := os.ReadFile(dst); err == nil {
		want, err := os.ReadFile(stagePath)
		if err != nil {
			return err
		}
		if bytes.Equal(existing, want) {
			return nil // idempotent rerun
		}
		return fmt.Errorf("%w: %s", ErrExportContent, dst)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.Rename(stagePath, dst)
}

func publishBytes(dst string, want []byte) error {
	if existing, err := os.ReadFile(dst); err == nil {
		if bytes.Equal(existing, want) {
			return nil
		}
		return fmt.Errorf("%w: %s", ErrExportContent, dst)
	} else if !os.IsNotExist(err) {
		return err
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, want, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
