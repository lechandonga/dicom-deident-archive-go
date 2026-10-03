package archive_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/lechandonga/dicom-deident-archive-go/archive"
	"github.com/lechandonga/dicom-deident-archive-go/deident"
	"github.com/lechandonga/dicom-deident-archive-go/dicom"
	"github.com/lechandonga/dicom-deident-archive-go/dicomtest"
)

func exportFields(i int) dicomtest.Fields {
	return dicomtest.Fields{
		PatientID: "REAL-PAT-1", PatientName: "Real^Surname",
		BirthDate: "19800505", Sex: "M",
		StudyUID:     "10.1",
		StudyDate:    fmt.Sprintf("2024%02d10", i%6+1),
		StudyID:      "ST",
		SeriesUID:    fmt.Sprintf("10.2.%d", i%2),
		SeriesNumber: "1",
		Modality:     []string{"CT", "MR", "US"}[i%3],
		SOPUID:       fmt.Sprintf("10.3.%d", i),
		InstanceNo:   fmt.Sprintf("%d", i),
		FrameRefUID:  "10.9.1",
	}
}

func importVariant(t *testing.T, s *archive.Store, dir, name, ts string, f dicomtest.Fields, opt dicomtest.Options) {
	t.Helper()
	p := dicomtest.WriteFileOptions(dir, name, ts, f, opt)
	if _, err := s.Import(p); err != nil {
		t.Fatalf("import %s: %v", name, err)
	}
}

