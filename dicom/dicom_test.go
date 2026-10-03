package dicom_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/lechandonga/dicom-deident-archive-go/dicom"
	"github.com/lechandonga/dicom-deident-archive-go/dicomtest"
)

var sample = dicomtest.Fields{
	PatientID: "PAT-001", PatientName: "Zhang^Wei", BirthDate: "19800102", Sex: "M",
	StudyUID: "1.2.3.4.100", StudyDate: "20240115", StudyID: "ST1",
	SeriesUID: "1.2.3.4.100.1", SeriesNumber: "2", Modality: "CT",
	SOPUID: "1.2.3.4.100.1.1", InstanceNo: "7",
}

func TestParseAllTransferSyntaxes(t *testing.T) {
	for _, ts := range []string{dicomtest.TSImplicitLE, dicomtest.TSExplicitLE, dicomtest.TSExplicitBE} {
		m, err := dicom.Parse(bytes.NewReader(dicomtest.Build(ts, sample)))
		if err != nil {
			t.Fatalf("ts %s: %v", ts, err)
		}
		if m.PatientID != "PAT-001" || m.StudyInstanceUID != "1.2.3.4.100" ||
			m.SeriesInstanceUID != "1.2.3.4.100.1" || m.SOPInstanceUID != "1.2.3.4.100.1.1" ||
			m.StudyDate != "20240115" || m.Modality != "CT" || m.InstanceNumber != "7" {
			t.Fatalf("ts %s: unexpected meta %+v", ts, m)
		}
	}
}

func TestRejectsNotDICOM(t *testing.T) {
	_, err := dicom.Parse(bytes.NewReader([]byte("definitely not a dicom file at all............")))
	if !errors.Is(err, dicom.ErrNotDICOM) && !errors.Is(err, dicom.ErrTruncated) {
		t.Fatalf("want ErrNotDICOM/ErrTruncated, got %v", err)
	}
	bad := dicomtest.Build(dicomtest.TSExplicitLE, sample)
	copy(bad[128:132], "XXXX")
	if _, err := dicom.Parse(bytes.NewReader(bad)); !errors.Is(err, dicom.ErrNotDICOM) {
		t.Fatalf("want ErrNotDICOM, got %v", err)
	}
}

func TestRejectsTruncated(t *testing.T) {
	full := dicomtest.Build(dicomtest.TSExplicitLE, sample)
	for _, n := range []int{10, 130, 140, 200, 300, 400} {
		_, err := dicom.Parse(bytes.NewReader(full[:n]))
		if !errors.Is(err, dicom.ErrTruncated) {
			t.Fatalf("cut at %d: want ErrTruncated, got %v", n, err)
		}
	}
}

func TestRejectsMissingMandatoryTag(t *testing.T) {
	noPatient := sample
	noPatient.PatientID = ""
	raw := dicomtest.Build(dicomtest.TSExplicitLE, noPatient)
	_, err := dicom.Parse(bytes.NewReader(raw))
	var mt *dicom.MissingTagError
	if !errors.As(err, &mt) {
		t.Fatalf("want MissingTagError, got %v", err)
	}
	t.Logf("missing tag detected: %v", err)
}

func TestRejectsUnsupportedTransferSyntax(t *testing.T) {
	// Deflated Explicit VR LE: real compression this implementation cannot
	// process locally (no inflate); must be rejected distinctly.
	raw := dicomtest.Build("1.2.840.10008.1.2.1.99", sample)
	_, err := dicom.Parse(bytes.NewReader(raw))
	var ut *dicom.UnsupportedTransferError
	if !errors.As(err, &ut) {
		t.Fatalf("want UnsupportedTransferError, got %v", err)
	}
	t.Logf("unsupported transfer syntax: %v", err)
}
