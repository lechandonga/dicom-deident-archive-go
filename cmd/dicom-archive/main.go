// Command dicom-archive is a local DICOM de-identification and archive CLI.
//
//	dicom-archive import -archive DIR -salt SECRET <file-or-dir>...
//	dicom-archive query  -archive DIR [-patient P] [-modality CT] [-from D] [-to D] [-cursor C] [-limit N]
//	dicom-archive gensample -out DIR [-n 10]
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/lechandonga/dicom-deident-archive-go/archive"
	"github.com/lechandonga/dicom-deident-archive-go/deident"
	"github.com/lechandonga/dicom-deident-archive-go/dicomtest"
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
	case "gensample":
		err = cmdGenSample(os.Args[2:])
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
	fmt.Fprintln(os.Stderr, `usage:
  dicom-archive import -archive DIR -salt SECRET <file-or-dir>...
  dicom-archive query  -archive DIR [-patient P] [-modality M] [-from YYYYMMDD] [-to YYYYMMDD] [-cursor C] [-limit N]
  dicom-archive gensample -out DIR [-n 10]`)
}

func cmdImport(args []string) error {
	flg := flag.NewFlagSet("import", flag.ContinueOnError)
	dir := flg.String("archive", "archive", "archive root directory")
	salt := flg.String("salt", "", "de-identification salt (required, keep secret & constant)")
	uidRoot := flg.String("uid-root", "2.25", "UID root for synthesised UIDs")
	if err := flg.Parse(args); err != nil {
		return err
	}
	if *salt == "" {
		return errors.New("-salt is required")
	}
	store, err := archive.Open(*dir, deident.Mapper{Salt: *salt, UIDRoot: *uidRoot})
	if err != nil {
		return err
	}
	var files []string
	for _, root := range flg.Args() {
		st, err := os.Stat(root)
		if err != nil {
			return err
		}
		if st.IsDir() {
			filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					files = append(files, p)
				}
				return err
			})
		} else {
			files = append(files, root)
		}
	}
	var imported, dup, rejected int
	for _, f := range files {
		res, err := store.Import(f)
		switch {
		case err != nil:
			rejected++
			fmt.Printf("REJECT  %s\n        %v\n", f, err)
		case res.Duplicate:
			dup++
			fmt.Printf("DUP     %s (identical content already archived)\n", f)
		default:
			imported++
			fmt.Printf("OK      %s -> %s (patient %s, study %s, date %s)\n",
				f, res.Instance.Path, res.Instance.PatientID, res.Instance.StudyUID, res.Instance.StudyDate)
		}
	}
	fmt.Printf("summary: imported=%d duplicates=%d rejected=%d\n", imported, dup, rejected)
	if rejected > 0 {
		return fmt.Errorf("%d file(s) rejected", rejected)
	}
	return nil
}

func cmdQuery(args []string) error {
	flg := flag.NewFlagSet("query", flag.ContinueOnError)
	dir := flg.String("archive", "archive", "archive root directory")
	patient := flg.String("patient", "", "pseudonymised patient ID")
	modality := flg.String("modality", "", "modality filter, e.g. CT")
	from := flg.String("from", "", "study date from, YYYYMMDD")
	to := flg.String("to", "", "study date to, YYYYMMDD")
	cursor := flg.String("cursor", "", "pagination cursor")
	limit := flg.Int("limit", 50, "page size")
	if err := flg.Parse(args); err != nil {
		return err
	}
	store, err := archive.Open(*dir, deident.Mapper{})
	if err != nil {
		return err
	}
	page, err := store.Query(archive.Query{
		PatientID: *patient, Modality: strings.ToUpper(*modality),
		DateFrom: *from, DateTo: *to, Cursor: *cursor, Limit: *limit,
	})
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(page.Instances); err != nil {
		return err
	}
	if page.NextCursor != "" {
		fmt.Println("next-cursor:", page.NextCursor)
	}
	return nil
}

func cmdGenSample(args []string) error {
	flg := flag.NewFlagSet("gensample", flag.ContinueOnError)
	out := flg.String("out", "sample", "output directory")
	n := flg.Int("n", 10, "number of instances")
	if err := flg.Parse(args); err != nil {
		return err
	}
	syntaxes := []string{dicomtest.TSImplicitLE, dicomtest.TSExplicitLE, dicomtest.TSExplicitBE}
	for i := 0; i < *n; i++ {
		f := dicomtest.Fields{
			PatientID:    fmt.Sprintf("PAT-%03d", i%3),
			PatientName:  fmt.Sprintf("Demo^Patient%d", i%3),
			BirthDate:    "19750101",
			Sex:          "M",
			StudyUID:     fmt.Sprintf("1.2.826.0.1.3680043.9.7.%d.1", i%4),
			StudyDate:    fmt.Sprintf("2024%02d15", i%12+1),
			StudyID:      "DEMO",
			SeriesUID:    fmt.Sprintf("1.2.826.0.1.3680043.9.7.%d.2", i),
			SeriesNumber: "1",
			Modality:     []string{"CT", "MR", "US"}[i%3],
			SOPUID:       fmt.Sprintf("1.2.826.0.1.3680043.9.7.%d.3", i),
			InstanceNo:   "1",
		}
		p := dicomtest.WriteFile(*out, fmt.Sprintf("sample-%03d.dcm", i), syntaxes[i%3], f)
		fmt.Println("wrote", p)
	}
	return nil
}
