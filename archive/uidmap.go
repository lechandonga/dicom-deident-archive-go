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
	mu     sync.Mutex
	cache  map[string]string
}

func newUIDMapper(mapper deident.Mapper, meta *dicom.Meta) *uidMapper {
	u := &uidMapper{mapper: mapper, cache: map[string]string{}}
	// Seed identity UIDs. meta carries original values; the matching
	// pseudonyms are recovered by recomputing with the same mapper key.
	u.cache[meta.StudyInstanceUID] = mapper.StudyUID(meta.StudyInstanceUID)
	u.cache[meta.SeriesInstanceUID] = mapper.SeriesUID(meta.SeriesInstanceUID)
	u.cache[meta.SOPInstanceUID] = mapper.SOPUID(meta.SOPInstanceUID)
	return u
}

func (u *uidMapper) mapUID(kind, orig string) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if v, ok := u.cache[orig]; ok {
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
