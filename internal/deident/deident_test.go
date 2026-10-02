package deident_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/lechandonga/dicom-deident-archive-go/internal/config"
	"github.com/lechandonga/dicom-deident-archive-go/internal/deident"
	"github.com/lechandonga/dicom-deident-archive-go/internal/dicom"
	"github.com/lechandonga/dicom-deident-archive-go/internal/sample"
)

func newTestMapping(t *testing.T) *deident.Mapping {
	t.Helper()
	m, err := deident.NewMapping(filepath.Join(t.TempDir(), "mapping.json"),
		"unit-test-salt", "1.2.3.4.5", 365)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func compiled(t *testing.T) *config.Compiled {
	t.Helper()
	c := config.Default()
	cc, err := c.Compile()
	if err != nil {
		t.Fatal(err)
	}
	return cc
}

func TestPatientIDStableAndNoCollision(t *testing.T) {
	m := newTestMapping(t)
	p1a := m.MapPatientID("PID-A")
	p1b := m.MapPatientID("PID-A")
	p2 := m.MapPatientID("PID-B")
	t.Logf("INPUT PID-A -> %s (repeat %s); INPUT PID-B -> %s; basis=HMAC-SHA256(salt)",
		p1a, p1b, p2)
	if p1a != p1b {
		t.Fatalf("same patient drifted: %s vs %s", p1a, p1b)
	}
	if p1a == p2 {
		t.Fatalf("distinct patients collided on %s", p1a)
	}

	// Many distinct originals must remain collision-free.
	seen := map[string]string{}
	for i := 0; i < 2000; i++ {
		orig := "PID-" + string(rune('A'+i/26)) + string(rune('A'+i%26)) + "-" +
			time.Duration(i).String()
		p := m.MapPatientID(orig)
		if other, ok := seen[p]; ok && other != orig {
			t.Fatalf("collision: %s and %s both -> %s", orig, other, p)
		}
		seen[p] = orig
	}
}

func TestMappingPersistenceIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mapping.json")
	m1, err := deident.NewMapping(path, "s", "1.2.3", 365)
	if err != nil {
		t.Fatal(err)
	}
	firstPID := m1.MapPatientID("PID-A")
	firstStudy := m1.MapStudyUID("9.1.1")
	firstShift := m1.DateShift("PID-A")
	if err := m1.Save(); err != nil {
		t.Fatal(err)
	}

	m2, err := deident.NewMapping(path, "s", "1.2.3", 365)
	if err != nil {
		t.Fatal(err)
	}
	if m2.MapPatientID("PID-A") != firstPID {
		t.Fatal("pseudonym drifted across reruns")
	}
	if m2.MapStudyUID("9.1.1") != firstStudy {
		t.Fatal("study UID drifted across reruns")
	}
	if m2.DateShift("PID-A") != firstShift {
		t.Fatal("date shift drifted across reruns")
	}
}

func TestDateShiftPreservesIntraPatientIntervals(t *testing.T) {
	m := newTestMapping(t)
	dates := []string{"20200301", "20200305", "20200601"}
	var shifted []string
	for _, d := range dates {
		s, err := m.ShiftDate("PID-A", d)
		if err != nil {
			t.Fatal(err)
		}
		shifted = append(shifted, s)
	}
	t.Logf("INPUT PID-A dates=%v -> shifted=%v basis=fixed per-patient HMAC offset",
		dates, shifted)
	parse := func(s string) time.Time {
		tt, err := time.Parse("20060102", s)
		if err != nil {
			t.Fatal(err)
		}
		return tt
	}
	for i := 1; i < len(dates); i++ {
		origGap := parse(dates[i]).Sub(parse(dates[i-1]))
		newGap := parse(shifted[i]).Sub(parse(shifted[i-1]))
		if origGap != newGap {
			t.Fatalf("interval %d changed: %v -> %v", i, origGap, newGap)
		}
	}
	if shifted[0] == dates[0] {
		t.Fatal("expected a nonzero shift")
	}

	// Determinism across calls.
	s2, _ := m.ShiftDate("PID-A", dates[0])
	if s2 != shifted[0] {
		t.Fatal("shift not stable")
	}
}

