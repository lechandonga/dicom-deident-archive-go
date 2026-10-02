package dicom

import "fmt"

// Tag identifies a DICOM data element by (group, element).
type Tag struct {
	Group   uint16
	Element uint16
}

func (t Tag) String() string {
	return fmt.Sprintf("(%04X,%04X)", t.Group, t.Element)
}

// IsPrivate reports whether the tag belongs to a private creator block (odd group).
func (t Tag) IsPrivate() bool { return t.Group%2 == 1 }

// Key DICOM tags used across parsing, de-identification and indexing.
var (
	TagMediaStorageSOPClassUID    = Tag{0x0002, 0x0002}
	TagMediaStorageSOPInstanceUID = Tag{0x0002, 0x0003}
	TagTransferSyntaxUID          = Tag{0x0002, 0x0010}
	TagImplementationClassUID     = Tag{0x0002, 0x0012}
	TagSpecificCharacterSet       = Tag{0x0008, 0x0005}
	TagImageType                  = Tag{0x0008, 0x0008}
	TagSOPClassUID                = Tag{0x0008, 0x0016}
	TagSOPInstanceUID             = Tag{0x0008, 0x0018}
	TagStudyDate                  = Tag{0x0008, 0x0020}
	TagSeriesDate                 = Tag{0x0008, 0x0021}
	TagAcquisitionDate            = Tag{0x0008, 0x0022}
	TagContentDate                = Tag{0x0008, 0x0023}
	TagStudyTime                  = Tag{0x0008, 0x0030}
	TagModality                   = Tag{0x0008, 0x0060}
	TagManufacturer               = Tag{0x0008, 0x0070}
	TagInstitutionName            = Tag{0x0008, 0x0080}
	TagReferringPhysicianName     = Tag{0x0008, 0x0090}
	TagCodeMeaning                = Tag{0x0008, 0x0104}
	TagStudyDescription           = Tag{0x0008, 0x1030}
	TagProcedureCodeSequence      = Tag{0x0008, 0x1032}
	TagPatientName                = Tag{0x0010, 0x0010}
	TagPatientID                  = Tag{0x0010, 0x0020}
	TagIssuerOfPatientID          = Tag{0x0010, 0x0021}
	TagPatientBirthDate           = Tag{0x0010, 0x0030}
	TagPatientSex                 = Tag{0x0010, 0x0040}
	TagOtherPatientIDsSequence    = Tag{0x0010, 0x1000}
	TagPatientBirthName           = Tag{0x0010, 0x1005}
	TagOtherPatientNames          = Tag{0x0010, 0x1001}
	TagStudyInstanceUID           = Tag{0x0020, 0x000D}
	TagSeriesInstanceUID          = Tag{0x0020, 0x000E}
	TagStudyID                    = Tag{0x0020, 0x0010}
	TagSeriesNumber               = Tag{0x0020, 0x0011}
	TagInstanceNumber             = Tag{0x0020, 0x0013}
	TagPatientOrientation         = Tag{0x0020, 0x0035}
	TagImageComments              = Tag{0x0020, 0x4000}
	TagPixelData                  = Tag{0x7FE0, 0x0010}
)

// dateTags are all DA attributes whose values are shifted per-patient.
var dateTags = []Tag{
	TagStudyDate, TagSeriesDate, TagAcquisitionDate, TagContentDate,
}

// birthDateTag is shifted only when config.KeepBirthDate is false.
var birthDateTag = TagPatientBirthDate

// VR is the two-letter DICOM value representation.
type VR string

const (
	VRAE VR = "AE"
	VRAS VR = "AS"
	VRAT VR = "AT"
	VRCS VR = "CS"
	VRDA VR = "DA"
	VRDS VR = "DS"
	VRDT VR = "DT"
	VRFD VR = "FD"
	VRFL VR = "FL"
	VRIS VR = "IS"
	VRLO VR = "LO"
	VRLT VR = "LT"
	VRPN VR = "PN"
	VRSH VR = "SH"
	VRSL VR = "SL"
	VRSQ VR = "SQ"
	VRSS VR = "SS"
	VRST VR = "ST"
	VRTM VR = "TM"
	VRUI VR = "UI"
	VRUL VR = "UL"
	VRUS VR = "US"
	VRUT VR = "UT"
	VROB VR = "OB"
	VROW VR = "OW"
	VROF VR = "OF"
	VRUN VR = "UN"
)

// longHeaderVRs list VRs that carry a 16-bit reserved word before the length
// in explicit-VR transfer syntaxes.
var longHeaderVRs = map[VR]bool{
	VROB: true, VROW: true, VROF: true, VRSQ: true,
	VRUT: true, VRUN: true,
}

// implicitVR gives the VR implied by the data dictionary for implicit-little-endian.
func implicitVR(t Tag) VR {
	if v, ok := tagVR[t]; ok {
		return v
	}
	// Standard data dictionary fallback: group-length elements are UL.
	if t.Element == 0x0000 {
		return VRUL
	}
	return VRUN
}

// tagVR is the minimal data dictionary needed for correct parsing/writing.
var tagVR = map[Tag]VR{
	TagMediaStorageSOPClassUID:    VRUI,
	TagMediaStorageSOPInstanceUID: VRUI,
	TagTransferSyntaxUID:          VRUI,
	TagImplementationClassUID:     VRUI,
	TagSpecificCharacterSet:       VRCS,
	TagImageType:                  VRCS,
	TagSOPClassUID:                VRUI,
	TagSOPInstanceUID:             VRUI,
	TagStudyDate:                  VRDA,
	TagSeriesDate:                 VRDA,
	TagAcquisitionDate:            VRDA,
	TagContentDate:                VRDA,
	TagStudyTime:                  VRTM,
	TagModality:                   VRCS,
	TagManufacturer:               VRLO,
	TagInstitutionName:            VRLO,
	TagReferringPhysicianName:     VRPN,
	TagCodeMeaning:                VRLO,
	TagStudyDescription:           VRLO,
	TagProcedureCodeSequence:      VRSQ,
	TagPatientName:                VRPN,
	TagPatientID:                  VRLO,
	TagIssuerOfPatientID:          VRLO,
	TagPatientBirthDate:           VRDA,
	TagPatientSex:                 VRCS,
	TagOtherPatientIDsSequence:    VRSQ,
	TagPatientBirthName:           VRPN,
	TagOtherPatientNames:          VRPN,
	TagStudyInstanceUID:           VRUI,
	TagSeriesInstanceUID:          VRUI,
	TagStudyID:                    VRSH,
	TagSeriesNumber:               VRIS,
	TagInstanceNumber:             VRIS,
	TagPatientOrientation:         VRCS,
	TagImageComments:              VRLT,
}
