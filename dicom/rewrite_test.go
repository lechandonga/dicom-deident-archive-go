package dicom_test

import (
	"bytes"
	"testing"

	"github.com/lechandonga/dicom-deident-archive-go/dicom"
	"github.com/lechandonga/dicom-deident-archive-go/dicomtest"
)

func TestRewritePreservesEncapsulatedPixelByteForByte(t *testing.T) {
	f := dicomtest.Fields{
		PatientID: "P1", PatientName: "Secret^Name", BirthDate: "19700102",
		StudyUID: "8.1", StudyDate: "20240202", SeriesUID: "8.2",
		SOPUID: "8.3", Modality: "MR",
	}
	// 80 MiB payload proves pixel data flows without whole-file buffering.
	const size = 80 << 20
	pixel := make([]byte, size)
	for i := range pixel {
		pixel[i] = byte(i % 251)
	}
	raw := dicomtest.BuildOptions(dicomtest.TSJPEGLossless, f,
		dicomtest.Options{Pixel: pixel, UndefLengthSequence: true})

	var out bytes.Buffer
	spec := dicom.RewriteSpec{
		MapUID:         func(k, s string) string { return "2.25.7." + s },
		MapPatientID:   func(string) string { return "PSEUDO" },
		MapPatientName: func(string) string { return "ANON^X" },
		ShiftDate:      func(string) string { return "20000101" },
		AuditSecrets:   []string{"Secret^Name", "P1", "8.1", "8.2", "8.3", "20240202", "19700102"},
	}
	res, err := dicom.Rewrite(bytes.NewReader(raw), &out, spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.AuditFindings) != 0 {
		t.Fatalf("audit false positives: %+v", res.AuditFindings)
	}
	if out.Len() < size {
		t.Fatalf("output lost payload: %d", out.Len())
	}
	got := out.Bytes()
	gstart := bytes.Index(got, pixel[:64])
	if gstart < 0 {
		t.Fatal("pixel payload not located")
	}
	tail := got[gstart : gstart+size]
	if !bytes.Equal(tail, pixel) {
		t.Fatal("encapsulated pixel bytes were modified")
	}
	t.Logf("streamed %d MiB pixel byte-for-byte", size>>20)
}

func TestRewriteRejectsResidualSecrets(t *testing.T) {
	f := dicomtest.Fields{
		PatientID: "P1", PatientName: "N1", StudyUID: "8.1", StudyDate: "20240202",
		SeriesUID: "8.2", SOPUID: "8.3", Modality: "CT",
	}
	raw := dicomtest.Build(dicomtest.TSExplicitLE, f)
	var out bytes.Buffer
	// Identity function: residual originals must be reported, not hidden.
	spec := dicom.RewriteSpec{
		MapUID:         func(k, s string) string { return s },
		MapPatientID:   func(s string) string { return s },
		MapPatientName: func(s string) string { return s },
		ShiftDate:      func(s string) string { return s },
		AuditSecrets:   []string{"P1", "N1", "8.1", "8.2", "8.3", "20240202"},
	}
	res, err := dicom.Rewrite(bytes.NewReader(raw), &out, spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.AuditFindings) == 0 {
		t.Fatal("expected residual identity findings, got none")
	}
	tags := map[string]bool{}
	for _, a := range res.AuditFindings {
		tags[a.Tag.String()] = true
	}
	for _, want := range []string{"(0010,0020)", "(0010,0010)", "(0020,000D)", "(0008,0020)"} {
		if !tags[want] {
			t.Fatalf("audit did not flag residual in %s: %+v", want, res.AuditFindings)
		}
	}
	t.Logf("residuals reported: %+v", res.AuditFindings)
}
