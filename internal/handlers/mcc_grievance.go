package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Mangaluru Mahanagara Palike grievance bot.
//
//	any text   -> menu (Garbage / Sewage reply buttons)
//	button     -> "Send location" request for that category
//	location   -> WhatsApp Flow form, Address pre-filled from the location
//	"skip"     -> the same form without a pre-filled address
//	nfm_reply  -> row in mcc_grievances + confirmation with Complaint ID / SLA
//
// The step between messages is kept in mcc_sessions (one row per contact).
// The two Flows are created and published on Meta on first use and recorded
// in whatsapp_flows, so they also show up in the Flows screen.

const (
	mccFlowTokenPrefix = "mcc:"
	mccFlowCategory    = "OTHER"
	mccFlowCTA         = "Fill complaint form"
	// mccSessionTTL bounds how long a picked category waits for a location.
	mccSessionTTL = 30 * time.Minute
)

var (
	mccFlowMu sync.Mutex
	// mccPublishTried records draft flows we already retried publishing in
	// this process, so a flow Meta refuses to publish doesn't add an API
	// round-trip to every form open.
	mccPublishTried sync.Map
)

// handleMCCMessage routes an inbound message through the grievance bot.
// It returns true when the message was handled and normal chatbot
// processing should be skipped.
func (a *App) handleMCCMessage(account *models.WhatsAppAccount, contact *models.Contact, waMessageID string, extracted ExtractedMessage) bool {
	if !a.Config.MCC.Enabled {
		return false
	}

	switch extracted.Type {
	case "nfm_reply":
		token, _ := extracted.FlowResponseData["flow_token"].(string)
		rest, ok := strings.CutPrefix(token, mccFlowTokenPrefix)
		if !ok {
			return false
		}
		category, _, _ := strings.Cut(rest, ":")
		form := mccFormByCategory(category)
		if form == nil {
			a.Log.Warn("MCC flow reply with unknown category", "flow_token", token)
			return false
		}
		a.saveMCCGrievance(account, contact, waMessageID, form, token, mccFlowResponse(extracted))
		return true

	case "button_reply":
		form := mccFormByButton(extracted.ButtonID)
		if form == nil {
			return false
		}
		a.requestMCCLocation(account, contact, form)
		return true

	case "location":
		if form := a.pendingMCCForm(contact); form != nil {
			a.sendMCCForm(account, contact, form, a.resolveMCCLocation(extracted.Text))
			return true
		}
		a.sendMCCMenu(account, contact)
		return true

	case "text":
		// Any text while a location is awaited means "skip".
		if form := a.pendingMCCForm(contact); form != nil {
			a.sendMCCForm(account, contact, form, nil)
			return true
		}
		a.sendMCCMenu(account, contact)
		return true
	}
	return false
}

// mccFlowResponse re-decodes the raw response_json keeping numbers exact:
// Meta sends PhotoPicker media IDs as JSON numbers, which can exceed what
// float64 holds precisely.
func mccFlowResponse(extracted ExtractedMessage) map[string]any {
	if extracted.FlowResponseJSON == "" {
		return extracted.FlowResponseData
	}
	dec := json.NewDecoder(strings.NewReader(extracted.FlowResponseJSON))
	dec.UseNumber()
	var data map[string]any
	if err := dec.Decode(&data); err != nil {
		return extracted.FlowResponseData
	}
	return data
}

// pendingMCCForm returns the form a contact picked and is still sharing a
// location for, or nil when none is pending.
func (a *App) pendingMCCForm(contact *models.Contact) *mccForm {
	var session models.MCCSession
	if err := a.DB.Where("contact_id = ?", contact.ID).First(&session).Error; err != nil {
		return nil
	}
	if session.Stage != models.MCCStageAwaitingLocation || time.Since(session.UpdatedAt) > mccSessionTTL {
		return nil
	}
	return mccFormByCategory(session.Category)
}

func (a *App) saveMCCSession(session *models.MCCSession) {
	session.UpdatedAt = time.Now()
	err := a.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "contact_id"}},
		UpdateAll: true,
	}).Create(session).Error
	if err != nil {
		a.Log.Error("Failed to save MCC session", "error", err, "contact_id", session.ContactID)
	}
}

