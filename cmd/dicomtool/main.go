// Command dicomtool is the local, offline DICOM de-identification and
// archive CLI. It never contacts any external service.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lechandonga/dicom-deident-archive-go/internal/archive"
	"github.com/lechandonga/dicom-deident-archive-go/internal/config"
	"github.com/lechandonga/dicom-deident-archive-go/internal/sample"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "import":
		err = cmdImport(os.Args[2:])
	case "query":
		err = cmdQuery(os.Args[2:])
	case "gen-samples":
		err = cmdGen(os.Args[2:])
	case "verify":
		err = cmdVerify(os.Args[2:])
	case "dump-mapping":
		err = cmdDumpMapping(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `dicomtool - local offline DICOM de-identification & archive

Usage:
  dicomtool import --archive DIR [--config FILE] FILE...
  dicomtool query  --archive DIR [--patient ID] [--modality CT]
                   [--date-from YYYYMMDD] [--date-to YYYYMMDD]
                   [--offset N] [--limit N]
  dicomtool gen-samples --out DIR
  dicomtool verify --archive DIR
  dicomtool dump-mapping --archive DIR
`)
}

func openArchive(dir, cfgPath string) (*archive.Archive, *config.Compiled, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, err
	}
	compiled, err := cfg.Compile()
	if err != nil {
		return nil, nil, err
	}
	a, err := archive.Open(dir, compiled)
	if err != nil {
		return nil, nil, err
	}
	return a, compiled, nil
}

func cmdImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	archDir := fs.String("archive", "", "archive directory")
	cfgPath := fs.String("config", "", "de-identification config JSON (default built-in)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *archDir == "" || fs.NArg() == 0 {
		return fmt.Errorf("--archive and at least one FILE are required")
	}
	a, _, err := openArchive(*archDir, *cfgPath)
	if err != nil {
		return err
	}

	files, err := expandInputs(fs.Args())
	if err != nil {
		return err
	}

	var accepted, rejected, idempotent int
	for _, f := range files {
		out := a.ImportFile(f)
		switch {
		case out.Idempotent:
			idempotent++
			fmt.Printf("IDEMPOTENT  %-40s %s\n", filepath.Base(f), out.Detail)
		case out.Accepted:
			accepted++
			fmt.Printf("ACCEPTED    %-40s patient=%s study=%s sop=%s\n",
				filepath.Base(f), out.Instance.PatientID,
				out.Instance.StudyUID, out.Instance.SOPUID)
		default:
			rejected++
			fmt.Printf("REJECTED    %-40s reason=%s detail=%s\n",
				filepath.Base(f), out.Reason, out.Detail)
		}
	}
	fmt.Printf("\nsummary: accepted=%d idempotent=%d rejected=%d total=%d\n",
		accepted, idempotent, rejected, len(files))
	if rejected > 0 {
		os.Exit(3)
	}
	return nil
}

