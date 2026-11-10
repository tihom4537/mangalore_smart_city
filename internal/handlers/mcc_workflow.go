package handlers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/contactutil"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
)

// Grievance workflow after a complaint is registered:
//
//	register   -> auto-assign to the ward's supervisor, send them the
//	              complaint (photos, map link, action buttons) on WhatsApp
//	supervisor -> "In progress" | "Close complaint" -> action-taken details
//	              (text and/or photos) -> status resolved
//	citizen    -> closure notice with the action taken; "Satisfied" closes
//	              it for good, "Reopen" asks why and sends it back to the
//	              supervisor
//
// Outside a recipient's 24-hour service window Meta only accepts approved
// templates, so [mcc] supervisor_template / citizen_closure_template are
// used there; their QUICK_REPLY buttons carry the same button IDs.

// Button ID prefixes; the rest is the grievance's serial ID.
const (
	mccBtnOpen     = "mcc_open:"     // supervisor: send me this complaint's card
	mccBtnProgress = "mcc_progress:" // supervisor: mark in progress
	mccBtnClose    = "mcc_close:"    // supervisor: close, then send details
	mccBtnOK       = "mcc_ok:"       // citizen: satisfied with the closure
	mccBtnReopen   = "mcc_reopen:"   // citizen: not satisfied, reopen
)

const (
	mccServiceWindow  = 24 * time.Hour
	mccInboxMax       = 10 // WhatsApp list rows
	mccCardTextMax    = 900
	mccDefaultMapsURL = "https://www.google.com/maps?q=%f,%f"
)

func mccHistoryEntry(status, by, note string) map[string]any {
	e := map[string]any{"status": status, "by": by, "at": time.Now().UTC().Format(time.RFC3339)}
	if note != "" {
		e["note"] = note
	}
	return e
}

// ---------------------------------------------------------------------------
// Assignment
// ---------------------------------------------------------------------------

// findMCCSupervisor picks the most specific active supervisor for a ward and
// category: ward+category, then ward only, then category only, then the
// catch-all (ward 0, no category).
func (a *App) findMCCSupervisor(orgID uuid.UUID, wardNo int, category string) (*models.MCCSupervisor, error) {
	var candidates []models.MCCSupervisor
	err := a.DB.Where("organization_id = ? AND is_active = ? AND (ward_no = ? OR ward_no = 0) AND (category = ? OR category = '')",
		orgID, true, wardNo, category).Order("created_at ASC").Find(&candidates).Error
	if err != nil {
		return nil, err
	}
	var best *models.MCCSupervisor
	bestScore := -1
	for i := range candidates {
		c := &candidates[i]
		score := 0
		if c.WardNo == wardNo && wardNo != 0 {
			score += 2
		}
		if c.Category == category && category != "" {
			score++
		}
		if score > bestScore {
			best, bestScore = c, score
		}
	}
	return best, nil
}

// assignMCCGrievance routes a freshly registered grievance to its
// supervisor and notifies them. Without a matching supervisor the
// grievance stays open with the reason recorded for the dashboard.
func (a *App) assignMCCGrievance(account *models.WhatsAppAccount, g *models.MCCGrievance) {
	sup, err := a.findMCCSupervisor(account.OrganizationID, g.WardNo, g.Category)
	if err != nil {
		a.Log.Error("Failed to look up MCC supervisor", "error", err, "grievance", g.ID)
		return
	}
	if sup == nil {
		a.Log.Warn("No MCC supervisor for grievance", "grievance", g.ID, "ward", g.WardNo, "category", g.Category)
		a.DB.Model(g).Update("notify_error", fmt.Sprintf("no supervisor configured for ward %d / %s", g.WardNo, g.Category))
		return
	}
	a.assignMCCGrievanceTo(account, g, sup, "auto")
}