// requestMCCLocation asks for the citizen's location before sending the
// form. If the request can't be sent the form goes out straight away.
func (a *App) requestMCCLocation(account *models.WhatsAppAccount, contact *models.Contact, form *mccForm) {
	a.saveMCCSession(&models.MCCSession{
		ContactID:      contact.ID,
		OrganizationID: account.OrganizationID,
		Category:       form.Category,
		Stage:          models.MCCStageAwaitingLocation,
	})

	_, err := a.SendOutgoingMessage(context.Background(), OutgoingMessageRequest{
		Account:         account,
		Contact:         contact,
		Type:            models.MessageTypeInteractive,
		InteractiveType: "location_request",
		BodyText: "📍 Please share your current location so we can fill in the complaint address for you.\n\n" +
			"Tap *Send location* below, or type *skip* to enter the address yourself.",
	}, ChatbotSendOptions())
	if err != nil {
		a.Log.Error("Failed to send MCC location request", "error", err, "contact", contact.PhoneNumber)
		a.sendMCCForm(account, contact, form, nil)
	}
}

// mccLocation is a shared location and the address it resolved to.
type mccLocation struct {
	Latitude  float64
	Longitude float64
	Address   string
}

// resolveMCCLocation parses an extracted location message and reverse
// geocodes it. WhatsApp's own place name/address is the fallback.
func (a *App) resolveMCCLocation(raw string) *mccLocation {
	var in struct {
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
		Name      string  `json:"name"`
		Address   string  `json:"address"`
	}
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		a.Log.Warn("Unparseable MCC location", "error", err)
		return nil
	}
	loc := &mccLocation{Latitude: in.Latitude, Longitude: in.Longitude}

	ctx, cancel := context.WithTimeout(context.Background(), mccGeocodeTimeout)
	defer cancel()
	addr, err := a.reverseGeocodeMCC(ctx, in.Latitude, in.Longitude)
	if err != nil {
		a.Log.Warn("MCC reverse geocoding failed", "error", err, "lat", in.Latitude, "lon", in.Longitude)
		addr = strings.Trim(strings.TrimSpace(in.Name)+", "+strings.TrimSpace(in.Address), ", ")
	}
	loc.Address = truncateRunes(addr, mccAddressMaxLen)
	return loc
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func (a *App) sendMCCMenu(account *models.WhatsAppAccount, contact *models.Contact) {
	body := "🙏 Welcome to *Mangaluru Mahanagara Palike* – One Touch Mangaluru.\n\n" +
		"What would you like to report? Choose an option below."
	buttons := make([]map[string]any, 0, len(mccForms))
	for _, f := range mccForms {
		buttons = append(buttons, map[string]any{"id": f.ButtonID, "title": f.ButtonText})
	}
	if err := a.sendAndSaveInteractiveButtons(account, contact, body, buttons); err != nil {
		a.Log.Error("Failed to send MCC menu", "error", err, "contact", contact.PhoneNumber)
	}
}

// sendMCCForm sends the Flow form, pre-filling Address from loc when the
// citizen shared a location, and remembers the location for the submission.
func (a *App) sendMCCForm(account *models.WhatsAppAccount, contact *models.Contact, form *mccForm, loc *mccLocation) {
	session := &models.MCCSession{
		ContactID:      contact.ID,
		OrganizationID: account.OrganizationID,
		Category:       form.Category,
		Stage:          models.MCCStageFormSent,
		FlowToken:      fmt.Sprintf("%s%s:%s", mccFlowTokenPrefix, form.Category, uuid.NewString()),
	}
	if loc != nil {
		session.Latitude, session.Longitude = &loc.Latitude, &loc.Longitude
		session.DetectedAddress = loc.Address
	}
	a.saveMCCSession(session)

	flow, err := a.ensureMCCFlow(account, form)
	if err != nil {
		a.Log.Error("MCC flow unavailable", "error", err, "form", form.MetaName)
		_ = a.sendAndSaveTextMessage(account, contact, mccFormUnavailableText)
		return
	}

	mode := ""
	if flow.Status != "PUBLISHED" {
		mode = "draft"
	}
	_, err = a.SendOutgoingMessage(context.Background(), OutgoingMessageRequest{
		Account:         account,
		Contact:         contact,
		Type:            models.MessageTypeFlow,
		FlowID:          flow.MetaFlowID,
		FlowHeader:      form.ScreenName,
		BodyText:        "Tap the button below and fill in the details of your complaint.",
		FlowCTA:         mccFlowCTA,
		FlowToken:       session.FlowToken,
		FlowFirstScreen: form.ScreenID,
		FlowMode:        mode,
		FlowData:        map[string]any{"address": session.DetectedAddress},
	}, ChatbotSendOptions())
	if err != nil {
		a.Log.Error("Failed to send MCC form", "error", err, "form", form.MetaName, "contact", contact.PhoneNumber)
		_ = a.sendAndSaveTextMessage(account, contact, mccFormUnavailableText)
	}
}

