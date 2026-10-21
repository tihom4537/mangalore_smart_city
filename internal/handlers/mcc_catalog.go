package handlers

import (
	"fmt"

	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
)

// Static catalog for the Mangaluru Mahanagara Palike grievance forms.
// Option titles are what WhatsApp shows (Meta caps Dropdown titles at 30
// chars); Label is the official wording stored on the grievance. SLA is the
// resolution timeline quoted to the citizen; SLAHours (its upper bound)
// sets the grievance due date, 0 when unknown.

type mccOption struct {
	ID       string
	Title    string
	Label    string
	SLA      string
	SLAHours int
}

// mccSLAUnknown is quoted while MCC has not published a timeline.
const mccSLAUnknown = "X"

// mccForm describes one WhatsApp Flow form. Bump MetaName when the Flow JSON
// changes so a fresh flow is created on Meta instead of reusing the old one.
type mccForm struct {
	Category   string
	MetaName   string
	ScreenID   string
	ScreenName string
	RefPrefix  string
	ButtonID   string
	ButtonText string
	WithQR     bool
	Types      []mccOption
}

const mccJSONVersion = "6.0"

var mccSolidWasteTypes = []mccOption{
	{"door_to_door_missed", "Door-to-door collection missed", "Absenteeism of door to door garbage collection", "24 Hours", 24},
	{"garbage_burning", "Burning of garbage", "Burning of garbage", "12 Hours", 12},
	{"road_sweeping", "Sweeping of roads", "Sweeping of roads", "48 Hours", 48},
	{"roadside_dump", "Roadside garbage dump", "Clearance of roadside garbage dump", "24 Hours", 24},
	{"dead_animal", "Removal of dead animal(s)", "Removal of dead animal/animals", "12 Hours", 12},
	{"construction_debris", "Construction debris removal", "Removal of construction debris", "7 Days", 7 * 24},
	{"weed_removal", "Jungle cutting & weed removal", "Jungle cutting & Weed growth removal", "7 to 15 Days", 15 * 24},
	{"small_drains", "Cleaning of small drains", "Cleaning of small drains", "48 Hours to 7 Days", 7 * 24},
	{"other", "Others", "Others", "Up to 15 Days", 15 * 24},
}

var mccSewageTypes = []mccOption{
	{"sewer_line_damage", "Sewer line blockage/damage", "Blockage/pipe damage of Sewerage line Pipe", mccSLAUnknown, 0},
	{"manhole_overflow", "Manhole overflow", "Manhole Overflow", mccSLAUnknown, 0},
	{"household_sewer", "Household sewer blockage", "Blockage/pipe damage of household sewerage line", mccSLAUnknown, 0},
	{"other", "Other", "Other", mccSLAUnknown, 0},
}

// mccWards are the 60 MCC wards, index i = ward number i+1.
var mccWards = []string{
	"Surathkal (West)", "Surathkal (East)", "Katipalla-1 (East)", "Katipalla-2 (Krishnapur)",
	"Katipalla-3 (North)", "Idya-1 (East)", "Idya-2 (West)", "Hosabettu", "Kulai", "Baikampady",
	"Panambur", "Panjimogaru", "Kunjathbail (North)", "Marakada", "Kunjathbail (South)",
	"Bangrakulur", "Derebail (North)", "Kavoor", "Pachchanady", "Tiruvail",
	"Padavu-1 (West)", "Kadri Padavu", "Derebail (East)", "Derebail (South)", "Derebail (West)",
	"Derebail (Central)", "Boloor", "Mannagudda", "Kambla", "Kodialbail",
	"Bejai", "Kadri (North)", "Kadri (South)", "Shivabagh", "Padavu-2 (Central)",
	"Padavu-3 (East)", "Maroli", "Bendoor", "Falnir", "Court",
	"Central Market", "Dongarkeri", "Kudroli", "Bunder", "Port",
	"Cantonment", "Milagres", "Kankanady-Valencia", "Kankanady", "Alape (South)",
	"Alape (North)", "Kannur", "Bajal", "Jeppinamogaru", "Attavar",
	"Mangaladevi", "Hoigebazar", "Bolar", "Jeppu", "Bengre",
}

var mccForms = []mccForm{
	{
		Category:   models.MCCCategorySolidWaste,
		MetaName:   "MMP Garbage Complaint v2",
		ScreenID:   "GARBAGE_COMPLAINT",
		ScreenName: "Garbage Complaint",
		RefPrefix:  "MMP-SW-",
		ButtonID:   "mcc_solid_waste",
		ButtonText: "Garbage / Waste",
		WithQR:     true,
		Types:      mccSolidWasteTypes,
	},
	{
		Category:   models.MCCCategorySewage,
		MetaName:   "MMP Sewage Complaint v2",
		ScreenID:   "SEWAGE_COMPLAINT",
		ScreenName: "Sewage Complaint",
		RefPrefix:  "MMP-SG-",
		ButtonID:   "mcc_sewage",
		ButtonText: "Sewage / Drainage",
		Types:      mccSewageTypes,
	},
}

