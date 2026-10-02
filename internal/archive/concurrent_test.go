package archive_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lechandonga/dicom-deident-archive-go/internal/archive"
)

// TestConcurrentImportAndQuery runs under -race. Invariants:
//   - queries never observe duplicate SOPs or missing backing files;
//   - every unique instance is imported exactly once (no omissions);
//   - identical-content concurrent imports resolve idempotently.
func TestConcurrentImportAndQuery(t *testing.T) {
	a := openTestArchive(t)
	dir := t.TempDir()

	const nWorkers = 8
	const perWorker = 10

	type file struct {
		path string
		sop  string
	}
	var uniques, duplicates []file
	for w := 0; w < nWorkers; w++ {
		for i := 0; i < perWorker; i++ {
			sop := fmt.Sprintf("9.%d.%d.1.1", w, i)
			b := validInstance("PID-A", fmt.Sprintf("9.%d.1", w),
				fmt.Sprintf("9.%d.1.1", w), sop, "20200301", "CT")
			p := writeFile(t, dir, fmt.Sprintf("w%d_i%d.dcm", w, i), b)
			uniques = append(uniques, file{p, sop})
			if i == perWorker-1 {
				p2 := writeFile(t, dir, fmt.Sprintf("dup_w%d.dcm", w), b)
				duplicates = append(duplicates, file{p2, sop})
			}
		}
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	recordErr := func(err error) {
		if err == nil {
			return
		}
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
	}

	stop := make(chan struct{})

	// Reader goroutine: continuously paginates and validates the snapshot.
	wg.Add(1)
	go func() {
		defer wg.Done()
		root := rootDir(a)
		for {
			select {
			case <-stop:
				return
			default:
			}
			page := a.Search(archive.Query{Limit: 64})
			seen := map[string]bool{}
			for _, in := range page.Instances {
				if seen[in.SOPUID] {
					recordErr(errors.New("query observed duplicate SOP " + in.SOPUID))
					return
				}
				seen[in.SOPUID] = true
				for _, rel := range []string{in.OriginalFile, in.DeidentifiedFile} {
					if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
						recordErr(fmt.Errorf("half-committed instance visible: %s (%v)",
							in.SOPUID, err))
						return
					}
				}
			}
			runtimeGosched()
		}
	}()

	// Importer goroutines: each owns a disjoint shard of unique files and
	// additionally races on the duplicate set.
	for w := 0; w < nWorkers; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				f := uniques[w*perWorker+i]
				if w == 0 && i == 0 {
					continue // racy "hot" file; asserted separately
				}
				o := a.ImportFile(f.path)
				if !o.Accepted {
					recordErr(fmt.Errorf("unique import %s failed: %s", f.sop, o.Detail))
					return
				}
				if o.Idempotent {
					recordErr(fmt.Errorf("unique import %s unexpectedly idempotent",
						f.sop))
					return
				}
			}
			if w < len(duplicates) {
				o := a.ImportFile(duplicates[w].path)
				if !o.Accepted {
					recordErr(fmt.Errorf("duplicate import rejected: %s", o.Detail))
					return
				}
				t.Logf("INPUT duplicate of %s -> idempotent=%v (race-safe)",
					duplicates[w].sop, o.Idempotent)
			}
		}()
	}

	// Also hammer one identical file from many goroutines simultaneously.
	hot := uniques[0]
	var hotWG sync.WaitGroup
	for k := 0; k < 12; k++ {
		hotWG.Add(1)
		go func() {
			defer hotWG.Done()
			o := a.ImportFile(hot.path)
			if !o.Accepted {
				recordErr(fmt.Errorf("hot duplicate rejected: %s", o.Detail))
			}
		}()
	}
	hotWG.Wait()

	// Wait for the shard importers, then stop the reader.
	done := make(chan struct{})
	go func() {
		// Importers are part of wg; wait on a subset by polling count.
		for a.InstanceCount() < nWorkers*perWorker {
			time.Sleep(time.Millisecond)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
	}
	close(stop)
	wg.Wait()

	if firstErr != nil {
		t.Fatal(firstErr)
	}
	if got := a.InstanceCount(); got != nWorkers*perWorker {
		t.Fatalf("instance count = %d, want exactly %d (no dup, no omission)",
			got, nWorkers*perWorker)
	}
	page := a.Search(archive.Query{Limit: 1000})
	seen := map[string]bool{}
	for _, in := range page.Instances {
		if seen[in.SOPUID] {
			t.Fatal("final index contains duplicate SOP")
		}
		seen[in.SOPUID] = true
	}
}

func runtimeGosched() { time.Sleep(time.Microsecond) }