func TestExportDeIdentifiesConsistentlyAndAudits(t *testing.T) {
	s := newStore(t)
	dir := t.TempDir()
	pixel := bytes.Repeat([]byte{0x77}, 4096)
	for i := 0; i < 4; i++ {
		ts := dicomtest.TSExplicitLE
		if i%2 == 1 {
			ts = dicomtest.TSJPEGLossless
		}
		opt := dicomtest.Options{Pixel: pixel}
		if i%2 == 0 {
			opt.UndefLengthSequence = true
		} else {
			opt.DefinedLengthSequence = true
		}
		importVariant(t, s, dir, fmt.Sprintf("f%d.dcm", i), ts, exportFields(i), opt)
	}

	exp := filepath.Join(t.TempDir(), "out")
	m1, err := s.Export(exp, archive.Query{Modality: "CT", Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	if m1.Count == 0 {
		t.Fatal("export matched nothing")
	}
	t.Logf("exported %d CT instances", m1.Count)

	for _, e := range m1.Entries {
		raw, err := os.ReadFile(filepath.Join(exp, e.ExportPath))
		if err != nil {
			t.Fatal(err)
		}
		m, err := dicom.ParseVerify(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("%s does not reparse: %v", e.ExportPath, err)
		}
		if m.PatientID == "REAL-PAT-1" || m.PatientName == "Real^Surname" {
			t.Fatalf("%s keeps original patient values: %s/%s", e.ExportPath, m.PatientID, m.PatientName)
		}
		if m.StudyInstanceUID == "10.1" || m.SOPInstanceUID == "10.3.0" {
			t.Fatalf("%s keeps original UIDs", e.ExportPath)
		}
		if m.StudyDate == "20240110" || m.PatientBirthDate == "19800505" {
			t.Fatalf("%s keeps original dates", e.ExportPath)
		}
		if m.PatientID != e.PatientID || m.PatientName != e.PatientName ||
			m.StudyInstanceUID != e.StudyUID || m.SeriesInstanceUID != e.SeriesUID ||
			m.SOPInstanceUID != e.SOPUID || m.StudyDate != e.StudyDate {
			t.Fatalf("%s pseudonyms diverge from manifest/index", e.ExportPath)
		}
		if v, ok := m.Tags[dicom.Tag{Group: 0x0008, Element: 0x1155}]; ok && v == "10.1" {
			t.Fatalf("%s keeps referenced study UID in sequence", e.ExportPath)
		}
		if v := m.Tags[dicom.Tag{Group: deident.ProvenanceTagGroup, Element: deident.ProvenanceTagElement}]; v != deident.ProvenanceValue {
			t.Fatalf("%s missing provenance marker, got %q", e.ExportPath, v)
		}
		if !bytes.Contains(raw, pixel) {
			t.Fatalf("%s pixel payload changed", e.ExportPath)
		}
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != e.ExportSHA256 {
			t.Fatalf("%s hash mismatch", e.ExportPath)
		}
		t.Logf("OK %s pid=%s date=%s ts=%s", e.ExportPath, e.PatientID, e.StudyDate, e.TransferSyntax)
	}

	pid := m1.Entries[0].PatientID
	for _, e := range m1.Entries {
		if e.PatientID != pid {
			t.Fatalf("patient pseudonym inconsistent: %q vs %q", e.PatientID, pid)
		}
	}
	mapper := deident.Mapper{Salt: "test-salt", UIDRoot: "2.25.999"}
	origBySOP := map[string]string{}
	for i := 0; i < 4; i++ {
		f := exportFields(i)
		origBySOP[mapper.SOPUID(f.SOPUID)] = f.StudyDate
	}
	wantBirth := mapper.ShiftDate("REAL-PAT-1", "19800505")
	for _, e := range m1.Entries {
		origDate := origBySOP[e.SOPUID]
		if mapper.ShiftDate("REAL-PAT-1", origDate) != e.StudyDate {
			t.Fatalf("study date shift mismatch for %s: %s -> %s", e.SOPUID, origDate, e.StudyDate)
		}
		raw, _ := os.ReadFile(filepath.Join(exp, e.ExportPath))
		m, _ := dicom.ParseVerify(bytes.NewReader(raw))
		if m.PatientBirthDate != wantBirth {
			t.Fatalf("birth date shift inconsistent: got %s want %s", m.PatientBirthDate, wantBirth)
		}
	}

	all, err := s.Export(filepath.Join(t.TempDir(), "all"), archive.Query{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	if all.Count != 4 {
		t.Fatalf("full export must include all 4 instances, got %d", all.Count)
	}
}

func TestExportReproducibleIdempotentAndConflictSafe(t *testing.T) {
	s := newStore(t)
	dir := t.TempDir()
	importVariant(t, s, dir, "a.dcm", dicomtest.TSExplicitLE, exportFields(0), dicomtest.Options{})

	exp := filepath.Join(t.TempDir(), "out")
	m1, err := s.Export(exp, archive.Query{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	m2, err := s.Export(exp, archive.Query{Limit: 500})
	if err != nil {
		t.Fatalf("re-export must be idempotent: %v", err)
	}
	for i := range m1.Entries {
		if m1.Entries[i].ExportSHA256 != m2.Entries[i].ExportSHA256 ||
			m1.Entries[i].ExportPath != m2.Entries[i].ExportPath {
			t.Fatalf("export not reproducible:\n%+v\n%+v", m1.Entries[i], m2.Entries[i])
		}
	}

	ma, _ := os.ReadFile(filepath.Join(exp, archive.ManifestName))
	if _, err := s.Export(exp, archive.Query{Limit: 500}); err != nil {
		t.Fatal(err)
	}
	mb, _ := os.ReadFile(filepath.Join(exp, archive.ManifestName))
	if !bytes.Equal(ma, mb) {
		t.Fatal("manifest bytes differ between runs")
	}
	var mf archive.Manifest
	if err := json.Unmarshal(ma, &mf); err != nil {
		t.Fatal(err)
	}

	count := 0
	filepath.WalkDir(exp, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Ext(p) == ".dcm" {
			count++
		}
		return nil
	})
	if count != len(m1.Entries) {
		t.Fatalf("duplicate files: %d dcm for %d entries", count, len(m1.Entries))
	}

	target := filepath.Join(exp, m1.Entries[0].ExportPath)
	orig, _ := os.ReadFile(target)
	if err := os.WriteFile(target, []byte("different-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = s.Export(exp, archive.Query{Limit: 500})
	if !errors.Is(err, archive.ErrExportConflict) {
		t.Fatalf("want ErrExportConflict, got %v", err)
	}
	after, _ := os.ReadFile(target)
	if !bytes.Equal(after, []byte("different-bytes")) {
		t.Fatal("conflicting file must not be overwritten")
	}
	t.Logf("conflict detected: %v", err)
	os.WriteFile(target, orig, 0o644)
	if _, err := s.Export(exp, archive.Query{Limit: 500}); err != nil {
		t.Fatalf("after restoring identical file export must succeed: %v", err)
	}
}

func TestExportLeavesNoStageAfterFailure(t *testing.T) {
	s := newStore(t)
	dir := t.TempDir()
	importVariant(t, s, dir, "a.dcm", dicomtest.TSExplicitLE, exportFields(0), dicomtest.Options{})

	// Corrupt the archived copy after import (simulating mid-run failure).
	var dataFile string
	filepath.WalkDir(filepath.Join(s.Dir(), "data"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Ext(p) == ".dcm" {
			dataFile = p
		}
		return nil
	})
	backup, _ := os.ReadFile(dataFile)
	os.WriteFile(dataFile, backup[:len(backup)/2], 0o644)

	failDir := filepath.Join(t.TempDir(), "out2")
	if _, err := s.Export(failDir, archive.Query{Limit: 500}); err == nil {
		t.Fatal("truncated archive copy must fail export")
	} else {
		t.Logf("failed export as expected: %v", err)
	}
	entries, _ := os.ReadDir(failDir)
	for _, e := range entries {
		if e.Name()[0] == '.' {
			t.Fatalf("leftover staging dir: %s", e.Name())
		}
	}
	os.WriteFile(dataFile, backup, 0o644)
}
