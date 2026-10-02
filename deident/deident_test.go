package deident_test

import (
	"strings"
	"testing"

	"github.com/lechandonga/dicom-deident-archive-go/deident"
)

var m = deident.Mapper{Salt: "test-salt", UIDRoot: "2.25.999"}

func TestDeterministicAndDistinct(t *testing.T) {
	if m.PatientID("A") != m.PatientID("A") {
		t.Fatal("same input must map to same output")
	}
	if m.PatientID("A") == m.PatientID("B") {
		t.Fatal("different patients must not collide")
	}
	if m.StudyUID("1.2.3") == m.SeriesUID("1.2.3") {
		t.Fatal("different kinds must not collide")
	}
	seen := map[string]string{}
	for _, id := range []string{"P1", "P2", "P3", "P4", "P5", "P6", "P7", "P8"} {
		out := m.PatientID(id)
		if prev, dup := seen[out]; dup {
			t.Fatalf("collision: %q and %q -> %q", prev, id, out)
		}
		seen[out] = id
	}
}

func TestDateShiftPreservesOrderAndIntervals(t *testing.T) {
	dates := []string{"20240101", "20240111", "20240301"}
	shifted := []string{}
	for _, d := range dates {
		shifted = append(shifted, m.ShiftDate("PAT-X", d))
	}
	t.Logf("patient PAT-X shift=%d days: %v -> %v", m.DateShiftDays("PAT-X"), dates, shifted)
	for i := 1; i < len(dates); i++ {
		if shifted[i] <= shifted[i-1] {
			t.Fatalf("order not preserved: %v", shifted)
		}
	}
	// Interval preserved: same patient, constant shift.
	if m.DateShiftDays("PAT-X") != m.DateShiftDays("PAT-X") {
		t.Fatal("shift must be constant per patient")
	}
	// Idempotent across reruns: mapping is from original values only.
	again := m.ShiftDate("PAT-X", "20240101")
	if again != shifted[0] {
		t.Fatalf("rerun drift: %q vs %q", again, shifted[0])
	}
}

func TestUIDSyntax(t *testing.T) {
	u := m.StudyUID("1.2.3.4.5")
	if len(u) > 64 || !strings.HasPrefix(u, "2.25.999.") {
		t.Fatalf("bad uid %q (len %d)", u, len(u))
	}
	for _, c := range u {
		if c != '.' && (c < '0' || c > '9') {
			t.Fatalf("invalid uid char %q in %q", c, u)
		}
	}
}
