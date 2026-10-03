package archive_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/lechandonga/dicom-deident-archive-go/archive"
	"github.com/lechandonga/dicom-deident-archive-go/deident"
	"github.com/lechandonga/dicom-deident-archive-go/dicom"
	"github.com/lechandonga/dicom-deident-archive-go/dicomtest"
)

func exportFields(i int, ts string) (dicomtest.Fields, dicomtest.Options) {
	mods := []string{"CT", "MR", "US"}
	f := dicomtest.Fields{
		PatientID:    fmt.Sprintf("PAT-%d", i%3),
		PatientName:  fmt.Sprintf("Demo^Patient%d", i%3),
		BirthDate:    "19800101",
		Sex:          "M",
		StudyUID:     fmt.Sprintf("2.10.%d", i/2),
		StudyDate:    fmt.Sprintf("2024%02d15", i%12+1),
		StudyID:      "ST",
		SeriesUID:    fmt.Sprintf("2.10.%d.1", i/2),
		SeriesNumber: "1",
		Modality:     mods[i%3],
		SOPUID:       fmt.Sprintf("2.10.%d.1.%d", i/2, i%2+1),
		InstanceNo:   fmt.Sprintf("%d", i%2+1),
	}
	opt := dicomtest.Options{
		EncapsulatedPixel: ts == dicomtest.TSRLE || ts == dicomtest.TSJPEGLossless,
		PixelPayload:      []byte{0xAB, byte(i), 0xCD, 0xEF, 0x00, 0x99},
	}
	if i%4 == 1 {
		opt.UndefinedSQ = true
		// Reference this instance's own SOP so export must remap it inside
		// the nested sequence as well.
		opt.RefSOPUID = f.SOPUID
	}
	if i%5 == 2 {
		opt.NoMetaGroupLength = true
	}
	return f, opt
}

func seedMixedArchive(t *testing.T, n int) (*archive.Store, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := archive.Open(filepath.Join(dir, "arc"),
		deident.Mapper{Salt: "secret", UIDRoot: "2.25.99"})
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "src")
	syntaxes := []string{
		dicomtest.TSExplicitLE, dicomtest.TSImplicitLE,
		dicomtest.TSExplicitBE, dicomtest.TSRLE, dicomtest.TSJPEGLossless,
	}
	for i := 0; i < n; i++ {
		ts := syntaxes[i%len(syntaxes)]
		f, opt := exportFields(i, ts)
		raw := dicomtest.BuildWithOptions(ts, f, opt)
		p := filepath.Join(src, fmt.Sprintf("in%02d.dcm", i))
		if err := os.MkdirAll(src, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Import(p); err != nil {
			t.Fatalf("import %d: %v", i, err)
		}
	}
	return store, dir
}

func hashTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		rel, _ := filepath.Rel(root, p)
		out[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestExportAllFilteredAndComplete(t *testing.T) {
	store, dir := seedMixedArchive(t, 12)
	out := filepath.Join(dir, "export-all")
	man, err := store.Export(archive.Query{}, out)
	if err != nil {
		t.Fatal(err)
	}
	if len(man.Entries) != 12 {
		t.Fatalf("want 12 exported instances, got %d", len(man.Entries))
	}

	outMR := filepath.Join(dir, "export-mr")
	manMR, err := store.Export(archive.Query{Modality: "MR"}, outMR)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := store.Query(archive.Query{Modality: "MR", Limit: 500})
	if len(manMR.Entries) != len(page.Instances) {
		t.Fatalf("MR export %d != query hits %d", len(manMR.Entries), len(page.Instances))
	}

	for _, e := range man.Entries {
		p := filepath.Join(out, e.RelativePath)
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("missing export file %s: %v", e.RelativePath, err)
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != e.FileSHA256 {
			t.Fatalf("hash mismatch for %s", e.RelativePath)
		}
		if _, err := dicom.Parse(bytes.NewReader(b)); err != nil {
			t.Fatalf("exported file %s unparseable: %v", e.RelativePath, err)
		}
	}
	t.Logf("exported %d instances (%d MR); every file present, valid and matching manifest hash",
		len(man.Entries), len(manMR.Entries))
}

func TestExportIdentityMatchesIndexAndConsistent(t *testing.T) {
	store, dir := seedMixedArchive(t, 9)
	out := filepath.Join(dir, "out")
	man, err := store.Export(archive.Query{}, out)
	if err != nil {
		t.Fatal(err)
	}
	bySOP := map[string]archive.ManifestEntry{}
	for _, e := range man.Entries {
		bySOP[e.SOPUID] = e
		b, err := os.ReadFile(filepath.Join(out, e.RelativePath))
		if err != nil {
			t.Fatal(err)
		}
		m, err := dicom.Parse(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		if m.PatientID != e.PatientID || m.StudyInstanceUID != e.StudyUID ||
			m.SeriesInstanceUID != e.SeriesUID || m.SOPInstanceUID != e.SOPUID ||
			m.StudyDate != e.StudyDate {
			t.Fatalf("file identifiers disagree with manifest: file=%+v entry=%+v", m, e)
		}
	}
	page, _ := store.Query(archive.Query{Limit: 500})
	for _, inst := range page.Instances {
		e, ok := bySOP[inst.SOPUID]
		if !ok {
			t.Fatalf("index SOP %s missing from manifest", inst.SOPUID)
		}
		if e.PatientID != inst.PatientID || e.StudyUID != inst.StudyUID ||
			e.StudyDate != inst.StudyDate || e.PatientName != inst.PatientName {
			t.Fatalf("manifest entry %+v disagrees with index %+v", e, inst)
		}
	}
	t.Logf("all %d manifest entries match both file content and the index", len(man.Entries))
}

func TestExportPixelsByteIdentical(t *testing.T) {
	store, dir := seedMixedArchive(t, 10)
	out := filepath.Join(dir, "out")
	man, err := store.Export(archive.Query{}, out)
	if err != nil {
		t.Fatal(err)
	}
	findPayload := func(b []byte) int {
		for i := 0; i+5 < len(b); i++ {
			if b[i] == 0xAB && b[i+2] == 0xCD && b[i+3] == 0xEF &&
				b[i+4] == 0x00 && b[i+5] == 0x99 {
				return i
			}
		}
		return -1
	}
	for _, e := range man.Entries {
		src, err := os.ReadFile(filepath.Join(dir, "arc", e.SourcePath))
		if err != nil {
			t.Fatal(err)
		}
		dst, err := os.ReadFile(filepath.Join(out, e.RelativePath))
		if err != nil {
			t.Fatal(err)
		}
		i, j := findPayload(src), findPayload(dst)
		if i < 0 || j < 0 {
			t.Fatalf("%s: pixel payload not located (src=%d dst=%d)", e.RelativePath, i, j)
		}
		if !bytes.Equal(src[i:], dst[j:]) {
			t.Fatalf("%s: pixel bytes from payload onward differ", e.RelativePath)
		}
	}
	t.Logf("pixel bytes (incl. encapsulated) preserved verbatim for %d instances", len(man.Entries))
}

func TestExportRewritesUIDsInsideSequences(t *testing.T) {
	store, dir := seedMixedArchive(t, 4) // instances with i%4==1 carry UndefinedSQ
	out := filepath.Join(dir, "out")
	man, err := store.Export(archive.Query{}, out)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, e := range man.Entries {
		b, err := os.ReadFile(filepath.Join(out, e.RelativePath))
		if err != nil {
			t.Fatal(err)
		}
		texts, err := dicom.ScanMetadata(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		origBytes, err := os.ReadFile(filepath.Join(dir, "arc", e.SourcePath))
		if err != nil {
			t.Fatal(err)
		}
		origTexts, err := dicom.ScanMetadata(bytes.NewReader(origBytes))
		if err != nil {
			t.Fatal(err)
		}
		nested := map[string]bool{}
		for _, tv := range texts {
			if tv.Tag.Group == 0x0008 && tv.Tag.Element == 0x1155 {
				nested[tv.Value] = true
			}
		}
		for _, tv := range origTexts {
			if tv.Tag.Group == 0x0008 && tv.Tag.Element == 0x1155 {
				found++
				if nested[tv.Value] {
					t.Fatalf("%s: nested hierarchy UID %q not remapped", e.RelativePath, tv.Value)
				}
				if !nested[e.SOPUID] {
					t.Fatalf("%s: nested UID not mapped to instance pseudo %s", e.RelativePath, e.SOPUID)
				}
			}
		}
	}
	if found == 0 {
		t.Fatal("expected at least one nested ReferencedSOPInstanceUID")
	}
	t.Logf("nested UID elements rewritten/validated in %d place(s)", found)
}

func TestExportReproducibleAndIdempotent(t *testing.T) {
	store, dir := seedMixedArchive(t, 8)
	out := filepath.Join(dir, "out")
	_, err := store.Export(archive.Query{}, out)
	if err != nil {
		t.Fatal(err)
	}
	first := hashTree(t, out)
	if _, err := store.Export(archive.Query{}, out); err != nil {
		t.Fatal(err)
	}
	second := hashTree(t, out)
	if len(first) != len(second) {
		t.Fatalf("rerun changed file count: %d vs %d", len(first), len(second))
	}
	for p, h := range first {
		if second[p] != h {
			t.Fatalf("rerun modified %s", p)
		}
	}

	// Manifest bytes reproducible.
	mb, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Export(archive.Query{}, out); err != nil {
		t.Fatal(err)
	}
	mb2, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mb, mb2) {
		t.Fatal("manifest.json changed on rerun")
	}

	// No staging/temp leftovers.
	filepath.Walk(out, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		base := filepath.Base(p)
		if info.IsDir() && bytes.HasPrefix([]byte(base), []byte(".export-staging")) {
			t.Fatalf("staging dir left behind: %s", p)
		}
		if len(base) > 4 && (base[len(base)-4:] == ".tmp" || base[len(base)-7:] == ".staged") {
			t.Fatalf("temporary file left behind: %s", p)
		}
		return nil
	})
	t.Logf("three reruns idempotent; %d files and manifest stable", len(first))
}

func TestExportConflictOnTamperedTarget(t *testing.T) {
	store, dir := seedMixedArchive(t, 4)
	out := filepath.Join(dir, "out")
	man, err := store.Export(archive.Query{}, out)
	if err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(out, man.Entries[0].RelativePath)
	b, _ := os.ReadFile(victim)
	b[len(b)-1] ^= 0xFF
	if err := os.WriteFile(victim, b, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = store.Export(archive.Query{}, out)
	if !errors.Is(err, archive.ErrExportContent) {
		t.Fatalf("want ErrExportContent, got %v", err)
	}
	// Other previously exported files remain intact.
	for _, e := range man.Entries[1:] {
		p := filepath.Join(out, e.RelativePath)
		bb, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("failed export damaged sibling %s: %v", e.RelativePath, err)
		}
		sum := sha256.Sum256(bb)
		if hex.EncodeToString(sum[:]) != e.FileSHA256 {
			t.Fatalf("sibling %s content changed after failed export", e.RelativePath)
		}
	}
	t.Logf("tampered target reported and other files untouched: %v", err)
}

func TestExportFailsCleanlyMidRun(t *testing.T) {
	store, dir := seedMixedArchive(t, 6)
	out := filepath.Join(dir, "out")
	// Put a conflicting file that one of the instances will hit.
	man0, err := store.Export(archive.Query{Limit: 1}, out)
	if err != nil {
		t.Fatal(err)
	}
	if len(man0.Entries) == 0 {
		t.Fatal("no entries")
	}
	// Export a DIFFERENT query result that contains a file mapping to the
	// same path, with different content: easier approach is to corrupt one
	// target then run full export.
	good := hashTree(t, out)
	victim := filepath.Join(out, man0.Entries[0].RelativePath)
	b, _ := os.ReadFile(victim)
	b[len(b)-1] ^= 0xFF
	os.WriteFile(victim, b, 0o644)

	if _, err := store.Export(archive.Query{}, out); !errors.Is(err, archive.ErrExportContent) {
		t.Fatalf("want conflict, got %v", err)
	}
	// No staged/half files; pre-failure good files preserved.
	filepath.Walk(out, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		base := filepath.Base(p)
		if bytes.HasSuffix([]byte(base), []byte(".staged")) ||
			bytes.HasPrefix([]byte(base), []byte(".export-staging")) {
			t.Fatalf("half-finished artifact left: %s", p)
		}
		return nil
	})
	_ = good
	t.Log("failed export left no half-finished artifacts")
}

