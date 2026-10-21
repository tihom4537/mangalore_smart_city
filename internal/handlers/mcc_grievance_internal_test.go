package handlers

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/shridarpatil/whatomate/internal/models"
)

// Meta rejects Flow JSON that exceeds these limits at upload time, which
// would only surface in production when the first user opens a form.
func TestMCCFlowJSONWithinMetaLimits(t *testing.T) {
	for _, form := range mccForms {
		screens := form.screens()
		raw, err := json.Marshal(form.flowJSON())
		if err != nil {
			t.Fatalf("%s: marshal: %v", form.MetaName, err)
		}
		if !strings.Contains(string(raw), `"version":"`+mccJSONVersion+`"`) {
			t.Errorf("%s: missing version", form.MetaName)
		}

		screen := screens[0].(map[string]any)
		children := screen["layout"].(map[string]any)["children"].([]any)
		if last := children[len(children)-1].(map[string]any); last["type"] != "Footer" {
			t.Errorf("%s: Footer must be the last component, got %v", form.MetaName, last["type"])
		}

		names := map[string]bool{}
		for _, c := range children {
			comp := c.(map[string]any)
			if label, ok := comp["label"].(string); ok && comp["type"] != "Footer" && comp["type"] != "PhotoPicker" {
				if n := utf8.RuneCountInString(label); n > 20 {
					t.Errorf("%s: label %q is %d chars, max 20", form.MetaName, label, n)
				}
			}
			if name, ok := comp["name"].(string); ok {
				names[name] = true
			}
			if ds, ok := comp["data-source"].([]any); ok {
				if len(ds) > 200 {
					t.Errorf("%s: %s has %d options, max 200", form.MetaName, comp["name"], len(ds))
				}
				for _, o := range ds {
					title := o.(map[string]any)["title"].(string)
					if n := utf8.RuneCountInString(title); n > 30 {
						t.Errorf("%s: option %q is %d chars, max 30", form.MetaName, title, n)
					}
				}
			}
		}

		required := map[string]bool{}
		for _, c := range children {
			comp := c.(map[string]any)
			if name, _ := comp["name"].(string); name != "" {
				required[name], _ = comp["required"].(bool)
				if comp["type"] == "PhotoPicker" {
					required[name] = comp["min-uploaded-photos"] == 1
				}
				if name == "address" && comp["init-value"] != "${data.address}" {
					t.Errorf("%s: address not pre-filled from ${data.address}", form.MetaName)
				}
			}
		}
		for _, name := range []string{"complaint_type", "ward", "address", "description", "photos"} {
			if !required[name] {
				t.Errorf("%s: %s must be required", form.MetaName, name)
			}
		}
		if _, ok := screen["data"].(map[string]any)["address"]; !ok {
			t.Errorf("%s: screen must declare data.address", form.MetaName)
		}

		// Every payload key must reference a field on the screen.
		footer := children[len(children)-1].(map[string]any)
		payload := footer["on-click-action"].(map[string]any)["payload"].(map[string]any)
		for key, ref := range payload {
			if !names[key] || ref != "${form."+key+"}" {
				t.Errorf("%s: payload %q -> %v has no matching field", form.MetaName, key, ref)
			}
		}
	}
}

func TestMCCButtonTitlesFitWhatsApp(t *testing.T) {
	for _, form := range mccForms {
		if n := utf8.RuneCountInString(form.ButtonText); n > 20 {
			t.Errorf("button %q is %d chars, max 20", form.ButtonText, n)
		}
	}
	if n := utf8.RuneCountInString(mccFlowCTA); n > 20 {
		t.Errorf("flow CTA %q is %d chars, max 20", mccFlowCTA, n)
	}
}

func TestMCCSLAs(t *testing.T) {
	for _, form := range mccForms {
		for _, typ := range form.Types {
			if typ.SLA == "" {
				t.Errorf("%s/%s: empty SLA", form.Category, typ.ID)
			}
			if form.Category == models.MCCCategorySolidWaste && (typ.SLA == mccSLAUnknown || typ.SLAHours <= 0) {
				t.Errorf("%s: solid waste SLA missing", typ.ID)
			}
		}
	}

	msg := mccConfirmationText("MMP-SW-000007", "24 Hours")
	for _, want := range []string{
		"Dear Citizen, your grievance has been successfully registered.\n\nComplaint ID: MMP-SW-000007\nResolution Timeline: 24 Hours\n\n",
		"Thank you for bringing this to our attention.",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("confirmation missing %q:\n%s", want, msg)
		}
	}
}

func TestMCCWards(t *testing.T) {
	if len(mccWards) != 60 {
		t.Fatalf("expected 60 wards, got %d", len(mccWards))
	}
	n, name, ok := mccWardByID(mccWardID(38))
	if !ok || n != 38 || name != "Bendoor" {
		t.Errorf("ward_38 -> (%d, %q, %v)", n, name, ok)
	}
	for _, bad := range []string{"", "ward_0", "ward_61", "38", "ward_x"} {
		if _, _, ok := mccWardByID(bad); ok {
			t.Errorf("%q should not resolve", bad)
		}
	}
}

func TestBuildMCCGrievance(t *testing.T) {
	form := mccFormByCategory(models.MCCCategorySolidWaste)
	g := buildMCCGrievance(form, map[string]any{
		"flow_token":     "mcc:solid_waste:abc",
		"complaint_type": "roadside_dump",
		"ward":           "ward_43",
		"qr_code":        "  QR-123 ",
		"address":        "Near Kudroli temple",
		"description":    "Garbage not cleared for a week",
	})
	if g.Category != models.MCCCategorySolidWaste ||
		g.ComplaintTypeCode != "roadside_dump" ||
		g.ComplaintType != "Clearance of roadside garbage dump" ||
		g.WardNo != 43 || g.WardName != "Kudroli" ||
		g.QRCode != "QR-123" || g.Address != "Near Kudroli temple" || g.Status != "open" ||
		g.SLA != "24 Hours" || g.DueAt == nil {
		t.Errorf("unexpected grievance: %+v", g)
	}

	// Unknown option IDs are kept rather than dropped.
	g = buildMCCGrievance(mccFormByCategory(models.MCCCategorySewage), map[string]any{
		"complaint_type": "new_type",
		"ward":           "somewhere",
	})
	if g.ComplaintType != "new_type" || g.WardNo != 0 || g.WardName != "somewhere" || g.SLA != mccSLAUnknown || g.DueAt != nil {
		t.Errorf("unexpected fallback mapping: %+v", g)
	}
}

func TestMCCFormLookup(t *testing.T) {
	if f := mccFormByButton("mcc_sewage"); f == nil || f.Category != models.MCCCategorySewage {
		t.Errorf("mcc_sewage -> %+v", f)
	}
	if mccFormByButton("other_button") != nil || mccFormByCategory("nope") != nil {
		t.Error("unknown IDs must not match a form")
	}
}
