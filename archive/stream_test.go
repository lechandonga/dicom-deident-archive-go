package archive_test

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/lechandonga/dicom-deident-archive-go/archive"
	"github.com/lechandonga/dicom-deident-archive-go/deident"
	"github.com/lechandonga/dicom-deident-archive-go/dicomtest"
)

// zeroReader streams zeros without allocating the full size.
type zeroReader struct{ remaining int64 }

func (z *zeroReader) Read(p []byte) (int, error) {
	if z.remaining == 0 {
		return 0, io.EOF
	}
	n := len(p)
	if int64(n) > z.remaining {
		n = int(z.remaining)
	}
	for i := 0; i < n; i++ {
		p[i] = 0
	}
	z.remaining -= int64(n)
	return n, nil
}

// buildHugeFile writes a DICOM file with a ~250 MiB defined-length pixel
// element to disk, streaming every byte.
func buildHugeFile(t *testing.T, path string, size int64) {
	t.Helper()
	f := dicomtest.Fields{
		PatientID: "BIG-1", PatientName: "Big^File", BirthDate: "19700101", Sex: "M",
		StudyUID: "8.8.8.1", StudyDate: "20240505", StudyID: "B",
		SeriesUID: "8.8.8.1.1", SeriesNumber: "1", Modality: "CT",
		SOPUID: "8.8.8.1.1.1", InstanceNo: "1",
	}
	head := dicomtest.BuildWithOptions(dicomtest.TSExplicitLE, f,
		dicomtest.Options{})
	// head ends with a 4-byte pixel element carrying PixelBytes; replace
	// that pixel element with a large OW one by truncating the tag prefix
	// and appending our own.
	pixTag := []byte{0xE0, 0x7F, 0x10, 0x00}
	idx := bytes.Index(head, pixTag)
	if idx < 0 {
		t.Fatal("pixel tag not found in head")
	}
	prefix := head[:idx]

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := out.Write(prefix); err != nil {
		t.Fatal(err)
	}
	// Explicit LE OW pixel header: tag + "OW" + reserved + u32 length.
	hdr := make([]byte, 12)
	copy(hdr[0:4], pixTag)
	copy(hdr[4:6], "OW")
	hdr[6], hdr[7] = 0, 0
	for i := 0; i < 4; i++ {
		hdr[8+i] = byte(size >> (8 * i))
	}
	if _, err := out.Write(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, &zeroReader{remaining: size}); err != nil {
		t.Fatal(err)
	}
}

func TestLargeFileStaysBoundedInMemory(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	const size = 250 << 20 // 250 MiB
	dir := t.TempDir()
	src := filepath.Join(dir, "huge.dcm")
	buildHugeFile(t, src, size)

	var ms0, ms1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&ms0)

	store, err := archive.Open(filepath.Join(dir, "arc"),
		deident.Mapper{Salt: "big", UIDRoot: "2.25.9"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := store.Import(src)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Duplicate {
		t.Fatal("unexpected duplicate")
	}

	out := filepath.Join(dir, "export")
	man, err := store.Export(archive.Query{}, out)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	runtime.GC()
	runtime.ReadMemStats(&ms1)

	growth := int64(ms1.HeapAlloc) - int64(ms0.HeapAlloc)
	if growth > size/4 {
		t.Fatalf("heap grew by %d MiB processing a %d MiB file (not streaming)",
			growth>>20, size>>20)
	}

	info, err := os.Stat(filepath.Join(out, man.Entries[0].RelativePath))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() < size {
		t.Fatalf("exported file truncated: %d bytes", info.Size())
	}
	t.Logf("processed %d MiB file; heap growth %d MiB; export %d bytes",
		size>>20, growth>>20, info.Size())
	fmt.Println()
}
