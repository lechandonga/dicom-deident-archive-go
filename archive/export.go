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

	"github.com/lechandonga/dicom-deident-archive-go/deident"
	"github.com/lechandonga/dicom-deident-archive-go/dicom"
)

// Export-specific errors.
var (
	ErrExportConflict   = errors.New("archive: export target contains conflicting file")
	ErrResidualIdentity = errors.New("archive: residual original identity found in export")
)

// ManifestEntry describes one exported instance.
type ManifestEntry struct {
	SourcePath     string `json:"source_path"`
	SourceSHA256   string `json:"source_sha256"`
	ExportPath     string `json:"export_path"`
	ExportSHA256   string `json:"export_sha256"`
	PatientID      string `json:"patient_id"`
	PatientName    string `json:"patient_name"`
	StudyUID       string `json:"study_uid"`
	SeriesUID      string `json:"series_uid"`
	SOPUID         string `json:"sop_uid"`
	Modality       string `json:"modality"`
	StudyDate      string `json:"study_date"`
	TransferSyntax string `json:"transfer_syntax_uid"`
}

// Manifest is the deterministic per-export listing.
type Manifest struct {
	Version  string          `json:"version"`
	SaltHash string          `json:"salt_token"`
	Count    int             `json:"count"`
	Entries  []ManifestEntry `json:"entries"`
}

// ManifestName is the manifest file name inside the export directory.
const ManifestName = "export-manifest.json"

// Export selects every instance matching q and writes a de-identified copy
// of each under dst, plus a manifest. See package docs for the guarantees
// (completeness, reproducibility, idempotence, conflict detection,
// atomicity, read-only source, identity self-audit).
func (s *Store) Export(dst string, q Query) (*Manifest, error) {
	s.mu.RLock()
	snapshot := s.snapshotLocked(q)
	srcRoot := s.dir
	mapper := s.mapper
	s.mu.RUnlock()

	sort.SliceStable(snapshot, func(i, j int) bool {
		return exportRel(snapshot[i]) < exportRel(snapshot[j])
	})

	if err := os.MkdirAll(dst, 0o755); err != nil {
		return nil, err
	}

	stage, err := os.MkdirTemp(dst, ".export-stage-*")
	if err != nil {
		return nil, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(stage)
		}
	}()

	manifest := &Manifest{
		Version:  "1",
		SaltHash: mapper.ProvenanceToken(),
		Count:    0,
		Entries:  []ManifestEntry{},
	}

	for _, inst := range snapshot {
		entry, err := s.exportOne(srcRoot, stage, inst, mapper)
		if err != nil {
			return nil, err
		}
		manifest.Entries = append(manifest.Entries, entry)
		manifest.Count++
	}

	sort.Slice(manifest.Entries, func(i, j int) bool {
		return manifest.Entries[i].ExportPath < manifest.Entries[j].ExportPath
	})

	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(filepath.Join(stage, ManifestName), raw, 0o644); err != nil {
		return nil, err
	}

	for _, e := range manifest.Entries {
		if err := publish(stage, dst, e.ExportPath); err != nil {
			return nil, err
		}
	}
	if err := publish(stage, dst, ManifestName); err != nil {
		return nil, err
	}

	cleanup = false
	if err := os.RemoveAll(stage); err != nil {
		return nil, err
	}
	return manifest, nil
}

func exportRel(inst Instance) string {
	return filepath.Join(inst.PatientID, inst.StudyUID, inst.SeriesUID, inst.SOPUID+".dcm")
}

