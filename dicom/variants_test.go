package dicom_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/lechandonga/dicom-deident-archive-go/dicom"
	"github.com/lechandonga/dicom-deident-archive-go/dicomtest"
)

func TestAcceptEncapsulatedAndRareEncodings(t *testing.T) {
	base := dicomtest.Fields{
		PatientID: "P1", PatientName: "N1", BirthDate: "19700101",
		StudyUID: "5.1", StudyDate: "20240101", SeriesUID: "5.2",
		SOPUID: "5.3", Modality: "CT",
	}
	pixel := bytes.Repeat([]byte{0xCD}, 5000)
	cases := []struct {
		name string
		ts   string
		opt  dicomtest.Options
	}{
		{"jpeg-lossless", dicomtest.TSJPEGLossless, dicomtest.Options{Pixel: pixel}},
		{"rle", dicomtest.TSRLE, dicomtest.Options{Pixel: pixel}},
		{"undef-sequence", dicomtest.TSExplicitLE, dicomtest.Options{UndefLengthSequence: true}},
		{"defined-sequence", dicomtest.TSExplicitLE, dicomtest.Options{DefinedLengthSequence: true}},
		{"no-meta-group-length", dicomtest.TSExplicitLE, dicomtest.Options{NoMetaGroupLength: true}},
		{"jpeg-undef-sq", dicomtest.TSJPEGLossless, dicomtest.Options{Pixel: pixel, UndefLengthSequence: true}},
	}
	for _, c := range cases {
		raw := dicomtest.BuildOptions(c.ts, base, c.opt)
		m, err := dicom.ParseVerify(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("%s rejected: %v", c.name, err)
		}
		if m.PatientID != "P1" || m.TransferSyntaxUID != c.ts {
			t.Fatalf("%s bad meta: %+v", c.name, m)
		}
		if v, ok := m.Tags[dicom.Tag{Group: 0x0008, Element: 0x1155}]; c.opt.UndefLengthSequence || c.opt.DefinedLengthSequence {
			if !ok || v != "5.1" {
				t.Fatalf("%s nested UID not extracted: %q ok=%v", c.name, v, ok)
			}
		}
		t.Logf("accepted %s (%d bytes)", c.name, len(raw))
	}
}

func TestTruncationAtEveryRegionRejected(t *testing.T) {
	f := dicomtest.Fields{
		PatientID: "P1", PatientName: "N1", StudyUID: "5.1", StudyDate: "20240101",
		SeriesUID: "5.2", SOPUID: "5.3", Modality: "CT",
	}
	raw := dicomtest.BuildOptions(dicomtest.TSJPEGLossless, f,
		dicomtest.Options{UndefLengthSequence: true, Pixel: bytes.Repeat([]byte{0xAB}, 3000)})

	// Cut points spanning preamble, meta, dataset, sequence, fragments.
	points := []int{10, 130, 133, 150, 200, 260, 300, len(raw) / 2, len(raw) - 400,
		len(raw) - 20, len(raw) - 8, len(raw) - 1}
	for _, cut := range points {
		if cut > len(raw) {
			continue
		}
		_, err := dicom.ParseVerify(bytes.NewReader(raw[:cut]))
		if err == nil {
			t.Fatalf("cut at %d/%d accepted", cut, len(raw))
		}
		if !errors.Is(err, dicom.ErrTruncated) && !errors.Is(err, dicom.ErrCorrupt) &&
			!errors.Is(err, dicom.ErrNotDICOM) && !errors.Is(err, dicom.ErrMissingTag) {
			t.Fatalf("cut at %d: unclassified error %v", cut, err)
		}
		t.Logf("cut %4d -> %v", cut, err)
	}

	// Removing the Sequence Delimiter (last 8 bytes) is caught: encapsulated
	// pixel data must be properly terminated.
	if _, err := dicom.ParseVerify(bytes.NewReader(raw[:len(raw)-8])); err == nil {
		t.Fatal("missing pixel Sequence Delimiter accepted")
	} else {
		t.Logf("missing delimiter -> %v", err)
	}
}

func TestStructuralCorruptionRejected(t *testing.T) {
	f := dicomtest.Fields{
		PatientID: "P1", PatientName: "N1", StudyUID: "5.1", StudyDate: "20240101",
		SeriesUID: "5.2", SOPUID: "5.3", Modality: "CT",
	}
	raw := dicomtest.BuildOptions(dicomtest.TSExplicitLE, f,
		dicomtest.Options{UndefLengthSequence: true})

	// Flip a byte in a dataset VR field -> unknown VR corruption. Find the
	// Modality element (CS) in raw bytes.
	damaged := append([]byte{}, raw...)
	idx := bytes.Index(damaged, []byte{0x08, 0x00, 0x60, 0x00, 'C', 'S'})
	if idx < 0 {
		t.Fatal("could not locate CS element")
	}
	damaged[idx+4] = '?'
	damaged[idx+5] = '?'
	_, err := dicom.ParseVerify(bytes.NewReader(damaged))
	if !errors.Is(err, dicom.ErrCorrupt) {
		t.Fatalf("bad VR: want ErrCorrupt, got %v", err)
	}
	t.Logf("bad VR -> %v", err)
}