func mccFormByButton(buttonID string) *mccForm {
	for i := range mccForms {
		if mccForms[i].ButtonID == buttonID {
			return &mccForms[i]
		}
	}
	return nil
}

func mccFormByCategory(category string) *mccForm {
	for i := range mccForms {
		if mccForms[i].Category == category {
			return &mccForms[i]
		}
	}
	return nil
}

func (f *mccForm) typeByID(id string) (mccOption, bool) {
	for _, t := range f.Types {
		if t.ID == id {
			return t, true
		}
	}
	return mccOption{}, false
}

func mccWardID(n int) string { return fmt.Sprintf("ward_%d", n) }

// mccWardByID maps "ward_N" back to (N, name).
func mccWardByID(id string) (int, string, bool) {
	var n int
	if _, err := fmt.Sscanf(id, "ward_%d", &n); err != nil || n < 1 || n > len(mccWards) {
		return 0, "", false
	}
	return n, mccWards[n-1], true
}

// mccAddressMaxLen caps the pre-filled address below the TextArea limit.
const mccAddressMaxLen = 500

// screens builds the single-screen Flow layout for the form. The address is
// pre-filled from the citizen's shared location via ${data.address}.
func (f *mccForm) screens() []any {
	typeOptions := make([]any, 0, len(f.Types))
	for _, t := range f.Types {
		typeOptions = append(typeOptions, map[string]any{"id": t.ID, "title": t.Title})
	}
	wardOptions := make([]any, 0, len(mccWards))
	for i, name := range mccWards {
		wardOptions = append(wardOptions, map[string]any{
			"id":    mccWardID(i + 1),
			"title": fmt.Sprintf("%s - %d", name, i+1),
		})
	}

	children := []any{
		map[string]any{"type": "TextSubheading", "text": "Mangaluru Mahanagara Palike - One Touch Mangaluru"},
	}
	payload := map[string]any{
		"complaint_type": "${form.complaint_type}",
		"ward":           "${form.ward}",
		"address":        "${form.address}",
		"description":    "${form.description}",
		"photos":         "${form.photos}",
	}
	if f.WithQR {
		children = append(children, map[string]any{
			"type":        "TextInput",
			"name":        "qr_code",
			"label":       "QR code (optional)",
			"input-type":  "text",
			"required":    false,
			"helper-text": "Number on the QR sticker at your house. Leave blank if not applicable.",
		})
		payload["qr_code"] = "${form.qr_code}"
	}
	children = append(children,
		map[string]any{
			"type":        "Dropdown",
			"name":        "complaint_type",
			"label":       "Complaint type",
			"required":    true,
			"data-source": typeOptions,
		},
		map[string]any{
			"type":        "Dropdown",
			"name":        "ward",
			"label":       "Area / Ward",
			"required":    true,
			"data-source": wardOptions,
		},
		map[string]any{
			"type":        "TextArea",
			"name":        "address",
			"label":       "Address / Landmark",
			"required":    true,
			"init-value":  "${data.address}",
			"helper-text": "Filled from your location. Edit or add a landmark if needed.",
		},
		map[string]any{
			"type":     "TextArea",
			"name":     "description",
			"label":    "Grievance details",
			"required": true,
		},
		map[string]any{
			"type":                "PhotoPicker",
			"name":                "photos",
			"label":               "Photo",
			"description":         "Take a photo or upload from gallery (at least 1)",
			"photo-source":        "camera_gallery",
			"min-uploaded-photos": 1,
			"max-uploaded-photos": 3,
		},
		map[string]any{
			"type":  "Footer",
			"label": "Submit",
			"on-click-action": map[string]any{
				"name":    "complete",
				"payload": payload,
			},
		},
	)

	return []any{
		map[string]any{
			"id":       f.ScreenID,
			"title":    f.ScreenName,
			"terminal": true,
			"success":  true,
			"data": map[string]any{
				"address": map[string]any{"type": "string", "__example__": "Hampankatta, Mangaluru"},
			},
			"layout": map[string]any{
				"type":     "SingleColumnLayout",
				"children": children,
			},
		},
	}
}

func (f *mccForm) flowJSON() *whatsapp.FlowJSON {
	return &whatsapp.FlowJSON{Version: mccJSONVersion, Screens: f.screens()}
}