// assignMCCGrievanceTo records sup as the grievance's supervisor and sends
// them the complaint. by is who made the assignment ("auto" or a user).
func (a *App) assignMCCGrievanceTo(account *models.WhatsAppAccount, g *models.MCCGrievance, sup *models.MCCSupervisor, by string) {
	supContact, _, err := contactutil.GetOrCreateContact(a.DB, account.OrganizationID, sup.PhoneNumber, sup.Name)
	if err != nil {
		a.Log.Error("Failed to get MCC supervisor contact", "error", err, "phone", sup.PhoneNumber)
		return
	}
	if supContact.WhatsAppAccount == "" {
		a.DB.Model(supContact).Update("whats_app_account", account.Name)
	}

	now := time.Now()
	g.SupervisorID = &sup.ID
	g.SupervisorName = sup.Name
	g.SupervisorPhone = sup.PhoneNumber
	g.SupervisorContactID = &supContact.ID
	g.AssignedAt = &now
	g.NotifyError = ""
	if g.Status == models.MCCStatusOpen {
		g.Status = models.MCCStatusAssigned
	}
	g.History = append(g.History, mccHistoryEntry(models.MCCStatusAssigned, by, "assigned to "+sup.Name))
	err = a.DB.Model(g).Select("supervisor_id", "supervisor_name", "supervisor_phone", "supervisor_contact_id",
		"assigned_at", "notify_error", "status", "history").Updates(g).Error
	if err != nil {
		a.Log.Error("Failed to assign MCC grievance", "error", err, "grievance", g.ID)
		return
	}
	a.Log.Info("MCC grievance assigned", "reference_no", mccRef(g), "supervisor", sup.Name, "ward", g.WardNo)

	a.notifyMCCSupervisor(account, g, supContact, "🆕 *New complaint assigned to you*")
}

// notifyMCCSupervisor sends the complaint to its supervisor, as the
// configured template when they are outside the service window, and
// records the outcome on the grievance.
func (a *App) notifyMCCSupervisor(account *models.WhatsAppAccount, g *models.MCCGrievance, supContact *models.Contact, headline string) {
	var err error
	if !mccInServiceWindow(supContact) && a.Config.MCC.SupervisorTemplate != "" {
		err = a.sendMCCSupervisorTemplate(account, supContact, g)
	} else {
		err = a.sendMCCComplaintCard(account, supContact, g, headline)
	}

	updates := map[string]any{"notify_error": ""}
	if err != nil {
		a.Log.Error("Failed to notify MCC supervisor", "error", err, "grievance", g.ID, "phone", supContact.PhoneNumber)
		updates["notify_error"] = err.Error()
	} else {
		updates["notified_at"] = time.Now()
	}
	a.DB.Model(g).Updates(updates)
}

func mccInServiceWindow(contact *models.Contact) bool {
	return contact.LastInboundAt != nil && time.Since(*contact.LastInboundAt) < mccServiceWindow
}

// sendMCCSupervisorTemplate sends the approved template; its first
// QUICK_REPLY button opens the complaint card once the supervisor taps it.
func (a *App) sendMCCSupervisorTemplate(account *models.WhatsAppAccount, contact *models.Contact, g *models.MCCGrievance) error {
	tpl, err := a.mccTemplate(account, a.Config.MCC.SupervisorTemplate)
	if err != nil {
		return err
	}
	return a.sendMCCMessage(OutgoingMessageRequest{
		Account:  account,
		Contact:  contact,
		Type:     models.MessageTypeTemplate,
		Template: tpl,
		BodyParams: map[string]string{
			"1": mccRef(g), "2": g.ComplaintType, "3": mccWardLabel(g),
			"4": mccOneLine(g.Address, 200), "5": mccOneLine(g.Description, 300), "6": g.SLA,
		},
		QuickReplyPayloads: mccQuickReplyPayloads(tpl, mccBtnOpen+mccIDString(g)),
	})
}

// mccTemplate loads an approved template by name for the account.
func (a *App) mccTemplate(account *models.WhatsAppAccount, name string) (*models.Template, error) {
	var tpl models.Template
	err := a.DB.Where("organization_id = ? AND whats_app_account = ? AND name = ?", account.OrganizationID, account.Name, name).
		Order("status = 'APPROVED' DESC").First(&tpl).Error
	if err != nil {
		return nil, fmt.Errorf("template %q: %w", name, err)
	}
	return &tpl, nil
}

