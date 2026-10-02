package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/lechandonga/dicom-deident-archive-go/internal/dicom"
)

// Rule actions applied to DICOM elements.
const (
	ActionKeep        = "keep"         // copy unchanged (default for non-identity tags)
	ActionRemove      = "remove"       // strip the element entirely
	ActionReplace     = "replace"      // set to a fixed string (config Value)
	ActionHash        = "hash"         // deterministic pseudonym (LO/SH/PN)
	ActionHashUID     = "hash-uid"     // deterministic UID mapping under OrgUID
	ActionEmpty       = "empty"        // keep element, zero-length value
	ActionShiftDate   = "shift-date"   // per-patient deterministic DA shift
	ActionRemoveGroup = "remove-group" // drop whole private/odd group (Tags ignored)
)

// Rule is one configured action.
type Rule struct {
	Tag    string `json:"tag"` // "(GGGG,EEEE)" or "group:GGGG"
	Action string `json:"action"`
	Value  string `json:"value,omitempty"` // for replace
}

// Config is the de-identification configuration.
type Config struct {
	// Salt is mixed into every HMAC so two sites with the same source data
	// but different salts produce different pseudonyms. Empty salt is
	// allowed for reproducible demos but discouraged.
	Salt string `json:"salt"`

	// OrgUID is the organization root for derived UIDs, e.g.
	// "1.2.840.113619.2.55". Derived UIDs are <OrgUID>.<digits> and always
	// stay within the 64-character DICOM UID limit.
	OrgUID string `json:"orgUID"`

	// DateShiftDays bounds the random-but-stable per-patient date shift.
	// The shift is a fixed whole-day offset chosen deterministically per
	// patient in [1, DateShiftDays]. 0 disables date shifting.
	DateShiftDays int `json:"dateShiftDays"`

	// KeepBirthDate keeps (0010,0030) unchanged; otherwise it is shifted by
	// the same per-patient offset (preserving intra-patient chronology).
	KeepBirthDate bool `json:"keepBirthDate"`

	// StripPrivate removes every private (odd-group) element.
	StripPrivate bool `json:"stripPrivate"`

	// Rules are applied in order after the built-in default rules. An
	// explicit rule for a tag wins over the default.
	Rules []Rule `json:"rules"`
}

// Compiled is a Config resolved to concrete tags.
type Compiled struct {
	Raw           Config
	tagAction     map[dicom.Tag]Rule
	groupRemovals map[uint16]bool
	DateTags      map[dicom.Tag]bool
	BirthDateTag  dicom.Tag
}

func ParseTag(s string) (dicom.Tag, error) {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "(", "")
	s = strings.ReplaceAll(s, ")", "")
	parts := strings.Split(s, ",")
	if len(parts) != 2 {
		return dicom.Tag{}, fmt.Errorf("invalid tag %q, want (GGGG,EEEE)", s)
	}
	g, err1 := strconv.ParseUint(strings.TrimSpace(parts[0]), 16, 16)
	e, err2 := strconv.ParseUint(strings.TrimSpace(parts[1]), 16, 16)
	if err1 != nil || err2 != nil {
		return dicom.Tag{}, fmt.Errorf("invalid tag %q", s)
	}
	return dicom.Tag{Group: uint16(g), Element: uint16(e)}, nil
}

// Default returns the built-in profile. Identity tags are removed/hashed;
// clinical and geometry tags are kept untouched.
func Default() Config {
	return Config{
		Salt:          "local-demo-salt",
		OrgUID:        "1.2.840.10008.99",
		DateShiftDays: 365,
		StripPrivate:  true,
		Rules: []Rule{
			{Tag: "(0010,0010)", Action: ActionHash},   // PatientName
			{Tag: "(0010,0020)", Action: ActionHash},   // PatientID
			{Tag: "(0010,0021)", Action: ActionRemove}, // IssuerOfPatientID
			{Tag: "(0010,0030)", Action: ActionShiftDate},
			{Tag: "(0010,1000)", Action: ActionRemove},    // OtherPatientIDsSequence
			{Tag: "(0010,1001)", Action: ActionRemove},    // OtherPatientNames
			{Tag: "(0010,1005)", Action: ActionRemove},    // PatientBirthName
			{Tag: "(0008,0080)", Action: ActionRemove},    // InstitutionName
			{Tag: "(0008,0090)", Action: ActionRemove},    // ReferringPhysicianName
			{Tag: "(0008,0070)", Action: ActionRemove},    // Manufacturer
			{Tag: "(0008,0020)", Action: ActionShiftDate}, // StudyDate
			{Tag: "(0008,0021)", Action: ActionShiftDate}, // SeriesDate
			{Tag: "(0008,0022)", Action: ActionShiftDate}, // AcquisitionDate
			{Tag: "(0008,0023)", Action: ActionShiftDate}, // ContentDate
			{Tag: "(0020,000D)", Action: ActionHashUID},   // StudyInstanceUID
			{Tag: "(0020,000E)", Action: ActionHashUID},   // SeriesInstanceUID
			{Tag: "(0008,0018)", Action: ActionHashUID},   // SOPInstanceUID
			{Tag: "(0008,1030)", Action: ActionRemove},    // StudyDescription
			{Tag: "(0008,1032)", Action: ActionRemove},    // ProcedureCodeSequence
			{Tag: "(0020,4000)", Action: ActionRemove},    // ImageComments
		},
	}
}

// Load reads a JSON config; path == "" returns the default profile.
func Load(path string) (*Config, error) {
	if path == "" {
		c := Default()
		return &c, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if len(c.Rules) == 0 {
		c = Default()
		if err := json.Unmarshal(b, &c); err != nil {
			return nil, err
		}
	}
	return &c, nil
}

// Compile resolves tag strings to dicom.Tag values.
func (c *Config) Compile() (*Compiled, error) {
	cc := &Compiled{
		Raw:           *c,
		tagAction:     map[dicom.Tag]Rule{},
		groupRemovals: map[uint16]bool{},
		DateTags:      map[dicom.Tag]bool{},
		BirthDateTag:  dicom.TagPatientBirthDate,
	}
	for i, r := range c.Rules {
		if strings.HasPrefix(r.Tag, "group:") {
			g, err := strconv.ParseUint(strings.TrimPrefix(r.Tag, "group:"), 16, 16)
			if err != nil {
				return nil, fmt.Errorf("rule %d: %w", i, err)
			}
			if r.Action != ActionRemoveGroup && r.Action != ActionRemove {
				return nil, fmt.Errorf("rule %d: group rule action must be remove/remove-group", i)
			}
			cc.groupRemovals[uint16(g)] = true
			continue
		}
		t, err := ParseTag(r.Tag)
		if err != nil {
			return nil, fmt.Errorf("rule %d: %w", i, err)
		}
		switch r.Action {
		case ActionKeep, ActionRemove, ActionReplace, ActionHash,
			ActionHashUID, ActionEmpty, ActionShiftDate:
		default:
			return nil, fmt.Errorf("rule %d: unknown action %q", i, r.Action)
		}
		cc.tagAction[t] = r
		if r.Action == ActionShiftDate {
			cc.DateTags[t] = true
		}
	}
	return cc, nil
}

// Lookup returns the rule for a tag and whether one exists.
func (c *Compiled) Lookup(t dicom.Tag) (Rule, bool) {
	r, ok := c.tagAction[t]
	return r, ok
}

// GroupRemoved reports whether the whole group must be dropped.
func (c *Compiled) GroupRemoved(g uint16) bool { return c.groupRemovals[g] }