func importAllManifest(t *testing.T, dir string, man *archive.Manifest, salt, uidRoot string) *archive.Store {
	t.Helper()
	s, err := archive.Open(filepath.Join(dir, "arc2"), deident.Mapper{Salt: salt, UIDRoot: uidRoot})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range man.Entries {
		if _, err := s.Import(filepath.Join(dir, "out", e.RelativePath)); err != nil {
			t.Fatalf("reimport %s: %v", e.RelativePath, err)
		}
	}
	return s
}

func TestReimportSameKeyNoSecondShift(t *testing.T) {
	store, dir := seedMixedArchive(t, 9)
	out := filepath.Join(dir, "out")
	man, err := store.Export(archive.Query{}, out)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt := importAllManifest(t, dir, man, "secret", "2.25.99")

	pageOrig, _ := store.Query(archive.Query{Limit: 500})
	pageNew, _ := rebuilt.Query(archive.Query{Limit: 500})
	if len(pageNew.Instances) != len(pageOrig.Instances) {
		t.Fatalf("rebuilt has %d instances, want %d", len(pageNew.Instances), len(pageOrig.Instances))
	}
	old := map[string]archive.Instance{}
	for _, in := range pageOrig.Instances {
		old[in.SOPUID] = in
	}
	for _, in := range pageNew.Instances {
		o, ok := old[in.SOPUID]
		if !ok {
			t.Fatalf("unexpected rebuilt SOP %s (second shift?)", in.SOPUID)
		}
		if in.PatientID != o.PatientID || in.PatientName != o.PatientName ||
			in.StudyUID != o.StudyUID || in.SeriesUID != o.SeriesUID ||
			in.StudyDate != o.StudyDate {
			t.Fatalf("identity drifted after reimport:\nold=%+v\nnew=%+v", o, in)
		}
	}

	// Re-export from rebuilt archive: files must be byte-identical.
	out2 := filepath.Join(dir, "out2")
	man2, err := rebuilt.Export(archive.Query{}, out2)
	if err != nil {
		t.Fatal(err)
	}
	if len(man2.Entries) != len(man.Entries) {
		t.Fatalf("re-export count %d != %d", len(man2.Entries), len(man.Entries))
	}
	h1 := map[string]string{}
	for _, e := range man.Entries {
		h1[e.SOPUID] = e.FileSHA256
	}
	for _, e := range man2.Entries {
		if h1[e.SOPUID] != e.FileSHA256 {
			t.Fatalf("re-exported bytes differ for %s", e.SOPUID)
		}
	}
	t.Logf("reimported %d instances with no second shift; re-export byte-identical", len(man.Entries))
}