// mccQuickReplyPayloads maps the template's QUICK_REPLY buttons, in order,
// onto the given payloads.
func mccQuickReplyPayloads(tpl *models.Template, payloads ...string) map[string]string {
	out := map[string]string{}
	n := 0
	for i, raw := range tpl.Buttons {
		btn, _ := raw.(map[string]any)
		if t, _ := btn["type"].(string); !strings.EqualFold(t, "QUICK_REPLY") {
			continue
		}
		if n >= len(payloads) {
			break
		}
		out[strconv.Itoa(i)] = payloads[n]
		n++
	}
	return out
}

// sendMCCMessage sends synchronously and reports Meta's rejection, which
// SendOutgoingMessage only records on the message row.
func (a *App) sendMCCMessage(req OutgoingMessageRequest) error {
	msg, err := a.SendOutgoingMessage(context.Background(), req, ChatbotSendOptions())
	if err != nil {
		return err
	}
	var sent models.Message
	if err := a.DB.Select("status", "error_message").Where("id = ?", msg.ID).First(&sent).Error; err != nil {
		return nil
	}
	if sent.Status == models.MessageStatusFailed {
		return errors.New(sent.ErrorMessage)
	}
	return nil
}

// sendMCCComplaintCard sends the complaint photos followed by the details
// with the supervisor's action buttons.
func (a *App) sendMCCComplaintCard(account *models.WhatsAppAccount, contact *models.Contact, g *models.MCCGrievance, headline string) error {
	ref := mccRef(g)
	for i, raw := range g.PhotoURLs {
		path, _ := raw.(string)
		data, mime, err := a.readMCCMedia(path)
		if err != nil {
			a.Log.Warn("MCC photo unreadable", "error", err, "path", path)
			continue
		}
		if err := a.sendMCCMessage(OutgoingMessageRequest{
			Account: account, Contact: contact, Type: models.MessageTypeImage,
			MediaData: data, MediaMimeType: mime, MediaURL: path,
			Caption: fmt.Sprintf("%s – photo %d/%d", ref, i+1, len(g.PhotoURLs)),
		}); err != nil {
			return fmt.Errorf("photo %d: %w", i+1, err)
		}
	}

	body := headline + "\n\n" + a.mccComplaintSummary(g)
	buttons := mccSupervisorButtons(g)
	if len(buttons) == 0 {
		return a.sendMCCMessage(OutgoingMessageRequest{Account: account, Contact: contact, Type: models.MessageTypeText, Content: body})
	}
	return a.sendMCCMessage(OutgoingMessageRequest{
		Account: account, Contact: contact, Type: models.MessageTypeInteractive,
		InteractiveType: "button", BodyText: body, Buttons: buttons,
	})
}

func mccSupervisorButtons(g *models.MCCGrievance) []whatsapp.Button {
	id := mccIDString(g)
	switch g.Status {
	case models.MCCStatusAssigned, models.MCCStatusReopened:
		return []whatsapp.Button{
			{ID: mccBtnProgress + id, Title: "🔧 In progress"},
			{ID: mccBtnClose + id, Title: "✅ Close complaint"},
		}
	case models.MCCStatusInProgress:
		return []whatsapp.Button{{ID: mccBtnClose + id, Title: "✅ Close complaint"}}
	}
	return nil
}

// mccComplaintSummary is the complaint as shown to the supervisor.
func (a *App) mccComplaintSummary(g *models.MCCGrievance) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Complaint ID: *%s*\n", mccRef(g))
	fmt.Fprintf(&b, "Type: %s\n", g.ComplaintType)
	fmt.Fprintf(&b, "Ward: %s\n", mccWardLabel(g))
	if g.QRCode != "" {
		fmt.Fprintf(&b, "QR code: %s\n", g.QRCode)
	}
	fmt.Fprintf(&b, "Address: %s\n", mccOneLine(g.Address, 200))
	fmt.Fprintf(&b, "Details: %s\n", mccOneLine(g.Description, 300))
	if g.Latitude != nil && g.Longitude != nil {
		fmt.Fprintf(&b, "Map: %s\n", a.mccMapsLink(*g.Latitude, *g.Longitude))
	}
	fmt.Fprintf(&b, "Citizen: %s (%s)\n", strings.TrimSpace(g.ProfileName), g.PhoneNumber)
	fmt.Fprintf(&b, "Filed: %s\n", g.CreatedAt.Local().Format("02 Jan 2006 15:04"))
	if g.DueAt != nil {
		fmt.Fprintf(&b, "Resolve by: %s (%s)\n", g.DueAt.Local().Format("02 Jan 2006 15:04"), g.SLA)
	} else {
		fmt.Fprintf(&b, "Resolution timeline: %s\n", g.SLA)
	}
	if g.ReopenCount > 0 {
		fmt.Fprintf(&b, "\n🔁 Reopened %d time(s). Citizen's reason: %s\n", g.ReopenCount, mccOneLine(g.ReopenReason, 200))
	}
	return truncateRunes(strings.TrimRight(b.String(), "\n"), mccCardTextMax)
}

