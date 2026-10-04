package archive_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/lechandonga/dicom-deident-archive-go/archive"
	"github.com/lechandonga/dicom-deident-archive-go/deident"
	"github.com/lechandonga/dicom-deident-archive-go/dicom"
	"github.com/lechandonga/dicom-deident-archive-go/dicomtest"
)

// TestRebuiltArchiveReExportIsStable covers the real rebuild workflow:
// export from archive A, import the export dir into an empty archive B with
// the SAME salt, then export from B again.
//
// Requirements verified:
//   - re-export from B succeeds (previously failed with ErrResidualIdentity);
//   - every exported file is byte-identical to A's delivery;
//   - patient/study/series/instance UIDs and dates match the original
//     manifest entry by entry;
//   - the second export from B into the same dir is idempotent;
//   - encapsulated compressed pixels survive byte-for-byte.
func TestRebuiltArchiveReExportIsStable(t *testing.T) {
	src := newStore(t)
	dir := t.TempDir()
	pixel := bytes.Repeat([]byte{0x6C}, 8192)
	cases := []struct {
		name string
		ts   string
		opt  dicomtest.Options
		f    dicomtest.Fields
	}{
		{"implicit.dcm", dicomtest.TSImplicitLE, dicomtest.Options{}, exportFields(0)},
		{"explicit.dcm", dicomtest.TSExplicitLE, dicomtest.Options{UndefLengthSequence: true}, exportFields(1)},
		{"bigendian.dcm", dicomtest.TSExplicitBE, dicomtest.Options{DefinedLengthSequence: true}, exportFields(2)},
		{"jpeg.dcm", dicomtest.TSJPEGLossless, dicomtest.Options{Pixel: pixel, UndefLengthSequence: true}, exportFields(3)},
		{"rle.dcm", dicomtest.TSRLE, dicomtest.Options{Pixel: pixel}, exportFields(4)},
	}
	for _, c := range cases {
		importVariant(t, src, dir, c.name, c.ts, c.f, c.opt)
	}

	deliv1 := filepath.Join(t.TempDir(), "delivery")
	m1, err := src.Export(deliv1, archive.Query{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	if m1.Count != len(cases) {
		t.Fatalf("want %d exported, got %d", len(cases), m1.Count)
	}

	rebuilt, err := archive.Open(t.TempDir(), deident.Mapper{Salt: "test-salt", UIDRoot: "2.25.999"})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range m1.Entries {
		r, err := rebuilt.Import(filepath.Join(deliv1, e.ExportPath))
		if err != nil {
			t.Fatalf("reimport %s: %v", e.ExportPath, err)
		}
		if !r.Instance.ReImported {
			t.Fatalf("%s not recognized as same-salt reimport", e.ExportPath)
		}
	}

	// Re-export from the rebuilt archive: must succeed (was the regression).
	deliv2 := filepath.Join(t.TempDir(), "delivery2")
	m2, err := rebuilt.Export(deliv2, archive.Query{Limit: 500})
	if err != nil {
		t.Fatalf("rebuilt archive re-export failed: %v", err)
	}
	if m2.Count != m1.Count {
		t.Fatalf("count drift: %d vs %d", m2.Count, m1.Count)
	}

	byPath := map[string]archive.ManifestEntry{}
	for _, e := range m2.Entries {
		byPath[e.ExportPath] = e
	}
	for _, e1 := range m1.Entries {
		e2, ok := byPath[e1.ExportPath]
		if !ok {
			t.Fatalf("entry missing in rebuilt export: %s", e1.ExportPath)
		}
		// Every de-identified field must line up, entry by entry.
		if e2.PatientID != e1.PatientID || e2.PatientName != e1.PatientName ||
			e2.StudyUID != e1.StudyUID || e2.SeriesUID != e1.SeriesUID ||
			e2.SOPUID != e1.SOPUID || e2.StudyDate != e1.StudyDate ||
			e2.Modality != e1.Modality || e2.TransferSyntax != e1.TransferSyntax {
			t.Fatalf("rebuilt manifest drift for %s:\n orig=%+v\n rebuilt=%+v",
				e1.ExportPath, e1, e2)
		}
		if e2.ExportSHA256 != e1.ExportSHA256 {
			t.Fatalf("export hash drift for %s: %s vs %s",
				e1.ExportPath, e1.ExportSHA256, e2.ExportSHA256)
		}

		a, err := os.ReadFile(filepath.Join(deliv1, e1.ExportPath))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(deliv2, e2.ExportPath))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Fatalf("rebuilt re-export bytes differ for %s", e1.ExportPath)
		}
		// The re-exported file must itself pass strict full verification
		// and must carry the same pseudonyms, not a new layer of them.
		m, err := dicom.ParseVerify(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("%s fails ParseVerify: %v", e2.ExportPath, err)
		}
		if m.PatientID != e2.PatientID || m.StudyInstanceUID != e2.StudyUID ||
			m.SeriesInstanceUID != e2.SeriesUID || m.SOPInstanceUID != e2.SOPUID ||
			m.StudyDate != e2.StudyDate {
			t.Fatalf("%s pseudonyms diverged on second export", e2.ExportPath)
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != e2.ExportSHA256 {
			t.Fatalf("%s hash mismatch", e2.ExportPath)
		}
		t.Logf("OK %s pid=%s date=%s ts=%s sha=%s",
			e2.ExportPath, e2.PatientID, e2.StudyDate, e2.TransferSyntax, e2.ExportSHA256[:12])
	}

	// Encapsulated pixel bytes remain untouched across the double hop.
	for _, e := range m2.Entries {
		if e.TransferSyntax != dicomtest.TSJPEGLossless && e.TransferSyntax != dicomtest.TSRLE {
			continue
		}
		b, err := os.ReadFile(filepath.Join(deliv2, e.ExportPath))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(b, pixel) {
			t.Fatalf("%s lost encapsulated pixel payload", e.ExportPath)
		}
	}

	// Idempotence: export again into the same directory; nothing changes.
	manifestBefore, err := os.ReadFile(filepath.Join(deliv2, archive.ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rebuilt.Export(deliv2, archive.Query{Limit: 500}); err != nil {
		t.Fatalf("second rebuilt export must be idempotent: %v", err)
	}
	manifestAfter, err := os.ReadFile(filepath.Join(deliv2, archive.ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	var mb, ma archive.Manifest
	if err := json.Unmarshal(manifestBefore, &mb); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(manifestAfter, &ma); err != nil {
		t.Fatal(err)
	}
	if len(mb.Entries) != len(ma.Entries) {
		t.Fatalf("idempotent export changed entry count")
	}
	for i := range mb.Entries {
		if mb.Entries[i].ExportSHA256 != ma.Entries[i].ExportSHA256 ||
			mb.Entries[i].ExportPath != ma.Entries[i].ExportPath {
			t.Fatalf("idempotent rebuilt export drifted: %+v vs %+v",
				mb.Entries[i], ma.Entries[i])
		}
	}
	t.Logf("rebuilt re-export stable and idempotent across %d instances", m2.Count)
}

// TestRebuiltImportRejectsTruncatedDelivery makes sure the archive-level
// import of the rebuilt delivery set rejects pixel-truncated files with the
// distinct parse error and a loggable source path.
func TestRebuiltImportRejectsTruncatedDelivery(t *testing.T) {
	src := newStore(t)
	dir := t.TempDir()
	importVariant(t, src, dir, "a.dcm", dicomtest.TSExplicitLE, exportFields(0),
		dicomtest.Options{Pixel: bytes.Repeat([]byte{0x22}, 5000)})
	deliv := filepath.Join(t.TempDir(), "d")
	if _, err := src.Export(deliv, archive.Query{Limit: 500}); err != nil {
		t.Fatal(err)
	}

	bad := filepath.Join(t.TempDir(), "bad.dcm")
	good, err := os.ReadFile(filepath.Join(deliv, findDCM(t, deliv)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, good[:len(good)-2], 0o644); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := archive.Open(t.TempDir(), deident.Mapper{Salt: "test-salt", UIDRoot: "2.25.999"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = rebuilt.Import(bad)
	if !errors.Is(err, archive.ErrParse) || !errors.Is(err, dicom.ErrTruncated) {
		t.Fatalf("truncated delivery into rebuilt: want ErrParse+ErrTruncated, got %v", err)
	}
	t.Logf("input=%s conclusion=rejected %v", bad, err)
}

func findDCM(t *testing.T, root string) string {
	t.Helper()
	var found string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && filepath.Ext(p) == ".dcm" {
			rel, _ := filepath.Rel(root, p)
			found = rel
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found == "" {
		t.Fatal("no dcm in delivery")
	}
	return found
}