// exportOne rewrites one archived original into the stage and audits it.
func (s *Store) exportOne(srcRoot, stage string, inst Instance, mapper deident.Mapper) (ManifestEntry, error) {
	src := filepath.Join(srcRoot, inst.Path)
	in, err := os.Open(src)
	if err != nil {
		return ManifestEntry{}, err
	}
	defer in.Close()

	meta, err := dicom.ParseVerify(in)
	if err != nil {
		return ManifestEntry{}, fmt.Errorf("%w: %s: %v", ErrParse, src, err)
	}

	secrets := residualSecrets(meta)
	umap := newUIDMapper(mapper, meta)

	rel := exportRel(inst)
	stagePath := filepath.Join(stage, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(stagePath), 0o755); err != nil {
		return ManifestEntry{}, err
	}
	out, err := os.OpenFile(stagePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return ManifestEntry{}, err
	}
	h := sha256.New()
	mw := io.MultiWriter(out, h)

	spec := dicom.RewriteSpec{
		MapUID:         umap.mapUID,
		MapPatientID:   func(string) string { return inst.PatientID },
		MapPatientName: func(string) string { return inst.PatientName },
		ShiftDate:      func(da string) string { return mapper.ShiftDate(meta.PatientID, da) },
		Markers: []dicom.Marker{
			{Tag: dicom.Tag{Group: deident.ProvenanceTagGroup, Element: deident.ProvenanceTagElement}, VR: "LO", Value: deident.ProvenanceValue},
			{Tag: dicom.Tag{Group: deident.TokenTagGroup, Element: deident.TokenTagElement}, VR: "UC", Value: mapper.ProvenanceToken()},
			{Tag: dicom.Tag{Group: 0x0012, Element: 0x0062}, VR: "CS", Value: "YES"},
			{Tag: dicom.Tag{Group: 0x0012, Element: 0x0063}, VR: "DA", Value: "19700101"},
		},
		AuditSecrets: secrets,
	}
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		out.Close()
		return ManifestEntry{}, err
	}
	res, err := dicom.Rewrite(in, mw, spec)
	if err != nil {
		out.Close()
		return ManifestEntry{}, err
	}
	if err := out.Close(); err != nil {
		return ManifestEntry{}, err
	}
	if len(res.AuditFindings) > 0 {
		msg := ""
		for _, f0 := range res.AuditFindings {
			msg += fmt.Sprintf("\n  %s: %s contains an original value", rel, f0.Tag)
		}
		return ManifestEntry{}, fmt.Errorf("%w: %s%s", ErrResidualIdentity, rel, msg)
	}

	return ManifestEntry{
		SourcePath:     inst.Path,
		SourceSHA256:   inst.SHA256,
		ExportPath:     filepath.ToSlash(rel),
		ExportSHA256:   hex.EncodeToString(h.Sum(nil)),
		PatientID:      inst.PatientID,
		PatientName:    inst.PatientName,
		StudyUID:       inst.StudyUID,
		SeriesUID:      inst.SeriesUID,
		SOPUID:         inst.SOPUID,
		Modality:       inst.Modality,
		StudyDate:      inst.StudyDate,
		TransferSyntax: inst.TransferSyntaxUID,
	}, nil
}

// residualSecrets lists original values that must not survive in exported
// textual headers.
func residualSecrets(meta *dicom.Meta) []string {
	out := []string{}
	add := func(s string) {
		if s != "" {
			out = append(out, s)
		}
	}
	add(meta.PatientID)
	add(meta.PatientName)
	add(meta.StudyInstanceUID)
	add(meta.SeriesInstanceUID)
	add(meta.SOPInstanceUID)
	if deident.IsValidDA(meta.StudyDate) {
		add(meta.StudyDate)
	}
	if deident.IsValidDA(meta.PatientBirthDate) {
		add(meta.PatientBirthDate)
	}
	return out
}

// publish moves a staged relative path into dst; if the target already
// exists it must match byte-for-byte or ErrExportConflict is returned.
func publish(stage, dst, rel string) error {
	from := filepath.Join(stage, filepath.FromSlash(rel))
	to := filepath.Join(dst, filepath.FromSlash(rel))
	if _, err := os.Stat(to); err == nil {
		a, err := fileHash(from)
		if err != nil {
			return err
		}
		b, err := fileHash(to)
		if err != nil {
			return err
		}
		if a != b {
			return fmt.Errorf("%w: %s (exported %s vs existing %s)",
				ErrExportConflict, rel, a[:12], b[:12])
		}
		return os.Remove(from)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	return os.Rename(from, to)
}

func fileHash(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 256*1024)
	if _, err := io.CopyBuffer(h, f, buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