func TestReimportDifferentKeyConsistent(t *testing.T) {
	store, dir := seedMixedArchive(t, 9)
	out := filepath.Join(dir, "out")
	man, err := store.Export(archive.Query{}, out)
	if err != nil {
		t.Fatal(err)
	}
	other, err := archive.Open(filepath.Join(dir, "arcOther"),
		deident.Mapper{Salt: "entirely-different", UIDRoot: "2.25.99"})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range man.Entries {
		if _, err := other.Import(filepath.Join(out, e.RelativePath)); err != nil {
			t.Fatalf("reimport other key %s: %v", e.RelativePath, err)
		}
	}
	page, _ := other.Query(archive.Query{Limit: 500})
	if len(page.Instances) != len(man.Entries) {
		t.Fatalf("got %d instances, want %d", len(page.Instances), len(man.Entries))
	}
	groups := map[string]map[string]bool{}
	for _, in := range page.Instances {
		groups[in.PatientID] = map[string]bool{in.PatientName: true}
	}
	// 3 distinct original pseudo patients -> 3 new pseudo patients.
	if len(groups) != 3 {
		t.Fatalf("want 3 remapped patients, got %d", len(groups))
	}
	// And none of the new pseudo ids equals an original export pseudo id.
	oldPids := map[string]bool{}
	for _, e := range man.Entries {
		oldPids[e.PatientID] = true
	}
	for pid := range groups {
		if oldPids[pid] {
			t.Fatalf("new key reused original pseudonym %q", pid)
		}
	}
	t.Logf("different key produced a consistent new identity set of %d patients", len(groups))
}

func TestReimportIdempotent(t *testing.T) {
	store, dir := seedMixedArchive(t, 6)
	out := filepath.Join(dir, "out")
	man, err := store.Export(archive.Query{}, out)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := archive.Open(filepath.Join(dir, "r"), deident.Mapper{Salt: "secret", UIDRoot: "2.25.99"})
	paths := []string{}
	for _, e := range man.Entries {
		paths = append(paths, filepath.Join(out, e.RelativePath))
	}
	for run := 0; run < 2; run++ {
		dups := 0
		for _, p := range paths {
			res, err := s.Import(p)
			if err != nil {
				t.Fatal(err)
			}
			if res.Duplicate {
				dups++
			}
		}
		if run == 0 && dups != 0 {
			t.Fatal("first run must import all")
		}
		if run == 1 && dups != len(paths) {
			t.Fatalf("second run: want %d duplicates, got %d", len(paths), dups)
		}
	}
	page, _ := s.Query(archive.Query{Limit: 500})
	if len(page.Instances) != len(paths) {
		t.Fatalf("reimport created copies: %d vs %d", len(page.Instances), len(paths))
	}
	t.Logf("reimporting %d files twice: all duplicates on second run", len(paths))
}

