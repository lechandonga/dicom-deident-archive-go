package deident

import (
	"fmt"

	"github.com/lechandonga/dicom-deident-archive-go/internal/config"
	"github.com/lechandonga/dicom-deident-archive-go/internal/dicom"
)

// Transformer applies the configured rules to a parsed dataset.
type Transformer struct {
	cfg     *config.Compiled
	mapping *Mapping
}

func NewTransformer(cfg *config.Compiled, m *Mapping) *Transformer {
	return &Transformer{cfg: cfg, mapping: m}
}

// Result is the de-identified view of one instance.
type Result struct {
	PatientID string
	StudyUID  string
	SeriesUID string
	SOPUID    string
	Modality  string
	StudyDate string
	Meta      []*dicom.Element // rewritten group 0002
	Elements  []*dicom.Element // transformed dataset body
}

// Apply runs the crosswalk. It must be called with mapping held or under the
// importer lock; the caller serializes batch-wide consistency.
func (t *Transformer) Apply(ds *dicom.Dataset) (*Result, error) {
	origPatientID := ds.Find(dicom.TagPatientID).First()
	origStudyUID := ds.Find(dicom.TagStudyInstanceUID).First()
	origSeriesUID := ds.Find(dicom.TagSeriesInstanceUID).First()
	origSOPUID := ds.Find(dicom.TagSOPInstanceUID).First()

	newPatientID := t.mapping.MapPatientID(origPatientID)
	newStudyUID := t.mapping.MapStudyUID(origStudyUID)
	newSeriesUID := t.mapping.MapSeriesUID(origSeriesUID)
	newSOPUID := t.mapping.MapSOPUID(origSOPUID)

	out := &Result{
		PatientID: newPatientID,
		StudyUID:  newStudyUID,
		SeriesUID: newSeriesUID,
		SOPUID:    newSOPUID,
	}

	// Transform the body (and nested sequences).
	var body []*dicom.Element
	for _, el := range ds.Elements {
		transformed, keep, err := t.transformElement(el, origPatientID, newPatientID)
		if err != nil {
			return nil, err
		}
		if !keep {
			continue
		}
		if el.Tag.IsPrivate() && t.cfg.Raw.StripPrivate {
			continue
		}
		body = append(body, transformed)
	}

	// Mandatory de-identified identifiers (defensive; rules should map them).
	ensureUID(body, dicom.TagStudyInstanceUID, newStudyUID)
	ensureUID(body, dicom.TagSeriesInstanceUID, newSeriesUID)
	ensureUID(body, dicom.TagSOPInstanceUID, newSOPUID)
	ensureText(body, dicom.TagPatientID, newPatientID)

	if e := findElement(body, dicom.TagModality); e != nil {
		out.Modality = e.First()
	}
	if e := findElement(body, dicom.TagStudyDate); e != nil {
		out.StudyDate = e.First()
	}
	out.Elements = body

	// Rewrite file meta: same SOP class UID, mapped SOP instance UID,
	// normalized to explicit little endian; drop group lengths.
	out.Meta = t.rewriteFileMeta(ds.FileMeta, newSOPUID)
	return out, nil
}

func (t *Transformer) transformElement(el *dicom.Element, origPID, newPID string) (*dicom.Element, bool, error) {
	if t.cfg.GroupRemoved(el.Tag.Group) {
		return nil, false, nil
	}

	if el.VR == dicom.VRSQ {
		// Sequences holding patient identifiers are either dropped by rule
		// or recursed into (code sequences can contain names).
		if rule, ok := t.cfg.Lookup(el.Tag); ok && rule.Action == config.ActionRemove {
			return nil, false, nil
		}
		cp := *el
		cp.Items = nil
		for _, item := range el.Items {
			var els []*dicom.Element
			for _, child := range item.Elements {
				if child.Tag.IsPrivate() && t.cfg.Raw.StripPrivate {
					continue
				}
				tr, keep, err := t.transformElement(child, origPID, newPID)
				if err != nil {
					return nil, false, err
				}
				if keep {
					els = append(els, tr)
				}
			}
			cp.Items = append(cp.Items, dicom.Item{Elements: els})
		}
		return &cp, true, nil
	}

	rule, hasRule := t.cfg.Lookup(el.Tag)
	if hasRule {
		switch rule.Action {
		case config.ActionRemove:
			return nil, false, nil
		case config.ActionKeep:
			return el, true, nil
		case config.ActionEmpty:
			cp := *el
			cp.SetValues("")
			cp.Values = []string{""}
			return &cp, true, nil
		case config.ActionReplace:
			cp := *el
			cp.SetValues(rule.Value)
			return &cp, true, nil
		case config.ActionHash:
			cp := *el
			var nv string
			if el.Tag == dicom.TagPatientName {
				nv = t.mapping.MapPatientName(el.First())
			} else {
				nv = t.mapping.MapPatientID(origPID)
			}
			cp.SetValues(nv)
			return &cp, true, nil
		case config.ActionHashUID:
			cp := *el
			cp.SetValues(t.mapUIDByTag(el.Tag, el.First()))
			return &cp, true, nil
		case config.ActionShiftDate:
			if el.Tag == dicom.TagPatientBirthDate && t.cfg.Raw.KeepBirthDate {
				return el, true, nil
			}
			cp := *el
			vs := make([]string, 0, len(el.Values))
			for _, v := range el.Values {
				nv, err := t.mapping.ShiftDate(origPID, v)
				if err != nil {
					return nil, false, &dicom.RejectError{
						Reason: dicom.ReasonInvalidDate,
						Tag:    el.Tag.String(),
						Detail: fmt.Sprintf("cannot shift %q: %v", v, err),
					}
				}
				vs = append(vs, nv)
			}
			cp.SetValues(vs...)
			return &cp, true, nil
		}
	}

	// Pixel data and everything not explicitly configured round-trip
	// unchanged. Private tags are stripped by the caller when configured.
	cp := *el
	return &cp, true, nil
}

func (t *Transformer) mapUIDByTag(tag dicom.Tag, v string) string {
	switch tag {
	case dicom.TagStudyInstanceUID:
		return t.mapping.MapStudyUID(v)
	case dicom.TagSeriesInstanceUID:
		return t.mapping.MapSeriesUID(v)
	case dicom.TagSOPInstanceUID:
		return t.mapping.MapSOPUID(v)
	default:
		return t.mapping.MapSOPUID(v)
	}
}

func (t *Transformer) rewriteFileMeta(meta []*dicom.Element, newSOPUID string) []*dicom.Element {
	var out []*dicom.Element
	for _, el := range meta {
		if el.Tag.Element == 0x0000 {
			continue
		}
		cp := *el
		switch cp.Tag {
		case dicom.TagMediaStorageSOPInstanceUID:
			cp.SetValues(newSOPUID)
		case dicom.TagTransferSyntaxUID:
			cp.SetValues(dicom.TSExplicitLE)
		}
		out = append(out, &cp)
	}
	return out
}

func findElement(els []*dicom.Element, t dicom.Tag) *dicom.Element {
	for _, e := range els {
		if e.Tag == t {
			return e
		}
	}
	return nil
}

func ensureUID(els []*dicom.Element, t dicom.Tag, v string) {
	if e := findElement(els, t); e != nil {
		e.SetValues(v)
		return
	}
}

func ensureText(els []*dicom.Element, t dicom.Tag, v string) {
	if e := findElement(els, t); e != nil {
		e.SetValues(v)
	}
}