func (a *App) mccMapsLink(lat, lon float64) string {
	format := a.Config.MCC.MapsURL
	if format == "" {
		format = mccDefaultMapsURL
	}
	return fmt.Sprintf(format, lat, lon)
}

func (a *App) readMCCMedia(relPath string) ([]byte, string, error) {
	if relPath == "" || strings.Contains(relPath, "..") {
		return nil, "", errors.New("invalid media path")
	}
	data, err := os.ReadFile(filepath.Join(a.getMediaStoragePath(), relPath))
	if err != nil {
		return nil, "", err
	}
	mime := "image/jpeg"
	if ext := strings.ToLower(filepath.Ext(relPath)); ext == ".png" {
		mime = "image/png"
	} else if ext == ".webp" {
		mime = "image/webp"
	}
	return data, mime, nil
}

func mccRef(g *models.MCCGrievance) string {
	if g.ReferenceNo != nil {
		return *g.ReferenceNo
	}
	return fmt.Sprintf("#%d", g.ID)
}

func mccIDString(g *models.MCCGrievance) string { return strconv.FormatUint(g.ID, 10) }

func mccWardLabel(g *models.MCCGrievance) string {
	if g.WardNo > 0 {
		return fmt.Sprintf("%d - %s", g.WardNo, g.WardName)
	}
	return g.WardName
}

func mccOneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) > max {
		return truncateRunes(s, max-1) + "…"
	}
	return s
}

// ---------------------------------------------------------------------------
// Supervisor side
// ---------------------------------------------------------------------------

// isMCCSupervisor reports whether a contact is an active supervisor.
func (a *App) isMCCSupervisor(orgID uuid.UUID, contact *models.Contact) bool {
	var n int64
	a.DB.Model(&models.MCCSupervisor{}).
		Where("organization_id = ? AND is_active = ? AND phone_number IN (?)", orgID, true, mccPhoneVariants(contact.PhoneNumber)).
		Count(&n)
	return n > 0
}

func mccPhoneVariants(phone string) []string {
	bare := strings.TrimPrefix(phone, "+")
	return []string{bare, "+" + bare}
}

// sendMCCSupervisorInbox lists the supervisor's unresolved complaints.
func (a *App) sendMCCSupervisorInbox(account *models.WhatsAppAccount, contact *models.Contact) {
	var open []models.MCCGrievance
	a.DB.Where("organization_id = ? AND supervisor_contact_id = ? AND status IN (?)",
		account.OrganizationID, contact.ID,
		[]string{models.MCCStatusAssigned, models.MCCStatusInProgress, models.MCCStatusReopened}).
		Order("created_at ASC").Limit(mccInboxMax).Find(&open)

	if len(open) == 0 {
		_ = a.sendAndSaveTextMessage(account, contact,
			"👋 You have no pending complaints right now.\n\nReply *"+mccNewComplaintKeyword+"* to report a complaint yourself.")
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "📋 *Your pending complaints (%d)*\n", len(open))
	buttons := make([]whatsapp.Button, 0, len(open))
	for _, g := range open {
		fmt.Fprintf(&b, "\n• %s – %s, ward %d", mccRef(&g), g.ComplaintType, g.WardNo)
		if g.Status == models.MCCStatusReopened {
			b.WriteString(" (reopened)")
		}
		buttons = append(buttons, whatsapp.Button{ID: mccBtnOpen + mccIDString(&g), Title: mccRef(&g)})
	}
	b.WriteString("\n\nSelect a complaint to see its details and take action.")
	if err := a.sendMCCMessage(OutgoingMessageRequest{
		Account: account, Contact: contact, Type: models.MessageTypeInteractive,
		InteractiveType: "list", BodyText: truncateRunes(b.String(), 1024), Buttons: buttons,
	}); err != nil {
		a.Log.Error("Failed to send MCC inbox", "error", err, "contact", contact.PhoneNumber)
	}
}