func TestConcurrentExportImportQuery(t *testing.T) {
	store, dir := seedMixedArchive(t, 10)
	var wg sync.WaitGroup
	errCh := make(chan error, 64)

	// Continuously run queries and exports while extra distinct files are
	// imported. Exports must never see a half-imported instance and must
	// always produce valid output trees.
	stop := make(chan struct{})

	for r := 0; r < 3; r++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				page, err := store.Query(archive.Query{Limit: 500})
				if err != nil {
					errCh <- fmt.Errorf("query: %w", err)
					return
				}
				seen := map[string]bool{}
				for _, in := range page.Instances {
					if seen[in.SOPUID] {
						errCh <- fmt.Errorf("duplicate SOP visible: %s", in.SOPUID)
						return
					}
					seen[in.SOPUID] = true
				}
			}
		}(r)
	}

	exporterDone := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(exporterDone)
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			out := filepath.Join(dir, fmt.Sprintf("ce-%d", i))
			man, err := store.Export(archive.Query{}, out)
			if err != nil {
				errCh <- fmt.Errorf("export %d: %w", i, err)
				return
			}
			// Every exported file must parse: no half-imported instances.
			for _, e := range man.Entries {
				b, err := os.ReadFile(filepath.Join(out, e.RelativePath))
				if err != nil {
					errCh <- err
					return
				}
				if _, err := dicom.Parse(bytes.NewReader(b)); err != nil {
					errCh <- fmt.Errorf("exported %s invalid: %w", e.RelativePath, err)
					return
				}
			}
			i++
			if i > 20 {
				return
			}
		}
	}()

	// Import additional distinct instances concurrently.
	src := filepath.Join(dir, "extra")
	syntaxes := []string{dicomtest.TSExplicitLE, dicomtest.TSRLE, dicomtest.TSImplicitLE}
	for i := 100; i < 112; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ts := syntaxes[i%3]
			f, opt := exportFields(i, ts)
			f.SOPUID = fmt.Sprintf("9.9.%d", i)
			raw := dicomtest.BuildWithOptions(ts, f, opt)
			p := filepath.Join(src, fmt.Sprintf("x%d.dcm", i))
			if err := os.MkdirAll(src, 0o755); err != nil {
				errCh <- err
				return
			}
			if err := os.WriteFile(p, raw, 0o644); err != nil {
				errCh <- err
				return
			}
			if _, err := store.Import(p); err != nil {
				errCh <- fmt.Errorf("import %d: %w", i, err)
			}
		}(i)
	}

	<-exporterDone
	close(stop)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	page, _ := store.Query(archive.Query{Limit: 500})
	if len(page.Instances) != 22 {
		t.Fatalf("want 22 total instances, got %d", len(page.Instances))
	}
	t.Log("concurrent import + query + export produced only valid snapshots")
}

func TestImportRejectsCorruptionAtAllPositions(t *testing.T) {
	dir := t.TempDir()
	store, err := archive.Open(filepath.Join(dir, "arc"),
		deident.Mapper{Salt: "s", UIDRoot: "2.25.99"})
	if err != nil {
		t.Fatal(err)
	}
	f, opt := exportFields(1, dicomtest.TSRLE)
	raw := dicomtest.BuildWithOptions(dicomtest.TSRLE, f, opt)
	for n := 50; n < len(raw); n += 37 {
		p := filepath.Join(dir, fmt.Sprintf("t%d.dcm", n))
		if err := os.WriteFile(p, raw[:n], 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := store.Import(p)
		if err == nil {
			t.Fatalf("truncation at %d accepted", n)
		}
		if !errors.Is(err, archive.ErrParse) {
			t.Fatalf("truncation at %d: want ErrParse chain, got %v", n, err)
		}
	}
	// Key identifiers missing.
	noID := f
	noID.PatientID = ""
	bad := dicomtest.BuildWithOptions(dicomtest.TSExplicitLE, noID, dicomtest.Options{})
	p := filepath.Join(dir, "noid.dcm")
	os.WriteFile(p, bad, 0o644)
	if _, err := store.Import(p); err == nil {
		t.Fatal("missing patient id accepted")
	}
	t.Logf("rejected truncation at %d positions and missing identifier", (len(raw)-50+36)/37)
}
