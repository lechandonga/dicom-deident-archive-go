package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/lechandonga/dicom-deident-archive-go/archive"
	"github.com/lechandonga/dicom-deident-archive-go/deident"
	"github.com/lechandonga/dicom-deident-archive-go/dicom"
	"github.com/lechandonga/dicom-deident-archive-go/dicomtest"
)

// TestGenSampleOutputsAreStrictlyValid reproduces the compliance failure:
// every file produced by `gensample` (regular rotation + -variants edge
// cases) must survive a strict whole-file ParseVerify, and every exported
// delivery file must survive it too. Previously the generator emitted
// explicit-VR OW pixel headers in the 16-bit short form, which strict
// parsers reject as length overrun / structural damage, while our own
// import accepted them.
func TestGenSampleOutputsAreStrictlyValid(t *testing.T) {
	out := filepath.Join(t.TempDir(), "sample")
	if err := cmdGenSample([]string{"-out", out, "-n", "12", "-variants"}); err != nil {
		t.Fatal(err)
	}

	files := 0
	err := filepath.WalkDir(out, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		meta, verr := dicom.ParseVerify(bytes.NewReader(raw))
		if verr != nil {
			t.Fatalf("input=%s conclusion=strict ParseVerify failed: %v", filepath.Base(p), verr)
		}
		if meta.PatientID == "" || meta.SOPInstanceUID == "" {
			t.Fatalf("%s missing identity metadata", p)
		}
		files++
		t.Logf("input=%s (%d bytes) ts=%s conclusion=strict parse OK",
			filepath.Base(p), len(raw), meta.TransferSyntaxUID)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files != 12+5 {
		t.Fatalf("want 17 samples (12 rotated + 5 variants), got %d", files)
	}

	// Every sample imports and re-exports; every delivery file passes the
	// same strict verification, and encapsulated pixels are byte-identical.
	store, err := archive.Open(t.TempDir(), deident.Mapper{Salt: "k", UIDRoot: "2.25.999"})
	if err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(out, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if _, ierr := store.Import(p); ierr != nil {
			t.Fatalf("import %s: %v", filepath.Base(p), ierr)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	delivery := filepath.Join(t.TempDir(), "delivery")
	m, err := store.Export(delivery, archive.Query{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	if m.Count != files {
		t.Fatalf("export completeness: want %d, got %d", files, m.Count)
	}
	for _, e := range m.Entries {
		raw, err := os.ReadFile(filepath.Join(delivery, e.ExportPath))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := dicom.ParseVerify(bytes.NewReader(raw)); err != nil {
			t.Fatalf("exported %s fails strict parse: %v", e.ExportPath, err)
		}
		if e.TransferSyntax == dicomtest.TSJPEGLossless || e.TransferSyntax == dicomtest.TSRLE {
			if !bytes.Contains(raw, dicomtest.DeterministicPixel()[:4096]) {
				t.Fatalf("exported %s altered encapsulated pixel", e.ExportPath)
			}
		}
		t.Logf("export %s (%d bytes) ts=%s conclusion=strict parse OK",
			e.ExportPath, len(raw), e.TransferSyntax)
	}

	// Re-importing with the same salt and exporting again must still yield
	// strictly valid, stable files.
	rebuilt, err := archive.Open(t.TempDir(), deident.Mapper{Salt: "k", UIDRoot: "2.25.999"})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range m.Entries {
		if _, err := rebuilt.Import(filepath.Join(delivery, e.ExportPath)); err != nil {
			t.Fatal(err)
		}
	}
	delivery2 := filepath.Join(t.TempDir(), "delivery2")
	m2, err := rebuilt.Export(delivery2, archive.Query{Limit: 500})
	if err != nil {
		t.Fatalf("rebuilt re-export: %v", err)
	}
	if m2.Count != m.Count {
		t.Fatalf("rebuilt count drift: %d vs %d", m2.Count, m.Count)
	}
	for _, e := range m2.Entries {
		raw, err := os.ReadFile(filepath.Join(delivery2, e.ExportPath))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := dicom.ParseVerify(bytes.NewReader(raw)); err != nil {
			t.Fatalf("rebuilt export %s fails strict parse: %v", e.ExportPath, err)
		}
	}
	t.Logf("conclusion: %d samples, %d first-export, %d rebuilt-export all strict-valid",
		files, m.Count, m2.Count)
}