// handleMCCActionButton dispatches the workflow buttons. It returns false
// for button IDs that are not part of the workflow.
func (a *App) handleMCCActionButton(account *models.WhatsAppAccount, contact *models.Contact, buttonID string) bool {
	var prefix string
	for _, p := range []string{mccBtnOpen, mccBtnProgress, mccBtnClose, mccBtnOK, mccBtnReopen} {
		if strings.HasPrefix(buttonID, p) {
			prefix = p
			break
		}
	}
	if prefix == "" {
		return false
	}
	id, err := strconv.ParseUint(strings.TrimPrefix(buttonID, prefix), 10, 64)
	if err != nil {
		return false
	}
	var g models.MCCGrievance
	if err := a.DB.Where("id = ? AND organization_id = ?", id, account.OrganizationID).First(&g).Error; err != nil {
		a.Log.Error("MCC grievance lookup failed", "error", err, "id", id, "org", account.OrganizationID, "button", buttonID)
		_ = a.sendAndSaveTextMessage(account, contact, "Sorry, that complaint could not be found.")
		return true
	}

	switch prefix {
	case mccBtnOpen, mccBtnProgress, mccBtnClose:
		if g.SupervisorContactID == nil || *g.SupervisorContactID != contact.ID {
			_ = a.sendAndSaveTextMessage(account, contact, "This complaint is not assigned to you.")
			return true
		}
		switch prefix {
		case mccBtnOpen:
			if err := a.sendMCCComplaintCard(account, contact, &g, "📄 *Complaint details*"); err != nil {
				a.Log.Error("Failed to send MCC card", "error", err, "grievance", g.ID)
			}
		case mccBtnProgress:
			a.mccMarkInProgress(account, contact, &g)
		case mccBtnClose:
			a.mccStartClose(account, contact, &g)
		}
	case mccBtnOK, mccBtnReopen:
		if g.ContactID != contact.ID {
			_ = a.sendAndSaveTextMessage(account, contact, "This complaint was not filed from your number.")
			return true
		}
		if prefix == mccBtnOK {
			a.mccCitizenSatisfied(account, contact, &g)
		} else {
			a.mccStartReopen(account, contact, &g)
		}
	}
	return true
}

func mccIsFinished(g *models.MCCGrievance) bool {
	return g.Status == models.MCCStatusResolved || g.Status == models.MCCStatusClosed
}

func (a *App) mccMarkInProgress(account *models.WhatsAppAccount, contact *models.Contact, g *models.MCCGrievance) {
	if mccIsFinished(g) {
		_ = a.sendAndSaveTextMessage(account, contact, fmt.Sprintf("%s is already %s.", mccRef(g), g.Status))
		return
	}
	if g.Status == models.MCCStatusInProgress {
		_ = a.sendAndSaveTextMessage(account, contact, fmt.Sprintf("%s is already marked in progress.", mccRef(g)))
		return
	}
	now := time.Now()
	g.Status = models.MCCStatusInProgress
	g.InProgressAt = &now
	g.History = append(g.History, mccHistoryEntry(models.MCCStatusInProgress, "supervisor", ""))
	if err := a.DB.Model(g).Select("status", "in_progress_at", "history").Updates(g).Error; err != nil {
		a.Log.Error("Failed to mark MCC grievance in progress", "error", err, "grievance", g.ID)
		return
	}
	_ = a.sendMCCMessage(OutgoingMessageRequest{
		Account: account, Contact: contact, Type: models.MessageTypeInteractive, InteractiveType: "button",
		BodyText: fmt.Sprintf("🔧 %s marked *in progress*. Tap below once the work is done.", mccRef(g)),
		Buttons:  mccSupervisorButtons(g),
	})

	if citizen := a.mccCitizenContact(g); citizen != nil {
		_ = a.sendAndSaveTextMessage(account, citizen, fmt.Sprintf(
			"Dear Citizen, work on your complaint *%s* (%s) has started. We will notify you once it is resolved.", mccRef(g), g.ComplaintType))
	}
}

