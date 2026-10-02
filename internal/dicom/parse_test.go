package dicom_test

import (
	"bytes"
	"compress/zlib"
	"errors"
	"strings"
	"testing"

	"github.com/lechandonga/dicom-deident-archive-go/internal/dicom"
	"github.com/lechandonga/dicom-deident-archive-go/internal/sample"
)

func TestParseAllTransferSyntaxes(t *testing.T) {
	cases := []struct {
		name string
		ts   string
		be   bool
	}{
		{"explicit-le", dicom.TSExplicitLE, false},
		{"implicit-le", dicom.TSImplicitLE, false},
		{"explicit-be", dicom.TSExplicitBE, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := sample.Build(sample.Options{
				PatientID: "PID-1", PatientName: "A^B", StudyUID: "1.2.3",
				SeriesUID: "1.2.3.4", SOPUID: "1.2.3.4.5", Modality: "CT",
				StudyDate: "20200301", TransferSyntax: tc.ts,
			})
			ds, err := dicom.ParseBytes(b, tc.name+".dcm")
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if ds.TransferSyntax != tc.ts {
				t.Errorf("transfer syntax = %q", ds.TransferSyntax)
			}
			if got := ds.Find(dicom.TagPatientID).First(); got != "PID-1" {
				t.Errorf("patient id = %q", got)
			}
			if got := ds.Find(dicom.TagModality).First(); got != "CT" {
				t.Errorf("modality = %q", got)
			}
			if len(ds.Find(dicom.TagPixelData).Raw) != 16 {
				t.Errorf("pixel bytes = %d", len(ds.Find(dicom.TagPixelData).Raw))
			}
		})
	}
}

func TestParseDeflatedExplicit(t *testing.T) {
	b := sample.Build(sample.Options{
		PatientID: "PID-1", StudyUID: "1.2.3", SeriesUID: "1.2.3.4",
		SOPUID: "1.2.3.4.5", Modality: "MR", StudyDate: "20210101",
		TransferSyntax: dicom.TSDeflatedExplicit,
	})
	split := bytes.Index(b[132:], []byte{0x10, 0x00, 0x20, 0x00, 'L', 'O'}) + 132
	var zbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	if _, err := zw.Write(b[split:]); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	defl := append(append([]byte{}, b[:split]...), zbuf.Bytes()...)
	ds, err := dicom.ParseBytes(defl, "deflated.dcm")
	if err != nil {
		t.Fatalf("parse deflated: %v", err)
	}
	if got := ds.Find(dicom.TagModality).First(); got != "MR" {
		t.Errorf("modality = %q", got)
	}
}

func TestParseCorruptAndTruncated(t *testing.T) {
	cases := []struct {
		name   string
		opt    sample.Options
		reason string
	}{
		{"truncated", sample.Options{PatientID: "P", StudyUID: "1", SeriesUID: "1.1",
			SOPUID: "1.1.1", StudyDate: "20200101", TruncateBytes: 20},
			dicom.ReasonTruncated},
		{"bad-preamble", sample.Options{PatientID: "P", StudyUID: "1",
			SeriesUID: "1.1", SOPUID: "1.1.1", BreakPreamble: true},
			dicom.ReasonNotDICOM},
		{"missing-pid", sample.Options{StudyUID: "1", SeriesUID: "1.1",
			SOPUID: "1.1.1", StudyDate: "20200101", DropPatientID: true},
			dicom.ReasonMissingPatientID},
		{"missing-sop", sample.Options{PatientID: "P", StudyUID: "1",
			SeriesUID: "1.1", SOPUID: "1.1.1", DropSOPUID: true},
			dicom.ReasonMissingSOPUID},
		{"missing-study", sample.Options{PatientID: "P",
			SeriesUID: "1.1", SOPUID: "1.1.1"},
			dicom.ReasonMissingStudyUID},
		{"invalid-date", sample.Options{PatientID: "P", StudyUID: "1",
			SeriesUID: "1.1", SOPUID: "1.1.1", BadDate: "2020-13-40"},
			dicom.ReasonInvalidDate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := sample.Build(tc.opt)
			_, err := dicom.ParseBytes(b, tc.name+".dcm")
			var re *dicom.RejectError
			if !errors.As(err, &re) {
				t.Fatalf("expected reject, got %v", err)
			}
			t.Logf("INPUT tag=%s file=%s -> REJECT reason=%s detail=%q",
				re.Tag, re.File, re.Reason, re.Detail)
			if re.Reason != tc.reason {
				t.Fatalf("reason = %s, want %s", re.Reason, tc.reason)
			}
			if !strings.Contains(re.File, tc.name+".dcm") {
				t.Fatalf("error must name the offending file, got %q", re.File)
			}
		})
	}
}

func TestParseTruncatedReasonsAreDistinct(t *testing.T) {
	b := sample.Build(sample.Options{PatientID: "P", StudyUID: "1",
		SeriesUID: "1.1", SOPUID: "1.1.1", StudyDate: "20200101"})
	reasons := map[string]bool{}
	rejectOne := func(raw []byte, label string) string {
		_, err := dicom.ParseBytes(raw, label)
		var re *dicom.RejectError
		if !errors.As(err, &re) {
			t.Fatalf("%s: expected reject, got nil", label)
		}
		return re.Reason
	}
	reasons[rejectOne(b[:100], "tiny.dcm")] = true
	broken := append(append([]byte{}, b...), 0)
	copy(broken[128:132], "XXXX")
	reasons[rejectOne(broken, "nomagic.dcm")] = true
	if !reasons[dicom.ReasonTruncated] || !reasons[dicom.ReasonNotDICOM] {
		t.Fatalf("expected distinct truncate/preamble reasons, got %v", reasons)
	}
}
