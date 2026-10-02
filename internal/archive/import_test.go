package archive_test

import (
	"bytes"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/lechandonga/dicom-deident-archive-go/internal/archive"
	"github.com/lechandonga/dicom-deident-archive-go/internal/dicom"
	"github.com/lechandonga/dicom-deident-archive-go/internal/sample"
)

func TestImportRejectsAreDistinguishable(t *testing.T) {
	a := openTestArchive(t)
	dir := t.TempDir()

	files := map[string][]byte{
		"truncated.dcm": sample.Build(sample.Options{PatientID: "P",
			StudyUID: "1", SeriesUID: "1.1", SOPUID: "1.1.1",
			StudyDate: "20200101", TruncateBytes: 50}),
		"nomagic.dcm": sample.Build(sample.Options{PatientID: "P",
			StudyUID: "1", SeriesUID: "1.1", SOPUID: "1.1.1",
			BreakPreamble: true}),
		"nopid.dcm": sample.Build(sample.Options{StudyUID: "1",
			SeriesUID: "1.1", SOPUID: "1.1.1", StudyDate: "20200101",
			DropPatientID: true}),
		"baddate.dcm": sample.Build(sample.Options{PatientID: "P",
			StudyUID: "1", SeriesUID: "1.1", SOPUID: "1.1.1",
			BadDate: "2020-13-40"}),
	}
	got := map[string]string{}
	for name, b := range files {
		p := writeFile(t, dir, name, b)
		out := a.ImportFile(p)
		t.Logf("INPUT file=%s -> accepted=%v reason=%s basis=%s",
			name, out.Accepted, out.Reason, out.Detail)
		if out.Accepted {
			t.Fatalf("%s should be rejected", name)
		}
		got[name] = out.Reason
	}
	want := map[string]string{
		"truncated.dcm": dicom.ReasonTruncated,
		"nomagic.dcm":   dicom.ReasonNotDICOM,
		"nopid.dcm":     dicom.ReasonMissingPatientID,
		"baddate.dcm":   dicom.ReasonInvalidDate,
	}
	for name, reason := range want {
		if got[name] != reason {
			t.Errorf("%s: reason=%s want %s", name, got[name], reason)
		}
	}
	if a.InstanceCount() != 0 {
		t.Fatalf("rejected files must not be indexed, count=%d", a.InstanceCount())
	}
}

func TestDuplicateImportIsIdempotent(t *testing.T) {
	a := openTestArchive(t)
	dir := t.TempDir()
	b := validInstance("PID-A", "9.1.1", "9.1.1.1", "9.1.1.1.1", "20200301", "CT")
	p1 := writeFile(t, dir, "a.dcm", b)
	p2 := writeFile(t, dir, "nested/copy.dcm", b)

	o1 := a.ImportFile(p1)
	if !o1.Accepted || o1.Idempotent {
		t.Fatalf("first import accepted=%v idempotent=%v", o1.Accepted, o1.Idempotent)
	}
	o2 := a.ImportFile(p2)
	t.Logf("INPUT identical re-import file=nested/copy.dcm -> accepted=%v "+
		"idempotent=%v reason=%s basis=sha256 content match",
		o2.Accepted, o2.Idempotent, o2.Reason)
	if !o2.Accepted || !o2.Idempotent {
		t.Fatalf("identical re-import must be idempotent: %+v", o2)
	}
	if o2.Instance.SOPUID != o1.Instance.SOPUID {
		t.Fatal("idempotent import resolved to a different instance")
	}
	if a.InstanceCount() != 1 {
		t.Fatalf("idempotent import created a duplicate copy, count=%d",
			a.InstanceCount())
	}
	o3 := a.ImportFile(p1)
	if !o3.Idempotent || o3.Instance.SOPUID != o1.Instance.SOPUID {
		t.Fatal("rerun drifted")
	}
}

