package archive_test

import (
	"bytes"
	"path/filepath"
	"sync"
	"testing"

	"github.com/lechandonga/dicom-deident-archive-go/archive"
	"github.com/lechandonga/dicom-deident-archive-go/deident"
	"github.com/lechandonga/dicom-deident-archive-go/dicomtest"
)

func TestReimportSameKeyNoSecondShift(t *testing.T) {
	s := newStore(t)
	dir := t.TempDir()
	importVariant(t, s, dir, "a.dcm", dicomtest.TSJPEGLossless, exportFields(0),
		dicomtest.Options{UndefLengthSequence: true, Pixel: bytes.Repeat([]byte{0x42}, 100)})

	exp := filepath.Join(t.TempDir(), "exp")
	m, err := s.Export(exp, archive.Query{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}

	arc2, err := archive.Open(t.TempDir(), deident.Mapper{Salt: "test-salt", UIDRoot: "2.25.999"})
	if err != nil {
		t.Fatal(err)
	}
	var imported []archive.Instance
	for _, e := range m.Entries {
		r, err := arc2.Import(filepath.Join(exp, e.ExportPath))
		if err != nil {
			t.Fatal(err)
		}
		if !r.Instance.ReImported {
			t.Fatalf("%s not recognised as same-key reimport", e.ExportPath)
		}
		imported = append(imported, *r.Instance)
	}
	for i, e := range m.Entries {
		got := imported[i]
		if got.PatientID != e.PatientID || got.StudyUID != e.StudyUID ||
			got.SeriesUID != e.SeriesUID || got.SOPUID != e.SOPUID || got.StudyDate != e.StudyDate {
			t.Fatalf("reimport shifted values again:\n manifest=%+v\n got=%+v", e, got)
		}
	}
	page, _ := s.Query(archive.Query{Limit: 500})
	page2, _ := arc2.Query(archive.Query{Limit: 500})
	if len(page.Instances) != len(page2.Instances) {
		t.Fatalf("instance set differs: %d vs %d", len(page.Instances), len(page2.Instances))
	}
	for i := range page.Instances {
		a, b := page.Instances[i], page2.Instances[i]
		if a.SOPUID != b.SOPUID || a.StudyDate != b.StudyDate {
			t.Fatalf("query result mismatch: %+v vs %+v", a, b)
		}
	}
	t.Logf("same-key reimport preserves pseudonyms: pid=%s date=%s",
		imported[0].PatientID, imported[0].StudyDate)
}

func TestReimportDifferentKeyRemapsConsistently(t *testing.T) {
	s := newStore(t)
	dir := t.TempDir()
	importVariant(t, s, dir, "a.dcm", dicomtest.TSExplicitLE, exportFields(0),
		dicomtest.Options{DefinedLengthSequence: true})
	exp := filepath.Join(t.TempDir(), "exp")
	m, err := s.Export(exp, archive.Query{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}

	// Different key: must import cleanly and produce a fresh consistent set.
	other, err := archive.Open(t.TempDir(), deident.Mapper{Salt: "other-secret", UIDRoot: "2.25.999"})
	if err != nil {
		t.Fatal(err)
	}
	var first string
	for _, e := range m.Entries {
		r, err := other.Import(filepath.Join(exp, e.ExportPath))
		if err != nil {
			t.Fatalf("foreign-key reimport must succeed: %v", err)
		}
		if r.Instance.ReImported {
			t.Fatal("foreign-key file must not be marked as same-key reimport")
		}
		if r.Instance.PatientID == e.PatientID {
			t.Fatalf("foreign key must yield a different patient pseudonym, both %s", e.PatientID)
		}
		if first == "" {
			first = r.Instance.PatientID
		} else if r.Instance.PatientID != first {
			t.Fatal("patient pseudonym inconsistent across files under new key")
		}
		t.Logf("foreign-key remap: %s -> %s, date=%s", e.PatientID, r.Instance.PatientID, r.Instance.StudyDate)
	}
}

func TestConcurrentExportImportQuery(t *testing.T) {
	s := newStore(t)
	dir := t.TempDir()
	const n = 24
	paths := make([]string, n)
	for i := 0; i < n; i++ {
		f := exportFields(i)
		paths[i] = dicomtest.WriteFileOptions(dir, f.SOPUID+".dcm",
			dicomtest.TSExplicitLE, f, dicomtest.Options{})
	}

	var wg sync.WaitGroup
	errs := make(chan error, n*3)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			if _, err := s.Import(p); err != nil {
				errs <- err
			}
		}(paths[i])
	}
	stop := make(chan struct{})
	var readers sync.WaitGroup
	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				page, err := s.Query(archive.Query{Limit: 500})
				if err != nil {
					errs <- err
					return
				}
				seen := map[string]bool{}
				for _, inst := range page.Instances {
					if seen[inst.SOPUID] {
						errs <- bytesErr("duplicate visible: " + inst.SOPUID)
						return
					}
					seen[inst.SOPUID] = true
				}
			}
		}()
	}
	// Exporters must see a consistent snapshot and never half instances.
	for ex := 0; ex < 3; ex++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			exp := filepath.Join(t.TempDir(), "exp")
			m, err := s.Export(exp, archive.Query{Limit: 500})
			if err != nil {
				errs <- err
				return
			}
			page, _ := s.Query(archive.Query{Limit: 500})
			// Export count must never exceed the visible total (snapshot).
			if m.Count > len(page.Instances) {
				errs <- bytesErr("exported more than visible instances")
			}
		}()
	}
	wg.Wait()
	close(stop)
	readers.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	page, _ := s.Query(archive.Query{Limit: 500})
	if len(page.Instances) != n {
		t.Fatalf("want %d instances, got %d", n, len(page.Instances))
	}
}

type strErr string

func (e strErr) Error() string { return string(e) }

func bytesErr(s string) error { return strErr(s) }