// mccStartClose asks the supervisor for the action-taken details.
func (a *App) mccStartClose(account *models.WhatsAppAccount, contact *models.Contact, g *models.MCCGrievance) {
	if mccIsFinished(g) {
		_ = a.sendAndSaveTextMessage(account, contact, fmt.Sprintf("%s is already %s.", mccRef(g), g.Status))
		return
	}
	a.saveMCCSession(&models.MCCSession{
		ContactID:      contact.ID,
		OrganizationID: account.OrganizationID,
		Category:       g.Category,
		Stage:          models.MCCStageAwaitingAction,
		GrievanceID:    &g.ID,
	})
	_ = a.sendAndSaveTextMessage(account, contact, fmt.Sprintf(
		"✍️ Closing *%s*.\n\nPlease reply with the *action taken* (what was done, when, by whom). "+
			"You can also send a photo of the completed work with the details as its caption.", mccRef(g)))
}

// receiveMCCActionDetails takes the supervisor's reply to mccStartClose. A
// photo without a caption is kept and the text is asked for again.
func (a *App) receiveMCCActionDetails(account *models.WhatsAppAccount, contact *models.Contact, session *models.MCCSession, extracted ExtractedMessage) {
	if session.GrievanceID == nil {
		a.DB.Delete(session)
		a.sendMCCHome(account, contact)
		return
	}
	var g models.MCCGrievance
	if err := a.DB.Where("id = ? AND organization_id = ?", *session.GrievanceID, account.OrganizationID).First(&g).Error; err != nil {
		a.DB.Delete(session)
		_ = a.sendAndSaveTextMessage(account, contact, "Sorry, that complaint could not be found.")
		return
	}

	photos := session.PendingPhotos
	if extracted.Media != nil && extracted.Media.MediaURL != "" {
		photos = append(photos, extracted.Media.MediaURL)
	}
	details := strings.TrimSpace(extracted.Text)
	if details == "" {
		session.PendingPhotos = photos
		a.saveMCCSession(session)
		_ = a.sendAndSaveTextMessage(account, contact,
			fmt.Sprintf("📷 Photo received for %s. Now please describe the action taken in a text message.", mccRef(&g)))
		return
	}

	a.DB.Delete(session)
	a.closeMCCGrievance(account, &g, details, photos, "supervisor")
	_ = a.sendAndSaveTextMessage(account, contact, fmt.Sprintf(
		"✅ *%s* closed. The citizen has been notified of the action taken.\n\nReply anything to see your pending complaints.", mccRef(&g)))
}

// closeMCCGrievance records the resolution and notifies the citizen.
func (a *App) closeMCCGrievance(account *models.WhatsAppAccount, g *models.MCCGrievance, actionTaken string, photos models.JSONBArray, by string) {
	now := time.Now()
	g.Status = models.MCCStatusResolved
	g.ActionTaken = actionTaken
	g.ActionPhotoURLs = photos
	if g.ActionPhotoURLs == nil {
		g.ActionPhotoURLs = models.JSONBArray{}
	}
	g.ResolvedAt = &now
	g.History = append(g.History, mccHistoryEntry(models.MCCStatusResolved, by, actionTaken))
	if err := a.DB.Model(g).Select("status", "action_taken", "action_photo_urls", "resolved_at", "history").Updates(g).Error; err != nil {
		a.Log.Error("Failed to close MCC grievance", "error", err, "grievance", g.ID)
		return
	}
	a.Log.Info("MCC grievance resolved", "reference_no", mccRef(g), "by", by)
	a.notifyMCCCitizenClosure(account, g)
}

// ---------------------------------------------------------------------------
// Citizen side
// ---------------------------------------------------------------------------

func (a *App) mccCitizenContact(g *models.MCCGrievance) *models.Contact {
	var c models.Contact
	if err := a.DB.Where("id = ?", g.ContactID).First(&c).Error; err != nil {
		a.Log.Error("MCC citizen contact missing", "error", err, "grievance", g.ID)
		return nil
	}
	return &c
}

