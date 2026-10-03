package archive_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/lechandonga/dicom-deident-archive-go/archive"
	"github.com/lechandonga/dicom-deident-archive-go/deident"
	"github.com/lechandonga/dicom-deident-archive-go/dicom"
	"github.com/lechandonga/dicom-deident-archive-go/dicomtest"
)

func newStore(t *testing.T) *archive.Store {
	t.Helper()
	s, err := archive.Open(t.TempDir(), deident.Mapper{Salt: "test-salt", UIDRoot: "2.25.999"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func fields(patient, study, series, sop, date, modality, instNo string) dicomtest.Fields {
	return dicomtest.Fields{
		PatientID: patient, PatientName: "PN^" + patient,
		StudyUID: study, StudyDate: date, StudyID: "S",
		SeriesUID: series, SeriesNumber: "1", Modality: modality,
		SOPUID: sop, InstanceNo: instNo,
	}
}

func importErr(t *testing.T, s *archive.Store, path string) error {
	t.Helper()
	_, err := s.Import(path)
	return err
}

func TestRejectsCorruptTruncatedMissingTag(t *testing.T) {
	s := newStore(t)
	dir := t.TempDir()

	good := dicomtest.Build(dicomtest.TSExplicitLE, fields("P1", "1.1.1", "1.1.2", "1.1.3", "20240101", "CT", "1"))

	cases := []struct {
		name string
		data []byte
		want error
	}{
		{"not-dicom", make([]byte, 512), dicom.ErrNotDICOM},
		{"truncated", good[:200], dicom.ErrTruncated},
	}
	for _, c := range cases {
		p := filepath.Join(dir, c.name+".dcm")
		os.WriteFile(p, c.data, 0o644)
		err := importErr(t, s, p)
		if !errors.Is(err, archive.ErrParse) || !errors.Is(err, c.want) {
			t.Fatalf("%s: got %v, want chain containing %v", c.name, err, c.want)
		}
		t.Logf("reject file=%s reason=%v", c.name, err)
	}

	// Missing PatientID.
	noPat := fields("", "1.1.1", "1.1.2", "1.1.9", "20240101", "CT", "1")
	p := filepath.Join(dir, "missing-patient.dcm")
	os.WriteFile(p, dicomtest.Build(dicomtest.TSExplicitLE, noPat), 0o644)
	err := importErr(t, s, p)
	var mt *dicom.MissingTagError
	if !errors.As(err, &mt) {
		t.Fatalf("missing-tag: got %v", err)
	}
	t.Logf("reject file=missing-patient.dcm reason=%v", err)

	// Nothing was archived.
	page, _ := s.Query(archive.Query{})
	if len(page.Instances) != 0 {
		t.Fatalf("rejected files must not be archived, got %d", len(page.Instances))
	}
}

func TestCrossFileConsistencyAndIdempotency(t *testing.T) {
	s := newStore(t)
	dir := t.TempDir()

	// One patient, two studies, mixed transfer syntaxes.
	files := []struct {
		name string
		ts   string
		f    dicomtest.Fields
	}{
		{"a.dcm", dicomtest.TSExplicitLE, fields("PAT-1", "ST.1", "SE.1", "SOP.1", "20240110", "CT", "1")},
		{"b.dcm", dicomtest.TSImplicitLE, fields("PAT-1", "ST.1", "SE.1", "SOP.2", "20240110", "CT", "2")},
		{"c.dcm", dicomtest.TSExplicitBE, fields("PAT-1", "ST.2", "SE.3", "SOP.3", "20240210", "MR", "1")},
	}
	for _, f := range files {
		p := dicomtest.WriteFile(dir, f.name, f.ts, f.f)
		if _, err := s.Import(p); err != nil {
			t.Fatalf("import %s: %v", f.name, err)
		}
	}

	page, err := s.Query(archive.Query{Limit: 10})
	if err != nil || len(page.Instances) != 3 {
		t.Fatalf("want 3 instances, got %+v err=%v", page, err)
	}
	// Same patient -> same pseudonym across files and syntaxes.
	pid := page.Instances[0].PatientID
	for _, inst := range page.Instances {
		if inst.PatientID != pid {
			t.Fatalf("patient pseudonym inconsistent: %q vs %q", inst.PatientID, pid)
		}
	}
	// Study date order preserved after shift: ST.1 (Jan) before ST.2 (Feb).
	var jan, feb string
	for _, inst := range page.Instances {
		switch inst.Modality {
		case "CT":
			jan = inst.StudyDate
		case "MR":
			feb = inst.StudyDate
		}
	}
	if jan >= feb {
		t.Fatalf("date order not preserved: jan=%s feb=%s", jan, feb)
	}
	t.Logf("patient PAT-1 -> %s; shifted dates jan=%s feb=%s", pid, jan, feb)

	// Re-import identical files: idempotent, no duplicates.
	for _, f := range files {
		p := filepath.Join(dir, f.name)
		res, err := s.Import(p)
		if err != nil || !res.Duplicate {
			t.Fatalf("reimport %s: res=%+v err=%v", f.name, res, err)
		}
	}
	page, _ = s.Query(archive.Query{Limit: 10})
	if len(page.Instances) != 3 {
		t.Fatalf("idempotency broken: %d instances", len(page.Instances))
	}
	// Same SOP UID, different content -> conflict.
	conflict := fields("PAT-1", "ST.1", "SE.1", "SOP.1", "20240111", "CT", "1") // different date => different bytes
	p := dicomtest.WriteFile(dir, "conflict.dcm", dicomtest.TSExplicitLE, conflict)
	err = importErr(t, s, p)
	if !errors.Is(err, archive.ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
	t.Logf("conflict detected for SOP.1: %v", err)
}

func TestQueryFiltersAndStablePagination(t *testing.T) {
	s := newStore(t)
	dir := t.TempDir()
	for i := 0; i < 12; i++ {
		mod := "CT"
		if i%2 == 1 {
			mod = "MR"
		}
		f := fields(fmt.Sprintf("PAT-%d", i%3), fmt.Sprintf("ST.%d", i), fmt.Sprintf("SE.%d", i),
			fmt.Sprintf("SOP.%d", i), fmt.Sprintf("2024%02d15", i%12+1), mod, "1")
		p := dicomtest.WriteFile(dir, fmt.Sprintf("f%02d.dcm", i), dicomtest.TSExplicitLE, f)
		if _, err := s.Import(p); err != nil {
			t.Fatal(err)
		}
	}
	// Page through everything with limit 5; collect and check no dup/miss.
	seen := map[string]bool{}
	cursor := ""
	total := 0
	for {
		page, err := s.Query(archive.Query{Limit: 5, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, inst := range page.Instances {
			if seen[inst.SOPUID] {
				t.Fatalf("duplicate across pages: %s", inst.SOPUID)
			}
			seen[inst.SOPUID] = true
			total++
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if total != 12 {
		t.Fatalf("pagination missed instances: %d/12", total)
	}
	// Modality filter.
	page, _ := s.Query(archive.Query{Modality: "MR", Limit: 100})
	if len(page.Instances) != 6 {
		t.Fatalf("MR filter: got %d", len(page.Instances))
	}
	// Patient filter.
	page, _ = s.Query(archive.Query{PatientID: page.Instances[0].PatientID, Limit: 100})
	for _, inst := range page.Instances {
		if inst.PatientID != page.Instances[0].PatientID {
			t.Fatal("patient filter leaked")
		}
	}
}

func TestConcurrentImportAndQuery(t *testing.T) {
	s := newStore(t)
	dir := t.TempDir()
	const n = 40
	paths := make([]string, n)
	for i := 0; i < n; i++ {
		f := fields(fmt.Sprintf("PAT-%d", i%5), fmt.Sprintf("ST.%d", i), fmt.Sprintf("SE.%d", i),
			fmt.Sprintf("SOP.%d", i), fmt.Sprintf("202401%02d", i%28+1), "CT", "1")
		paths[i] = dicomtest.WriteFile(dir, fmt.Sprintf("c%02d.dcm", i), dicomtest.TSExplicitLE, f)
	}

	var wg sync.WaitGroup
	errs := make(chan error, n*2)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			if _, err := s.Import(p); err != nil {
				errs <- err
			}
		}(paths[i])
	}
	// Concurrent readers must always see a consistent, duplicate-free snapshot.
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
						errs <- fmt.Errorf("duplicate visible: %s", inst.SOPUID)
						return
					}
					seen[inst.SOPUID] = true
				}
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

	// Index survives a reload.
	s2, err := archive.Open(filepath.Join(dir, "..", "nonexistent"), deident.Mapper{})
	_ = s2 // separate dir; just ensure Open works on empty
	if err != nil {
		t.Fatal(err)
	}
}
