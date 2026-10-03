package dicom_test

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/lechandonga/dicom-deident-archive-go/dicom"
	"github.com/lechandonga/dicom-deident-archive-go/dicomtest"
)

func fmtTag(t dicom.Tag) string { return fmt.Sprintf("(%04X,%04X)", t.Group, t.Element) }

func rewriteFields() dicomtest.Fields {
	return dicomtest.Fields{
		PatientID: "PAT-9", PatientName: "Li^Si", BirthDate: "19771231", Sex: "F",
		StudyUID: "9.8.7.1", StudyDate: "20230601", StudyID: "SX",
		SeriesUID: "9.8.7.1.2", SeriesNumber: "3", Modality: "MR",
		SOPUID: "9.8.7.1.2.4", InstanceNo: "11",
	}
}

func testPlan(f dicomtest.Fields) dicom.Plan {
	return dicom.Plan{
		Ident: dicom.Identity{
			OrigPatientID: f.PatientID, OrigPatientName: f.PatientName,
			PseudoPatientID: "PSEUDO-9", PseudoName: "ANON^X",
			UIDs: map[string]string{
				f.StudyUID:  "2.25.7.1",
				f.SeriesUID: "2.25.7.2",
				f.SOPUID:    "2.25.7.4",
			},
		},
		ShiftDA: func(da string) string {
			if da == f.StudyDate {
				return "20230502"
			}
			if da == f.BirthDate {
				return "19771201"
			}
			return da
		},
		Fingerprint:    "FP-1",
		OriginalSOPUID: f.SOPUID,
		Method:         "TEST-1",
	}
}