// notifyMCCCitizenClosure tells the citizen what was done and offers to
// reopen, via template when they are outside the service window.
func (a *App) notifyMCCCitizenClosure(account *models.WhatsAppAccount, g *models.MCCGrievance) {
	citizen := a.mccCitizenContact(g)
	if citizen == nil {
		return
	}
	id := mccIDString(g)
	var err error
	if !mccInServiceWindow(citizen) && a.Config.MCC.CitizenClosureTemplate != "" {
		var tpl *models.Template
		tpl, err = a.mccTemplate(account, a.Config.MCC.CitizenClosureTemplate)
		if err == nil {
			err = a.sendMCCMessage(OutgoingMessageRequest{
				Account: account, Contact: citizen, Type: models.MessageTypeTemplate, Template: tpl,
				BodyParams:         map[string]string{"1": mccRef(g), "2": g.ComplaintType, "3": mccOneLine(g.ActionTaken, 300)},
				QuickReplyPayloads: mccQuickReplyPayloads(tpl, mccBtnOK+id, mccBtnReopen+id),
			})
		}
	} else {
		for i, raw := range g.ActionPhotoURLs {
			path, _ := raw.(string)
			data, mime, rerr := a.readMCCMedia(path)
			if rerr != nil {
				continue
			}
			_ = a.sendMCCMessage(OutgoingMessageRequest{
				Account: account, Contact: citizen, Type: models.MessageTypeImage,
				MediaData: data, MediaMimeType: mime, MediaURL: path,
				Caption: fmt.Sprintf("%s – work completed, photo %d/%d", mccRef(g), i+1, len(g.ActionPhotoURLs)),
			})
		}
		err = a.sendMCCMessage(OutgoingMessageRequest{
			Account: account, Contact: citizen, Type: models.MessageTypeInteractive, InteractiveType: "button",
			BodyText: mccClosureText(g),
			Buttons: []whatsapp.Button{
				{ID: mccBtnOK + id, Title: "👍 Satisfied"},
				{ID: mccBtnReopen + id, Title: "🔁 Reopen"},
			},
		})
	}
	if err != nil {
		a.Log.Error("Failed to notify MCC citizen of closure", "error", err, "grievance", g.ID)
	}
}

func mccClosureText(g *models.MCCGrievance) string {
	return truncateRunes(fmt.Sprintf("Dear Citizen, your complaint *%s* (%s) has been resolved.\n\n"+
		"*Action taken:* %s\n\n"+
		"Are you satisfied with the resolution? If the issue persists, tap *Reopen* and we will send it back to the supervisor.",
		mccRef(g), g.ComplaintType, mccOneLine(g.ActionTaken, 500)), 1024)
}

func (a *App) mccCitizenSatisfied(account *models.WhatsAppAccount, contact *models.Contact, g *models.MCCGrievance) {
	switch g.Status {
	case models.MCCStatusClosed:
		_ = a.sendAndSaveTextMessage(account, contact, fmt.Sprintf("Complaint %s is already closed. Thank you!", mccRef(g)))
		return
	case models.MCCStatusResolved:
	default:
		_ = a.sendAndSaveTextMessage(account, contact, fmt.Sprintf("Complaint %s is still being worked on (%s).", mccRef(g), g.Status))
		return
	}
	now := time.Now()
	g.Status = models.MCCStatusClosed
	g.ClosedAt = &now
	g.History = append(g.History, mccHistoryEntry(models.MCCStatusClosed, "citizen", "satisfied"))
	if err := a.DB.Model(g).Select("status", "closed_at", "history").Updates(g).Error; err != nil {
		a.Log.Error("Failed to close MCC grievance", "error", err, "grievance", g.ID)
		return
	}
	a.DB.Where("contact_id = ?", contact.ID).Delete(&models.MCCSession{})
	_ = a.sendAndSaveTextMessage(account, contact, fmt.Sprintf(
		"🙏 Thank you! Complaint *%s* is now closed. Mangaluru Mahanagara Palike appreciates your feedback.", mccRef(g)))
}

