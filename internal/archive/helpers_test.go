package archive_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lechandonga/dicom-deident-archive-go/internal/archive"
	"github.com/lechandonga/dicom-deident-archive-go/internal/config"
	"github.com/lechandonga/dicom-deident-archive-go/internal/sample"
)

func readFile(p string) ([]byte, error) { return os.ReadFile(p) }

func rootDir(a *archive.Archive) string { return filepath.Dir(a.MappingPath()) }

func openTestArchive(t *testing.T) *archive.Archive {
	t.Helper()
	c := config.Default()
	cc, err := c.Compile()
	if err != nil {
		t.Fatal(err)
	}
	a, err := archive.Open(filepath.Join(t.TempDir(), "arch"), cc)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func writeFile(t *testing.T, dir, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func validInstance(pid, study, series, sop, date, modality string) []byte {
	return sample.Build(sample.Options{
		PatientID: pid, PatientName: pid + "^Name", StudyUID: study,
		SeriesUID: series, SOPUID: sop, Modality: modality, StudyDate: date,
		InstanceNumber: sop[len(sop)-1:],
	})
}