func TestSameSOPDifferentContentRejected(t *testing.T) {
	a := openTestArchive(t)
	dir := t.TempDir()

	first := validInstance("PID-A", "9.1.1", "9.1.1.1", "9.1.1.1.1",
		"20200301", "CT")
	conflict := sample.Build(sample.Options{PatientID: "PID-A",
		StudyUID: "9.1.1", SeriesUID: "9.1.1.1", SOPUID: "9.1.1.1.1",
		Modality: "CT", StudyDate: "20200301", Rows: 8, Cols: 8})

	p1 := writeFile(t, dir, "v1.dcm", first)
	p2 := writeFile(t, dir, "v2.dcm", conflict)

	if o := a.ImportFile(p1); !o.Accepted {
		t.Fatalf("first import rejected: %s", o.Detail)
	}
	o := a.ImportFile(p2)
	t.Logf("INPUT same SOP 9.1.1.1.1 with different pixels -> accepted=%v "+
		"reason=%s basis=sha mismatch on same de-identified SOP",
		o.Accepted, o.Reason)
	if o.Accepted || o.Reason != dicom.ReasonDuplicateInstance {
		t.Fatalf("expected DUPLICATE_INSTANCE_CONFLICT, got %+v", o)
	}
	if a.InstanceCount() != 1 {
		t.Fatal("conflicting import must not add a second instance")
	}
}

func TestOriginalIsPreservedAndPixelsUnchanged(t *testing.T) {
	a := openTestArchive(t)
	dir := t.TempDir()
	b := validInstance("PID-A", "9.1.1", "9.1.1.1", "9.1.1.1.1", "20200301", "CT")
	p := writeFile(t, dir, "a.dcm", b)
	o := a.ImportFile(p)
	if !o.Accepted {
		t.Fatal(o.Detail)
	}
	root := rootDir(a)

	stored, err := readFile(filepath.Join(root, o.Instance.OriginalFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, b) {
		t.Fatal("archived original differs byte-for-byte from the source file")
	}
	deid, err := readFile(filepath.Join(root, o.Instance.DeidentifiedFile))
	if err != nil {
		t.Fatal(err)
	}
	origDS, err := dicom.ParseBytes(b, "orig")
	if err != nil {
		t.Fatal(err)
	}
	deidDS, err := dicom.ParseBytes(deid, "deid")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(origDS.Find(dicom.TagPixelData).Raw,
		deidDS.Find(dicom.TagPixelData).Raw) {
		t.Fatal("pixel payload changed in the de-identified copy")
	}
	if deidDS.Find(dicom.TagPatientID).First() == "PID-A" {
		t.Fatal("de-identified copy still contains the original patient ID")
	}
}

func TestQueryFiltersAndStablePagination(t *testing.T) {
	a := openTestArchive(t)
	dir := t.TempDir()
	for i := 0; i < 6; i++ {
		pid, mod := "PID-A", "CT"
		if i >= 3 {
			pid = "PID-B"
		}
		if i == 5 {
			mod = "MR"
		}
		b := validInstance(pid,
			fmt.Sprintf("9.%d.1", i), fmt.Sprintf("9.%d.1.1", i),
			fmt.Sprintf("9.%d.1.1.1", i), "20200301", mod)
		if o := a.ImportFile(writeFile(t, dir, fmt.Sprintf("f%d.dcm", i), b)); !o.Accepted {
			t.Fatalf("import %d: %s", i, o.Detail)
		}
	}

	if page := a.Search(archive.Query{Modality: "CT"}); page.Total != 5 {
		t.Fatalf("CT total = %d, want 5", page.Total)
	}
	if page := a.Search(archive.Query{Modality: "mr"}); page.Total != 1 {
		t.Fatalf("case-insensitive MR total = %d", page.Total)
	}
	if page := a.Search(archive.Query{StudyDateFrom: "20200101",
		StudyDateTo: "20210101"}); page.Total != 6 {
		t.Fatalf("date range total = %d, want 6", page.Total)
	}

	full := a.Search(archive.Query{Limit: 100})
	var paged []*archive.Instance
	for off := 0; ; off += 2 {
		page := a.Search(archive.Query{Offset: off, Limit: 2})
		if len(page.Instances) == 0 {
			break
		}
		paged = append(paged, page.Instances...)
	}
	if len(paged) != full.Total {
		t.Fatalf("paginated count=%d full=%d", len(paged), full.Total)
	}
	seen := map[string]bool{}
	for i, in := range paged {
		if in.SOPUID != full.Instances[i].SOPUID {
			t.Fatalf("pagination order mismatch at %d", i)
		}
		if seen[in.SOPUID] {
			t.Fatalf("duplicate page entry %s", in.SOPUID)
		}
		seen[in.SOPUID] = true
	}
}
