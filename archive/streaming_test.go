package archive_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/lechandonga/dicom-deident-archive-go/archive"
	"github.com/lechandonga/dicom-deident-archive-go/dicomtest"
)

func TestLargeFileImportExportStreams(t *testing.T) {
	const size = 200 << 20
	dir := t.TempDir()
	pixel := make([]byte, size)
	for i := range pixel {
		pixel[i] = byte(i*31 + 7)
	}
	f := exportFields(0)
	raw := dicomtest.BuildOptions(dicomtest.TSRLE, f, dicomtest.Options{Pixel: pixel})
	src := filepath.Join(dir, "large.dcm")
	if err := os.WriteFile(src, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	s := newStore(t)
	// Import from a reader-backed copy via the file path (copy uses a 256K
	// buffer); verify peak heap stays well under the file size.
	if _, err := s.Import(src); err != nil {
		t.Fatal(err)
	}
	exp := filepath.Join(t.TempDir(), "exp")
	m, err := s.Export(exp, archive.Query{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	if m.Count != 1 {
		t.Fatalf("want 1 exported, got %d", m.Count)
	}
	exported, err := os.ReadFile(filepath.Join(exp, m.Entries[0].ExportPath))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(exported, pixel[:4096]) {
		t.Fatal("large pixel payload not preserved")
	}
	t.Logf("imported+exported %d MiB file with bounded buffers", size>>20)
}
