package handlers

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm/clause"
)

// Mangaluru Mahanagara Palike grievance bot.
//
//	any text   -> menu (Garbage / Sewage reply buttons)
//	button     -> WhatsApp Flow form for that category
//	nfm_reply  -> row in mcc_grievances + confirmation with reference number
//
// The two Flows are created and published on Meta on first use and recorded
// in whatsapp_flows, so they also show up in the Flows screen.

const (
	mccFlowTokenPrefix = "mcc:"
	mccFlowCategory    = "OTHER"
	mccFlowCTA         = "Fill complaint form"
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
		a.saveMCCGrievance(account, contact, waMessageID, form, token, extracted.FlowResponseData)
		return true

	case "button_reply":
		form := mccFormByButton(extracted.ButtonID)
		if form == nil {
			return false
		}
		a.sendMCCForm(account, contact, form)
		return true

	case "text":
		a.sendMCCMenu(account, contact)
		return true
	}
	return false
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

func (a *App) sendMCCForm(account *models.WhatsAppAccount, contact *models.Contact, form *mccForm) {
	flow, err := a.ensureMCCFlow(account, form)
	if err != nil {
		a.Log.Error("MCC flow unavailable", "error", err, "form", form.MetaName)
		_ = a.sendAndSaveTextMessage(account, contact, "Sorry, the complaint form is temporarily unavailable. Please try again in a little while.")
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
		FlowToken:       fmt.Sprintf("%s%s:%s", mccFlowTokenPrefix, form.Category, uuid.NewString()),
		FlowFirstScreen: form.ScreenID,
		FlowMode:        mode,
	}, ChatbotSendOptions())
	if err != nil {
		a.Log.Error("Failed to send MCC form", "error", err, "form", form.MetaName, "contact", contact.PhoneNumber)
	}
}

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

	res := a.DB.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "wa_message_id"}}, DoNothing: true}).Create(&g)
	if res.Error != nil {
		a.Log.Error("Failed to save MCC grievance", "error", res.Error, "contact", contact.PhoneNumber)
		_ = a.sendAndSaveTextMessage(account, contact, "Sorry, we could not register your complaint right now. Please try again.")
		return
	}
	if res.RowsAffected == 0 {
		return
	}

	ref := fmt.Sprintf("%s%06d", form.RefPrefix, g.ID)
	if err := a.DB.Model(&g).Update("reference_no", ref).Error; err != nil {
		a.Log.Error("Failed to set MCC grievance reference", "error", err, "id", g.ID)
	}
	a.Log.Info("MCC grievance registered", "reference_no", ref, "category", g.Category, "ward", g.WardNo)

	if err := a.sendAndSaveTextMessage(account, contact, mccConfirmationText(ref, &g)); err != nil {
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
	}

	typeID := str("complaint_type")
	if t, ok := form.typeByID(typeID); ok {
		g.ComplaintTypeCode, g.ComplaintType = t.ID, t.Label
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
		mediaID, _ := m["id"].(string)
		if mediaID == "" {
			mediaID, _ = m["media_id"].(string)
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

func mccConfirmationText(ref string, g *models.MCCGrievance) string {
	area := g.WardName
	if g.WardNo > 0 {
		area = fmt.Sprintf("%s - %d", g.WardName, g.WardNo)
	}
	return fmt.Sprintf("✅ *Complaint registered*\n\n"+
		"Complaint No: *%s*\n"+
		"Type: %s\n"+
		"Area: %s\n\n"+
		"Please save this number for future reference. Send *hi* any time to register another complaint.",
		ref, g.ComplaintType, area)
}