// mccStartReopen asks the citizen why they are not satisfied.
func (a *App) mccStartReopen(account *models.WhatsAppAccount, contact *models.Contact, g *models.MCCGrievance) {
	switch g.Status {
	case models.MCCStatusResolved, models.MCCStatusClosed:
	default:
		_ = a.sendAndSaveTextMessage(account, contact, fmt.Sprintf("Complaint %s is still open (%s) and cannot be reopened yet.", mccRef(g), g.Status))
		return
	}
	if g.SupervisorContactID == nil {
		_ = a.sendAndSaveTextMessage(account, contact, "Sorry, this complaint cannot be reopened. Please file a new complaint.")
		return
	}
	a.saveMCCSession(&models.MCCSession{
		ContactID:      contact.ID,
		OrganizationID: account.OrganizationID,
		Category:       g.Category,
		Stage:          models.MCCStageAwaitingReopenReason,
		GrievanceID:    &g.ID,
	})
	_ = a.sendAndSaveTextMessage(account, contact, fmt.Sprintf(
		"We're sorry the issue isn't resolved. Please tell us briefly what is still wrong with *%s* and we will reopen it.", mccRef(g)))
}

// receiveMCCReopenReason reopens the complaint with the citizen's reason
// (a photo caption counts) and sends it back to the supervisor.
func (a *App) receiveMCCReopenReason(account *models.WhatsAppAccount, contact *models.Contact, session *models.MCCSession, extracted ExtractedMessage) {
	a.DB.Delete(session)
	if session.GrievanceID == nil {
		a.sendMCCHome(account, contact)
		return
	}
	var g models.MCCGrievance
	if err := a.DB.Where("id = ? AND organization_id = ? AND contact_id = ?", *session.GrievanceID, account.OrganizationID, contact.ID).First(&g).Error; err != nil {
		_ = a.sendAndSaveTextMessage(account, contact, "Sorry, that complaint could not be found.")
		return
	}
	reason := strings.TrimSpace(extracted.Text)
	if reason == "" {
		reason = "(no reason given)"
	}
	a.reopenMCCGrievance(account, &g, reason, "citizen")
	_ = a.sendAndSaveTextMessage(account, contact, fmt.Sprintf(
		"🔁 Complaint *%s* has been reopened and sent back to the supervisor. We will notify you once it is resolved again.", mccRef(&g)))
}

// reopenMCCGrievance puts a resolved grievance back with its supervisor.
func (a *App) reopenMCCGrievance(account *models.WhatsAppAccount, g *models.MCCGrievance, reason, by string) {
	g.Status = models.MCCStatusReopened
	g.ReopenCount++
	g.ReopenReason = reason
	g.ResolvedAt, g.ClosedAt = nil, nil
	g.History = append(g.History, mccHistoryEntry(models.MCCStatusReopened, by, reason))
	err := a.DB.Model(g).Select("status", "reopen_count", "reopen_reason", "resolved_at", "closed_at", "history").Updates(g).Error
	if err != nil {
		a.Log.Error("Failed to reopen MCC grievance", "error", err, "grievance", g.ID)
		return
	}
	a.Log.Info("MCC grievance reopened", "reference_no", mccRef(g), "count", g.ReopenCount)

	if g.SupervisorContactID == nil {
		return
	}
	var sup models.Contact
	if err := a.DB.Where("id = ?", *g.SupervisorContactID).First(&sup).Error; err != nil {
		a.Log.Error("MCC supervisor contact missing", "error", err, "grievance", g.ID)
		return
	}
	a.notifyMCCSupervisor(account, g, &sup, "🔁 *Complaint reopened by the citizen*")
}

// mccGrievanceByRef finds a grievance by its reference number or ID.
func (a *App) mccGrievanceByRef(orgID uuid.UUID, ref string) (*models.MCCGrievance, error) {
	var g models.MCCGrievance
	q := a.DB.Where("organization_id = ?", orgID)
	if id, err := strconv.ParseUint(ref, 10, 64); err == nil {
		q = q.Where("id = ?", id)
	} else {
		q = q.Where("reference_no = ?", strings.ToUpper(strings.TrimSpace(ref)))
	}
	if err := q.First(&g).Error; err != nil {
		return nil, err
	}
	return &g, nil
}