const mccFormUnavailableText = "Sorry, the complaint form is temporarily unavailable. Please try again in a little while."

// ensureMCCFlow returns the Meta flow for form, creating, uploading and
// publishing it the first time. Existing flows (in our DB or on Meta) are
// reused by name.
func (a *App) ensureMCCFlow(account *models.WhatsAppAccount, form *mccForm) (*models.WhatsAppFlow, error) {
	mccFlowMu.Lock()
	defer mccFlowMu.Unlock()

	ctx := context.Background()
	waAccount := a.toWhatsAppAccount(account)

	var flow models.WhatsAppFlow
	err := a.DB.Where("organization_id = ? AND whats_app_account = ? AND name = ?",
		account.OrganizationID, account.Name, form.MetaName).First(&flow).Error
	if err == nil && flow.MetaFlowID != "" {
		if flow.Status != "PUBLISHED" {
			if _, tried := mccPublishTried.LoadOrStore(flow.MetaFlowID, true); !tried {
				if err := a.WhatsApp.PublishFlow(ctx, waAccount, flow.MetaFlowID); err != nil {
					a.Log.Warn("MCC flow still not publishable, using draft mode", "error", err, "flow", form.MetaName)
				} else {
					flow.Status = "PUBLISHED"
					a.DB.Model(&flow).Update("status", flow.Status)
				}
			}
		}
		return &flow, nil
	}

	// Not in our DB: reuse a same-named flow on Meta if one exists.
	metaFlowID, status := "", ""
	if existing, err := a.WhatsApp.ListFlows(ctx, waAccount); err == nil {
		for _, f := range existing {
			if f.Name == form.MetaName && f.Status != "DEPRECATED" {
				metaFlowID, status = f.ID, f.Status
				break
			}
		}
	}
	if metaFlowID == "" {
		metaFlowID, err = a.WhatsApp.CreateFlow(ctx, waAccount, form.MetaName, []string{mccFlowCategory})
		if err != nil {
			return nil, fmt.Errorf("create flow: %w", err)
		}
		status = "DRAFT"
	}
	if status != "PUBLISHED" {
		if err := a.WhatsApp.UpdateFlowJSON(ctx, waAccount, metaFlowID, form.flowJSON()); err != nil {
			return nil, fmt.Errorf("upload flow json: %w", err)
		}
		if err := a.WhatsApp.PublishFlow(ctx, waAccount, metaFlowID); err != nil {
			a.Log.Warn("MCC flow created but not published, using draft mode", "error", err, "flow", form.MetaName)
			mccPublishTried.Store(metaFlowID, true)
		} else {
			status = "PUBLISHED"
		}
	}

	screens := form.screens()
	flow = models.WhatsAppFlow{
		OrganizationID:  account.OrganizationID,
		WhatsAppAccount: account.Name,
		MetaFlowID:      metaFlowID,
		Name:            form.MetaName,
		Status:          status,
		Category:        mccFlowCategory,
		JSONVersion:     mccJSONVersion,
		FlowJSON:        models.JSONB{"version": mccJSONVersion, "screens": screens},
		Screens:         models.JSONBArray(screens),
		HasLocalChanges: false,
	}
	if err := a.DB.Create(&flow).Error; err != nil {
		return nil, fmt.Errorf("save flow: %w", err)
	}
	a.Log.Info("MCC flow ready", "flow", form.MetaName, "meta_flow_id", metaFlowID, "status", status)
	return &flow, nil
}

