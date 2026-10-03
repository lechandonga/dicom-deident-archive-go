package archive

import (
	"github.com/lechandonga/dicom-deident-archive-go/deident"
	"github.com/lechandonga/dicom-deident-archive-go/dicom"
)

// markerTags collected from simple header elements for provenance checks.
var (
	tagProvenance = dicom.Tag{Group: deident.ProvenanceTagGroup, Element: deident.ProvenanceTagElement}
	tagToken      = dicom.Tag{Group: deident.TokenTagGroup, Element: deident.TokenTagElement}
)

// metaValues returns simple element values relevant to provenance. The
// parser collects every simple header tag into Meta.Values, but the public
// Meta struct only exposes named fields; the markers are carried through
// MetaExtensions captured by the parser.
func metaValues(meta *dicom.Meta) []string {
	return meta.ExtensionValues(tagProvenance, tagToken)
}

// extractProvenance reads the provenance marker and token from metadata.
func extractProvenance(meta *dicom.Meta) (bool, string) {
	vals := meta.ExtensionValues(tagProvenance, tagToken)
	var hasProv bool
	var token string
	for _, v := range vals {
		if v == deident.ProvenanceValue {
			hasProv = true
		}
		if len(v) == 32 && isHex(v) {
			token = v
		}
	}
	return hasProv, token
}

func isHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