func TestRewriteRewritesIdentityAcrossSyntaxes(t *testing.T) {
	f := rewriteFields()
	for _, ts := range []string{dicomtest.TSImplicitLE, dicomtest.TSExplicitLE, dicomtest.TSExplicitBE} {
		in := dicomtest.Build(ts, f)
		var out bytes.Buffer
		if err := dicom.RewriteStream(bytes.NewReader(in), &out, testPlan(f)); err != nil {
			t.Fatalf("%s: %v", ts, err)
		}
		m, err := dicom.Parse(bytes.NewReader(out.Bytes()))
		if err != nil {
			t.Fatalf("%s reparse: %v", ts, err)
		}
		if m.PatientID != "PSEUDO-9" || m.PatientName != "ANON^X" {
			t.Fatalf("%s identity: %+v", ts, m)
		}
		if m.StudyInstanceUID != "2.25.7.1" || m.SeriesInstanceUID != "2.25.7.2" ||
			m.SOPInstanceUID != "2.25.7.4" {
			t.Fatalf("%s uids: %+v", ts, m)
		}
		if m.StudyDate != "20230502" || m.PatientBirthDate != "19771201" {
			t.Fatalf("%s dates: study=%s birth=%s", ts, m.StudyDate, m.PatientBirthDate)
		}
		if m.DeidentFingerprint != "FP-1" {
			t.Fatalf("%s marker missing: %q", ts, m.DeidentFingerprint)
		}
		// Original values must be gone from the bytes.
		texts, err := dicom.ScanMetadata(bytes.NewReader(out.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		forbidden := map[string]bool{
			f.PatientID: true, f.PatientName: true,
			f.StudyUID: true, f.SeriesUID: true, f.SOPUID: true,
			f.StudyDate: true, f.BirthDate: true,
		}
		for _, tv := range texts {
			if tv.Tag == dicom.TagDeidentOriginalSOPUID {
				if tv.Value != f.SOPUID {
					t.Fatalf("%s provenance marker = %q", ts, tv.Value)
				}
				continue
			}
			if forbidden[tv.Value] {
				t.Fatalf("%s tag %s leaks %q", ts,
					fmtTag(tv.Tag), tv.Value)
			}
		}
		t.Logf("%s: rewritten pid=%s study=%s birth=%s marker=%s",
			ts, m.PatientID, m.StudyDate, m.PatientBirthDate, m.DeidentFingerprint)
	}
}

func TestRewriteDeterministic(t *testing.T) {
	f := rewriteFields()
	in := dicomtest.Build(dicomtest.TSExplicitLE, f)
	var a, b bytes.Buffer
	if err := dicom.RewriteStream(bytes.NewReader(in), &a, testPlan(f)); err != nil {
		t.Fatal(err)
	}
	if err := dicom.RewriteStream(bytes.NewReader(in), &b, testPlan(f)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("rewrite is not reproducible")
	}
}

func TestEncapsulatedPixelsByteIdentical(t *testing.T) {
	f := rewriteFields()
	payload := []byte{0xFF, 0xD8, 0x00, 0x12, 0x34, 0x56, 0x78, 0x9A, 0x00, 0xFF, 0xD9}
	for _, ts := range []string{dicomtest.TSJPEGLossless, dicomtest.TSRLE, dicomtest.TSJPEGBaseline} {
		in := dicomtest.BuildWithOptions(ts, f, dicomtest.Options{
			EncapsulatedPixel: true, PixelPayload: payload,
		})
		var out bytes.Buffer
		if err := dicom.RewriteStream(bytes.NewReader(in), &out, testPlan(f)); err != nil {
			t.Fatalf("%s: %v", ts, err)
		}
		// The exact fragment payload survives untouched.
		if !bytes.Contains(out.Bytes(), payload) {
			t.Fatalf("%s: encapsulated payload was modified", ts)
		}
		// Byte span from first payload byte to EOF must equal the input
		// span: headers before the fragment change (meta rewritten), but
		// fragment bytes and delimiters do not.
		i := bytes.Index(in, payload)
		j := bytes.Index(out.Bytes(), payload)
		if i < 0 || j < 0 {
			t.Fatalf("%s: payload not found", ts)
		}
		if !bytes.Equal(in[i:], out.Bytes()[j:]) {
			t.Fatalf("%s: bytes from pixel payload onward differ", ts)
		}
		t.Logf("%s: %d pixel bytes preserved verbatim", ts, len(payload))
	}
}

func TestUncompressedPixelsByteIdentical(t *testing.T) {
	f := rewriteFields()
	payload := make([]byte, 4096)
	for i := range payload {
		payload[i] = byte(i * 7)
	}
	in := dicomtest.BuildWithOptions(dicomtest.TSExplicitLE, f,
		dicomtest.Options{PixelPayload: payload})
	var out bytes.Buffer
	if err := dicom.RewriteStream(bytes.NewReader(in), &out, testPlan(f)); err != nil {
		t.Fatal(err)
	}
	i := bytes.Index(in, payload)
	j := bytes.Index(out.Bytes(), payload)
	if !bytes.Equal(in[i:], out.Bytes()[j:]) {
		t.Fatal("defined-length pixel bytes changed")
	}
}

func TestAcceptsUndefinedSQAndMissingMetaLength(t *testing.T) {
	f := rewriteFields()
	cases := []struct {
		name string
		ts   string
		opt  dicomtest.Options
	}{
		{"undef-sq-explicit", dicomtest.TSExplicitLE, dicomtest.Options{UndefinedSQ: true}},
		{"undef-sq-implicit", dicomtest.TSImplicitLE, dicomtest.Options{UndefinedSQ: true}},
		{"no-meta-length", dicomtest.TSExplicitLE, dicomtest.Options{NoMetaGroupLength: true}},
		{"all-quirks", dicomtest.TSRLE, dicomtest.Options{
			UndefinedSQ: true, NoMetaGroupLength: true, EncapsulatedPixel: true}},
	}
	for _, c := range cases {
		raw := dicomtest.BuildWithOptions(c.ts, f, c.opt)
		if _, err := dicom.Parse(bytes.NewReader(raw)); err != nil {
			t.Fatalf("%s parse: %v", c.name, err)
		}
		var out bytes.Buffer
		if err := dicom.RewriteStream(bytes.NewReader(raw), &out, testPlan(f)); err != nil {
			t.Fatalf("%s rewrite: %v", c.name, err)
		}
		if _, err := dicom.Parse(bytes.NewReader(out.Bytes())); err != nil {
			t.Fatalf("%s reparse: %v", c.name, err)
		}
		t.Logf("accepted and rewrote %s", c.name)
	}
}

func TestRejectsCorruptionEverywhere(t *testing.T) {
	f := rewriteFields()
	base := dicomtest.BuildWithOptions(dicomtest.TSJPEGLossless, f,
		dicomtest.Options{EncapsulatedPixel: true, UndefinedSQ: true})

	// Truncation at many positions must always be ErrTruncated.
	step := len(base) / 12
	for n := step; n < len(base); n += step {
		_, err := dicom.Parse(bytes.NewReader(base[:n]))
		if err == nil {
			t.Fatalf("truncate at %d unexpectedly accepted", n)
		}
		if !errors.Is(err, dicom.ErrTruncated) && !errors.Is(err, dicom.ErrCorrupt) &&
			!errors.Is(err, dicom.ErrMissingTag) {
			t.Fatalf("truncate at %d: want structural rejection, got %v", n, err)
		}
	}

	// Flip a byte inside the pixel fragment: the declared fragment length
	// will overrun -> truncated/corrupt, never silently accepted.
	bad := append([]byte(nil), base...)
	payload := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	idx := bytes.Index(bad, payload)
	if idx < 0 {
		t.Fatal("payload marker not found")
	}
	// Corrupt the fragment length header (8 bytes before payload).
	bad[idx-6] ^= 0xFF
	bad[idx-5] ^= 0x01
	if _, err := dicom.Parse(bytes.NewReader(bad)); err == nil {
		t.Fatal("corrupt fragment length must be rejected")
	} else if !errors.Is(err, dicom.ErrTruncated) && !errors.Is(err, dicom.ErrCorrupt) {
		t.Fatalf("want truncation/corruption, got %v", err)
	}

	// Trailing garbage after a clean file is also rejected.
	trailer := append(append([]byte(nil), base...), 0x01, 0x02, 0x03)
	if _, err := dicom.Parse(bytes.NewReader(trailer)); err == nil ||
		(!errors.Is(err, dicom.ErrTruncated) && !errors.Is(err, dicom.ErrCorrupt)) {
		t.Fatalf("trailing garbage must be rejected, got %v", err)
	}
}