func (a *App) saveMCCGrievance(account *models.WhatsAppAccount, contact *models.Contact, waMessageID string, form *mccForm, flowToken string, data map[string]any) {
	// Meta retries webhooks; the first delivery already replied.
	var existing int64
	a.DB.Model(&models.MCCGrievance{}).Where("wa_message_id = ?", waMessageID).Count(&existing)
	if existing > 0 {
		return
	}

	g := buildMCCGrievance(form, data)
	g.OrganizationID = account.OrganizationID
	g.WhatsAppAccount = account.Name
	g.ContactID = contact.ID
	g.PhoneNumber = contact.PhoneNumber
	g.ProfileName = contact.ProfileName
	g.WAMessageID = waMessageID
	g.FlowToken = flowToken
	g.RawResponse = models.JSONB(data)
	g.PhotoURLs = a.downloadMCCPhotos(account, data["photos"])

	var session models.MCCSession
	err := a.DB.Where("contact_id = ? AND flow_token = ?", contact.ID, flowToken).First(&session).Error
	switch {
	case err == nil:
		g.Latitude, g.Longitude = session.Latitude, session.Longitude
		g.DetectedAddress = session.DetectedAddress
	case !errors.Is(err, gorm.ErrRecordNotFound):
		a.Log.Error("Failed to load MCC session", "error", err, "contact", contact.PhoneNumber)
	}

	res := a.DB.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "wa_message_id"}}, DoNothing: true}).Create(&g)
	if res.Error != nil {
		a.Log.Error("Failed to save MCC grievance", "error", res.Error, "contact", contact.PhoneNumber)
		_ = a.sendAndSaveTextMessage(account, contact, "Sorry, we could not register your complaint right now. Please try again.")
		return
	}
	if res.RowsAffected == 0 {
		return
	}
	a.DB.Where("contact_id = ? AND flow_token = ?", contact.ID, flowToken).Delete(&models.MCCSession{})

	ref := fmt.Sprintf("%s%06d", form.RefPrefix, g.ID)
	if err := a.DB.Model(&g).Update("reference_no", ref).Error; err != nil {
		a.Log.Error("Failed to set MCC grievance reference", "error", err, "id", g.ID)
	}
	a.Log.Info("MCC grievance registered", "reference_no", ref, "category", g.Category, "ward", g.WardNo)

	if err := a.sendAndSaveTextMessage(account, contact, mccConfirmationText(ref, g.SLA)); err != nil {
		a.Log.Error("Failed to send MCC confirmation", "error", err, "contact", contact.PhoneNumber)
	}
}

// buildMCCGrievance maps the Flow's response_json onto a grievance row.
// Unknown option IDs are kept verbatim rather than dropped.
func buildMCCGrievance(form *mccForm, data map[string]any) models.MCCGrievance {
	str := func(key string) string {
		v, _ := data[key].(string)
		return strings.TrimSpace(v)
	}

	g := models.MCCGrievance{
		Category:    form.Category,
		QRCode:      str("qr_code"),
		Address:     str("address"),
		Description: str("description"),
		Status:      "open",
		SLA:         mccSLAUnknown,
	}

	typeID := str("complaint_type")
	if t, ok := form.typeByID(typeID); ok {
		g.ComplaintTypeCode, g.ComplaintType, g.SLA = t.ID, t.Label, t.SLA
		if t.SLAHours > 0 {
			due := time.Now().Add(time.Duration(t.SLAHours) * time.Hour)
			g.DueAt = &due
		}
	} else {
		g.ComplaintTypeCode, g.ComplaintType = typeID, typeID
	}
	if n, name, ok := mccWardByID(str("ward")); ok {
		g.WardNo, g.WardName = n, name
	} else {
		g.WardName = str("ward")
	}
	return g
}

// downloadMCCPhotos saves PhotoPicker uploads locally. Flows that finish
// with a "complete" action deliver each photo as a regular media ID.
func (a *App) downloadMCCPhotos(account *models.WhatsAppAccount, raw any) models.JSONBArray {
	paths := models.JSONBArray{}
	items, _ := raw.([]any)
	if len(items) == 0 {
		return paths
	}

	waAccount := a.toWhatsAppAccount(account)
	for _, item := range items {
		m, _ := item.(map[string]any)
		mediaID := mccMediaID(m["id"])
		if mediaID == "" {
			mediaID = mccMediaID(m["media_id"])
		}
		if mediaID == "" {
			a.Log.Warn("MCC photo without media id", "photo", m)
			continue
		}
		mimeType, _ := m["mime_type"].(string)
		if mimeType == "" {
			mimeType = "image/jpeg"
		}
		path, err := a.DownloadAndSaveMedia(context.Background(), mediaID, mimeType, waAccount)
		if err != nil {
			a.Log.Error("Failed to download MCC photo", "error", err, "media_id", mediaID)
			continue
		}
		paths = append(paths, path)
	}
	return paths
}

// mccMediaID accepts a media ID sent as a string or a JSON number.
func mccMediaID(v any) string {
	switch id := v.(type) {
	case string:
		return id
	case json.Number:
		return id.String()
	case float64:
		return strconv.FormatFloat(id, 'f', -1, 64)
	}
	return ""
}

func mccConfirmationText(complaintID, sla string) string {
	return fmt.Sprintf("Dear Citizen, your grievance has been successfully registered.\n\n"+
		"Complaint ID: %s\n"+
		"Resolution Timeline: %s\n\n"+
		"You can use your Complaint ID to track the status of your grievance. "+
		"MCC will work towards resolving it within the stipulated timeline.\n\n"+
		"Thank you for bringing this to our attention.", complaintID, sla)
}
