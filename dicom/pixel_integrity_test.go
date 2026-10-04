package dicom_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/lechandonga/dicom-deident-archive-go/dicom"
	"github.com/lechandonga/dicom-deident-archive-go/dicomtest"
)

func pixelIntegrityFields() dicomtest.Fields {
	return dicomtest.Fields{
		PatientID: "P1", PatientName: "N1", StudyUID: "5.1", StudyDate: "20240101",
		SeriesUID: "5.2", SOPUID: "5.3", Modality: "CT",
	}
}

// TestNativePixelTruncationRejected cuts native (uncompressed) pixel data
// in the middle of the payload, not just at file head/tail. Every cut must
// be classified as ErrTruncated by ParseVerify, while the intact file and a
// 1-byte truncation that lands exactly on a trailing pad boundary stay
// well-defined.
func TestNativePixelTruncationRejected(t *testing.T) {
	pixel := bytes.Repeat([]byte{0x5A}, 6000) // even length
	for _, ts := range []string{dicomtest.TSImplicitLE, dicomtest.TSExplicitLE, dicomtest.TSExplicitBE} {
		raw := dicomtest.BuildOptions(ts, pixelIntegrityFields(), dicomtest.Options{Pixel: pixel})
		if _, err := dicom.ParseVerify(bytes.NewReader(raw)); err != nil {
			t.Fatalf("%s: intact file rejected: %v", ts, err)
		}
		// Locate the pixel payload directly: its content is the only run of
		// the 0x5A fill byte. Cuts are made *inside* that run.
		idx := bytes.Index(raw, pixel[:256])
		if idx < 0 {
			t.Fatalf("%s: pixel payload not located", ts)
		}
		for _, cutBytes := range []int{1, 2, 7, 100, 3000, 5999} {
			cut := len(raw) - cutBytes
			if cut <= idx {
				continue
			}
			_, err := dicom.ParseVerify(bytes.NewReader(raw[:cut]))
			if !errors.Is(err, dicom.ErrTruncated) {
				t.Fatalf("%s cut %d byte(s) inside/after pixel: want ErrTruncated, got %v",
					ts, cutBytes, err)
			}
			t.Logf("input=%s truncate=%d byte(s) -> rejected: %v", ts, cutBytes, err)
		}
	}
}

// TestDeclaredPixelLengthWithoutPayloadRejected covers the extreme case:
// the pixel element header (declared length intact) survives but the whole
// payload is gone.
func TestDeclaredPixelLengthWithoutPayloadRejected(t *testing.T) {
	pixel := bytes.Repeat([]byte{0x33}, 1000)
	raw := dicomtest.BuildOptions(dicomtest.TSExplicitLE, pixelIntegrityFields(),
		dicomtest.Options{Pixel: pixel})
	idx := bytes.Index(raw, pixel[:64])
	if idx < 0 {
		t.Fatal("pixel payload not located")
	}
	// Cut right after the pixel element header: declared length remains,
	// zero payload bytes present.
	for _, cut := range []int{idx, idx + 1, idx + 2, idx + 50} {
		_, err := dicom.ParseVerify(bytes.NewReader(raw[:cut]))
		if !errors.Is(err, dicom.ErrTruncated) {
			t.Fatalf("cut at %d (payload offset %d): want ErrTruncated, got %v", cut, idx, err)
		}
		t.Logf("input=native cut=%d/%d (payload missing) -> rejected: %v", cut, len(raw), err)
	}
}