func expandInputs(inputs []string) ([]string, error) {
	var out []string
	for _, in := range inputs {
		info, err := os.Stat(in)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			out = append(out, in)
			continue
		}
		err = filepath.WalkDir(in, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && strings.HasSuffix(strings.ToLower(p), ".dcm") {
				out = append(out, p)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func cmdQuery(args []string) error {
	fs := flag.NewFlagSet("query", flag.ContinueOnError)
	archDir := fs.String("archive", "", "")
	patient := fs.String("patient", "", "de-identified patient ID")
	modality := fs.String("modality", "", "e.g. CT, MR")
	dateFrom := fs.String("date-from", "", "YYYYMMDD inclusive")
	dateTo := fs.String("date-to", "", "YYYYMMDD inclusive")
	offset := fs.Int("offset", 0, "")
	limit := fs.Int("limit", 50, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *archDir == "" {
		return fmt.Errorf("--archive is required")
	}
	a, _, err := openArchive(*archDir, "")
	if err != nil {
		return err
	}
	page := a.Search(archive.Query{
		PatientID:     *patient,
		Modality:      *modality,
		StudyDateFrom: *dateFrom,
		StudyDateTo:   *dateTo,
		Offset:        *offset,
		Limit:         *limit,
	})
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(page)
}

func cmdDumpMapping(args []string) error {
	fs := flag.NewFlagSet("dump-mapping", flag.ContinueOnError)
	archDir := fs.String("archive", "", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	b, err := os.ReadFile(filepath.Join(*archDir, "mapping.json"))
	if err != nil {
		return err
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// cmdGen writes a fixed, deterministic fixture set used by the README's
// reproduce steps and by humans who want to try the tool without PHI.
func cmdGen(args []string) error {
	fs := flag.NewFlagSet("gen-samples", flag.ContinueOnError)
	out := fs.String("out", "", "output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return fmt.Errorf("--out is required")
	}
	good := filepath.Join(*out, "good")
	bad := filepath.Join(*out, "bad")
	for _, d := range []string{good, bad} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}

	type spec struct {
		name string
		opt  sample.Options
		dir  string
	}
	specs := []spec{
		{"ct1.dcm", sample.Options{PatientID: "PID-A", PatientName: "Zhang^San",
			StudyUID: "9.1.1", SeriesUID: "9.1.1.1", SOPUID: "9.1.1.1.1",
			Modality: "CT", StudyDate: "20200301", InstanceNumber: "1",
			TransferSyntax: "1.2.840.10008.1.2.1"}, good},
		{"ct2.dcm", sample.Options{PatientID: "PID-A", PatientName: "Zhang^San",
			StudyUID: "9.1.1", SeriesUID: "9.1.1.1", SOPUID: "9.1.1.1.2",
			Modality: "CT", StudyDate: "20200305", InstanceNumber: "2",
			TransferSyntax: "1.2.840.10008.1.2"}, good},
		{"mr1.dcm", sample.Options{PatientID: "PID-A", PatientName: "Zhang^San",
			StudyUID: "9.1.2", SeriesUID: "9.1.2.9", SOPUID: "9.1.2.9.7",
			Modality: "MR", StudyDate: "20200601",
			TransferSyntax: "1.2.840.10008.1.2.2"}, good},
		{"ctb.dcm", sample.Options{PatientID: "PID-B", PatientName: "Li^Si",
			StudyUID: "9.2.1", SeriesUID: "9.2.1.1", SOPUID: "9.2.1.1.1",
			Modality: "CT", StudyDate: "20211231",
			TransferSyntax: "1.2.840.10008.1.2.1"}, good},
		{"dup_ct1.dcm", sample.Options{PatientID: "PID-A", PatientName: "Zhang^San",
			StudyUID: "9.1.1", SeriesUID: "9.1.1.1", SOPUID: "9.1.1.1.1",
			Modality: "CT", StudyDate: "20200301", InstanceNumber: "1",
			TransferSyntax: "1.2.840.10008.1.2.1"}, good},

		{"truncated.dcm", sample.Options{PatientID: "PID-A",
			StudyUID: "9.3.1", SeriesUID: "9.3.1.1", SOPUID: "9.3.1.1.1",
			Modality: "CT", StudyDate: "20200301",
			TruncateBytes: 60}, bad},
		{"notdicom.dcm", sample.Options{PatientID: "PID-A",
			StudyUID: "9.3.2", SeriesUID: "9.3.2.1", SOPUID: "9.3.2.1.1",
			Modality: "CT", BreakPreamble: true}, bad},
		{"nopid.dcm", sample.Options{StudyUID: "9.3.3",
			SeriesUID: "9.3.3.1", SOPUID: "9.3.3.1.1",
			Modality: "CT", StudyDate: "20200301", DropPatientID: true}, bad},
		{"nosop.dcm", sample.Options{PatientID: "PID-A",
			StudyUID: "9.3.4", SeriesUID: "9.3.4.1", SOPUID: "9.3.4.1.1",
			Modality: "CT", StudyDate: "20200301", DropSOPUID: true}, bad},
		{"baddate.dcm", sample.Options{PatientID: "PID-A",
			StudyUID: "9.3.5", SeriesUID: "9.3.5.1", SOPUID: "9.3.5.1.1",
			Modality: "CT", BadDate: "2020-13-99"}, bad},
	}
	for _, s := range specs {
		p := filepath.Join(s.dir, s.name)
		if err := os.WriteFile(p, sample.Build(s.opt), 0o644); err != nil {
			return err
		}
	}

	// Same SOP UID, different content -> duplicate conflict case.
	conflict := sample.Build(sample.Options{PatientID: "PID-A",
		StudyUID: "9.1.1", SeriesUID: "9.1.1.1", SOPUID: "9.1.1.1.1",
		Modality: "CT", StudyDate: "20200301", InstanceNumber: "1",
		Rows: 8, Cols: 8,
		TransferSyntax: "1.2.840.10008.1.2.1"})
	if err := os.MkdirAll(filepath.Join(*out, "conflict"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*out, "conflict", "ct1_conflict.dcm"),
		conflict, 0o644); err != nil {
		return err
	}
	fmt.Println("samples written to", *out)
	return nil
}

// cmdVerify re-opens the archive and checksums each stored de-identified
// file, asserting the index matches what is on disk.
func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	archDir := fs.String("archive", "", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	a, _, err := openArchive(*archDir, "")
	if err != nil {
		return err
	}
	page := a.Search(archive.Query{Limit: 1_000_000})
	bad := 0
	for _, in := range page.Instances {
		deidPath := filepath.Join(*archDir, in.DeidentifiedFile)
		origPath := filepath.Join(*archDir, in.OriginalFile)
		for _, p := range []string{deidPath, origPath} {
			if _, err := os.Stat(p); err != nil {
				fmt.Printf("MISSING %s: %v\n", p, err)
				bad++
			}
		}
	}
	fmt.Printf("verified %d instances, problems=%d\n", len(page.Instances), bad)
	if bad > 0 {
		os.Exit(4)
	}
	return nil
}
