package archive

import (
	"sync"

	"github.com/lechandonga/dicom-deident-archive-go/deident"
	"github.com/lechandonga/dicom-deident-archive-go/dicom"
)

// uidMapper rewrites UID-valued elements during export. Hierarchy UIDs are
// seeded from the parsed original metadata and forced to the index's
// pseudonyms, guaranteeing cross-file consistency. Every other UID value is
// mapped through the mapper's generic value space, with one stable result
// per original value so repeated occurrences stay identical.
type uidMapper struct {
	mapper deident.Mapper
	// identityPass is true when the source file already carries this
	// archive's pseudonyms (same-salt rebuilt archive): every UID passes
	// through unchanged instead of being mapped a second time.
	identityPass bool
	mu           sync.Mutex
	cache        map[string]string
}

func newUIDMapper(mapper deident.Mapper, meta *dicom.Meta, identityPass bool) *uidMapper {
	u := &uidMapper{mapper: mapper, cache: map[string]string{}, identityPass: identityPass}
	// Seed identity UIDs. meta carries the file's current values; the
	// index record carries the target pseudonyms. For original files the
	// targets are recomputed with the same mapper key; a same-salt rebuilt
	// file already equals its targets, so hierarchy seeds are identity.
	if identityPass {
		u.cache[meta.StudyInstanceUID] = meta.StudyInstanceUID
		u.cache[meta.SeriesInstanceUID] = meta.SeriesInstanceUID
		u.cache[meta.SOPInstanceUID] = meta.SOPInstanceUID
	} else {
		u.cache[meta.StudyInstanceUID] = mapper.StudyUID(meta.StudyInstanceUID)
		u.cache[meta.SeriesInstanceUID] = mapper.SeriesUID(meta.SeriesInstanceUID)
		u.cache[meta.SOPInstanceUID] = mapper.SOPUID(meta.SOPInstanceUID)
	}
	return u
}

func (u *uidMapper) mapUID(kind, orig string) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if v, ok := u.cache[orig]; ok {
		return v
	}
	if u.identityPass {
		v := orig
		u.cache[orig] = v
		return v
	}
	var v string
	switch kind {
	case "study":
		v = u.mapper.StudyUID(orig)
	case "series":
		v = u.mapper.SeriesUID(orig)
	case "sop":
		v = u.mapper.SOPUID(orig)
	default:
		v = u.mapper.UID(orig)
	}
	u.cache[orig] = v
	return v
}