// TestEncapsulatedPixelTruncationRejected cuts JPEG-lossless and RLE
// encapsulated payloads in the middle of a fragment and at the fragment
// header level.
func TestEncapsulatedPixelTruncationRejected(t *testing.T) {
	pixel := bytes.Repeat([]byte{0xAB}, 6000)
	for _, ts := range []string{dicomtest.TSJPEGLossless, dicomtest.TSRLE} {
		raw := dicomtest.BuildOptions(ts, pixelIntegrityFields(),
			dicomtest.Options{Pixel: pixel, UndefLengthSequence: true})
		if _, err := dicom.ParseVerify(bytes.NewReader(raw)); err != nil {
			t.Fatalf("%s: intact file rejected: %v", ts, err)
		}
		idx := bytes.Index(raw, pixel[:256])
		if idx < 0 {
			t.Fatalf("%s: fragment payload not located", ts)
		}
		// Internal fragment cuts (must not pass).
		for _, cutBytes := range []int{9, 20, 4000, 5999} {
			cut := len(raw) - cutBytes
			if cut <= idx {
				continue
			}
			_, err := dicom.ParseVerify(bytes.NewReader(raw[:cut]))
			if !errors.Is(err, dicom.ErrTruncated) {
				t.Fatalf("%s cut %d byte(s) inside fragment: want ErrTruncated, got %v",
					ts, cutBytes, err)
			}
			t.Logf("input=%s truncate=%d byte(s) -> rejected: %v", ts, cutBytes, err)
		}
		// Remove only the Sequence Delimiter (last 8 bytes).
		if _, err := dicom.ParseVerify(bytes.NewReader(raw[:len(raw)-8])); !errors.Is(err, dicom.ErrTruncated) {
			t.Fatalf("%s missing sequence delimiter: want ErrTruncated, got %v", ts, err)
		}
		// Cut into the data fragment header/body boundary.
		for _, at := range []int{idx - 1, idx - 4, idx - 8} {
			if at < 0 {
				continue
			}
			_, err := dicom.ParseVerify(bytes.NewReader(raw[:at]))
			if !errors.Is(err, dicom.ErrTruncated) {
				t.Fatalf("%s cut at fragment boundary %d: want ErrTruncated, got %v", ts, at, err)
			}
			t.Logf("input=%s cut=%d fragment boundary -> rejected: %v", ts, at, err)
		}
	}
}

// TestTruncationInEveryRegionClassified ensures cuts through the file meta
// group, dataset headers, nested sequences and the file tail all surface as
// ErrTruncated/ErrCorrupt/ErrNotDICOM (never silent acceptance) on native
// files too.
func TestTruncationInEveryRegionClassified(t *testing.T) {
	pixel := bytes.Repeat([]byte{0xC7}, 4000)
	raw := dicomtest.BuildOptions(dicomtest.TSExplicitLE, pixelIntegrityFields(),
		dicomtest.Options{Pixel: pixel, DefinedLengthSequence: true, UndefLengthSequence: true})
	points := []int{10, 130, 140, 200, 260, 330, len(raw) / 2, len(raw) - 3000, len(raw) - 10, len(raw) - 1}
	for _, cut := range points {
		_, err := dicom.ParseVerify(bytes.NewReader(raw[:cut]))
		if err == nil {
			t.Fatalf("cut at %d/%d accepted", cut, len(raw))
		}
		if !errors.Is(err, dicom.ErrTruncated) && !errors.Is(err, dicom.ErrCorrupt) &&
			!errors.Is(err, dicom.ErrNotDICOM) && !errors.Is(err, dicom.ErrMissingTag) {
			t.Fatalf("cut at %d: unclassified error %v", cut, err)
		}
		t.Logf("input=native+seqs cut=%d -> rejected: %v", cut, err)
	}
}

// TestDatasetPaddingAcceptedAndTrailingGarbageRejected covers the tail
// positions: (FFFC,FFFC) Data Set Trailing Padding and one pad byte after
// an odd-length final element are legal; two or more stray bytes are not.
func TestDatasetPaddingAcceptedAndTrailingGarbageRejected(t *testing.T) {
	raw := dicomtest.Build(dicomtest.TSExplicitLE, pixelIntegrityFields())
	// Append a Data Set Trailing Padding element: explicit VR UN uses the
	// 12-byte long header form (tag + "UN" + 2 reserved + 32-bit length).
	padded := append(append([]byte{}, raw...),
		0xFC, 0xFF, 0xFC, 0xFF, 'U', 'N', 0, 0, 2, 0, 0, 0, 0, 0)
	if _, err := dicom.ParseVerify(bytes.NewReader(padded)); err != nil {
		t.Fatalf("dataset trailing padding rejected: %v", err)
	}
	t.Logf("input=explicit+FFFCFFFC conclusion=accepted")

	// Two random trailing bytes with no element header -> corruption.
	bad := append(append([]byte{}, raw...), 0xAA, 0xBB)
	err := parseErr(bad)
	if !errors.Is(err, dicom.ErrCorrupt) &&
		!errors.Is(err, dicom.ErrTruncated) {
		t.Fatalf("trailing garbage: want ErrCorrupt/ErrTruncated, got %v", err)
	}
	t.Logf("input=explicit+2stray-bytes conclusion=rejected: %v", err)
}