func TestTransformRemovesIdentityAndKeepsPixels(t *testing.T) {
	m := newTestMapping(t)
	cc := compiled(t)
	b := sample.Build(sample.Options{
		PatientID: "PID-A", PatientName: "Zhang^San", StudyUID: "9.1.1",
		SeriesUID: "9.1.1.1", SOPUID: "9.1.1.1.1", Modality: "CT",
		StudyDate: "20200301",
	})
	ds, err := dicom.ParseBytes(b, "in.dcm")
	if err != nil {
		t.Fatal(err)
	}
	pixels := append([]byte(nil), ds.Find(dicom.TagPixelData).Raw...)

	res, err := deident.NewTransformer(cc, m).Apply(ds)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("INPUT patient=PID-A study=9.1.1 sop=9.1.1.1.1 date=20200301 "+
		"-> patient=%s study=%s sop=%s date=%s basis=HMAC crosswalk",
		res.PatientID, res.StudyUID, res.SOPUID, res.StudyDate)

	if res.PatientID == "PID-A" {
		t.Fatal("patient id not pseudonymized")
	}
	for _, el := range res.Elements {
		for _, v := range el.Values {
			if v == "Zhang^San" {
				t.Fatal("original patient name leaked")
			}
		}
	}
	pd := findEl(res.Elements, dicom.TagPixelData)
	if pd == nil {
		t.Fatal("pixel data dropped")
	}
	if string(pd.Raw) != string(pixels) {
		t.Fatal("pixel data modified during de-identification")
	}
	if res.StudyUID == "9.1.1" || res.SeriesUID == "9.1.1.1" ||
		res.SOPUID == "9.1.1.1.1" {
		t.Fatal("UIDs not remapped")
	}
}

func TestTransformConsistentAcrossInstances(t *testing.T) {
	m := newTestMapping(t)
	cc := compiled(t)
	build := func(sop, date string) *dicom.Dataset {
		ds, err := dicom.ParseBytes(sample.Build(sample.Options{
			PatientID: "PID-A", PatientName: "Zhang^San", StudyUID: "9.1.1",
			SeriesUID: "9.1.1.1", SOPUID: sop, Modality: "CT",
			StudyDate: date,
		}), sop+".dcm")
		if err != nil {
			t.Fatal(err)
		}
		return ds
	}
	r1, err := deident.NewTransformer(cc, m).Apply(build("9.1.1.1.1", "20200301"))
	if err != nil {
		t.Fatal(err)
	}
	r2, err := deident.NewTransformer(cc, m).Apply(build("9.1.1.1.2", "20200305"))
	if err != nil {
		t.Fatal(err)
	}
	if r1.PatientID != r2.PatientID {
		t.Fatalf("patient mapping inconsistent: %s vs %s", r1.PatientID, r2.PatientID)
	}
	if r1.StudyUID != r2.StudyUID || r1.SeriesUID != r2.SeriesUID {
		t.Fatal("study/series mapping inconsistent across instances")
	}
	shift1 := daysBetween(t, "20200301", r1.StudyDate)
	shift2 := daysBetween(t, "20200305", r2.StudyDate)
	if shift1 != shift2 {
		t.Fatalf("per-patient shift changed between instances: %d vs %d",
			shift1, shift2)
	}
}

func daysBetween(t *testing.T, a, b string) int {
	t.Helper()
	ta, err := time.Parse("20060102", a)
	if err != nil {
		t.Fatal(err)
	}
	tb, err := time.Parse("20060102", b)
	if err != nil {
		t.Fatal(err)
	}
	return int(tb.Sub(ta).Hours() / 24)
}

func findEl(els []*dicom.Element, t dicom.Tag) *dicom.Element {
	for _, e := range els {
		if e.Tag == t {
			return e
		}
	}
	return nil
}