func parseErr(b []byte) error {
	_, err := dicom.ParseVerify(bytes.NewReader(b))
	return err
}

// TestOddLengthNativePixelPadByte: native pixel data with an odd declared
// length is legal only when exactly one pad byte follows and the element is
// last; removing the pad or adding a second byte must fail.
func TestOddLengthNativePixelPadByte(t *testing.T) {
	oddPixel := bytes.Repeat([]byte{0x44}, 999) // odd -> one pad follows
	raw := dicomtest.BuildOptions(dicomtest.TSExplicitLE, pixelIntegrityFields(),
		dicomtest.Options{Pixel: oddPixel})
	if _, err := dicom.ParseVerify(bytes.NewReader(raw)); err != nil {
		t.Fatalf("odd pixel with pad byte rejected: %v", err)
	}
	t.Logf("input=odd-pixel+pad conclusion=accepted")
	if _, err := dicom.ParseVerify(bytes.NewReader(raw[:len(raw)-1])); !errors.Is(err, dicom.ErrTruncated) {
		t.Fatalf("odd pixel missing pad: want ErrTruncated, got %v", err)
	}
	t.Log("input=odd-pixel-minus-pad conclusion=rejected ErrTruncated")
	withExtra := append(append([]byte{}, raw...), 0x00)
	if _, err := dicom.ParseVerify(bytes.NewReader(withExtra)); !errors.Is(err, dicom.ErrCorrupt) &&
		!errors.Is(err, dicom.ErrTruncated) {
		t.Fatalf("odd pixel + extra byte: want reject, got %v", err)
	}
	t.Log("input=odd-pixel+extra-byte conclusion=rejected")
}

// TestDelimiterInsideDefinedItemRejected guards a previously silent hole:
// an Item/Sequence Delimiter inside a defined-length item is structural
// corruption, not an end marker.
func TestDelimiterInsideDefinedItemRejected(t *testing.T) {
	raw := dicomtest.BuildOptions(dicomtest.TSExplicitLE, pixelIntegrityFields(),
		dicomtest.Options{DefinedLengthSequence: true})
	if _, err := dicom.ParseVerify(bytes.NewReader(raw)); err != nil {
		t.Fatalf("intact defined-sequence file rejected: %v", err)
	}
	// Handcraft a defined-length SQ whose item contains an Item Delimiter
	// in the middle of the declared region.
	var b bytes.Buffer
	b.Write(make([]byte, 128))
	b.WriteString("DICM")
	metaHdr := raw[132 : bytes.Index(raw[132:], []byte{0x08, 0x00, 0x16, 0x00})+132]
	b.Write(metaHdr)
	// Copy dataset up to the sequence start (0008,1110).
	seqTag := []byte{0x08, 0x00, 0x10, 0x11}
	si := bytes.Index(raw, seqTag)
	if si < 0 {
		t.Fatal("sequence tag not located")
	}
	b.Write(raw[len(metaHdr)+132 : si])
	// Defined SQ of length 16: Item(len=16) then a stray Item Delimiter
	// (8 bytes) inside the declared region followed by 8 pad bytes.
	b.Write(seqTag)
	b.WriteString("SQ")
	b.Write([]byte{0, 0})
	b.Write([]byte{16, 0, 0, 0})
	b.Write([]byte{0xFE, 0xFF, 0x00, 0xE0, 16, 0, 0, 0})
	b.Write([]byte{0xFE, 0xFF, 0x0D, 0xE0, 0, 0, 0, 0})
	b.Write([]byte{0, 0, 0, 0, 0, 0, 0, 0})
	// Then minimal mandatory tags are missing anyway; the delimiter error
	// must surface as ErrCorrupt before MissingTag.
	_, err := dicom.ParseVerify(bytes.NewReader(b.Bytes()))
	if !errors.Is(err, dicom.ErrCorrupt) {
		t.Fatalf("stray delimiter in defined item: want ErrCorrupt, got %v", err)
	}
	t.Logf("input=defined-item-with-stray-delim conclusion=rejected: %v", err)
}
