package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/contactutil"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"gorm.io/gorm"
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
	mccBtnInbox    = "mcc_inbox:"    // supervisor: list complaints (pending|open|breach)
)

// Supervisor menu filters.
const (
	mccInboxPending = "pending" // assigned / reopened, work not started
	mccInboxOpen    = "open"    // in progress
	mccInboxBreach  = "breach"  // pending or open and due within mccBreachWindow
	mccBreachWindow = 48 * time.Hour
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

// notifyMCCSupervisor sends the complaint to its supervisor and records
// the outcome on the grievance. With a supervisor template configured the
// template is the whole notification: the first complaint photo is its
// image header and its quick-reply button opens the details. Without one
// the complaint goes out free-form (only possible inside the 24h window).
func (a *App) notifyMCCSupervisor(account *models.WhatsAppAccount, g *models.MCCGrievance, supContact *models.Contact, headline string) {
	var err error
	if a.Config.MCC.SupervisorTemplate != "" {
		err = a.sendMCCSupervisorTemplate(account, supContact, g)
	} else {
		err = a.sendMCCComplaintCard(account, supContact, g, headline, true)
	}

	if err == nil {
		a.sendMCCLocationPin(account, supContact, g)
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

// sendMCCSupervisorTemplate sends the approved template with the first
// complaint photo as its image header (when the template has one); its
// first QUICK_REPLY button opens the complaint card.
func (a *App) sendMCCSupervisorTemplate(account *models.WhatsAppAccount, contact *models.Contact, g *models.MCCGrievance) error {
	tpl, err := a.mccTemplate(account, a.Config.MCC.SupervisorTemplate)
	if err != nil {
		return err
	}
	req := OutgoingMessageRequest{
		Account:  account,
		Contact:  contact,
		Type:     models.MessageTypeTemplate,
		Template: tpl,
		BodyParams: map[string]string{
			"1": mccRef(g), "2": g.ComplaintType, "3": mccWardLabel(g),
			"4": mccOneLine(g.Address, 200), "5": mccOneLine(g.Description, 300), "6": g.SLA,
		},
		QuickReplyPayloads: mccQuickReplyPayloads(tpl, mccBtnOpen+mccIDString(g)),
	}
	if strings.EqualFold(tpl.HeaderType, "IMAGE") {
		mediaID, filename, err := a.uploadMCCPhoto(account, g.PhotoURLs)
		if err != nil {
			return fmt.Errorf("template header photo: %w", err)
		}
		req.HeaderMediaID, req.HeaderMediaFilename = mediaID, filename
	}
	return a.sendMCCMessage(req)
}

// uploadMCCPhoto uploads the first readable photo to Meta and returns its
// media ID.
func (a *App) uploadMCCPhoto(account *models.WhatsAppAccount, photos models.JSONBArray) (string, string, error) {
	var lastErr error = errors.New("no photo on the complaint")
	for _, raw := range photos {
		path, _ := raw.(string)
		data, mime, err := a.readMCCMedia(path)
		if err != nil {
			lastErr = err
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		id, err := a.WhatsApp.UploadMedia(ctx, a.toWhatsAppAccount(account), data, mime, filepath.Base(path))
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		return id, filepath.Base(path), nil
	}
	return "", "", lastErr
}

// mccTemplate loads an approved template by name for the account.
// A template named in the config but not yet synced from Meta is fetched on
// first use, at most once per mccTemplateSyncEvery per account.
func (a *App) mccTemplate(account *models.WhatsAppAccount, name string) (*models.Template, error) {
	var tpl models.Template
	lookup := func() error {
		return a.DB.Where("organization_id = ? AND whats_app_account = ? AND name = ?", account.OrganizationID, account.Name, name).
			Order("status = 'APPROVED' DESC").First(&tpl).Error
	}
	err := lookup()
	if errors.Is(err, gorm.ErrRecordNotFound) && a.mccSyncTemplatesOnce(account) {
		err = lookup()
	}
	if err != nil {
		return nil, fmt.Errorf("template %q: %w", name, err)
	}
	return &tpl, nil
}

const mccTemplateSyncEvery = 10 * time.Minute

var mccTemplateSyncedAt sync.Map // account ID -> time.Time

// mccSyncTemplatesOnce syncs the account's templates from Meta unless that
// was done recently. It returns true when a sync ran.
func (a *App) mccSyncTemplatesOnce(account *models.WhatsAppAccount) bool {
	if last, ok := mccTemplateSyncedAt.Load(account.ID); ok && time.Since(last.(time.Time)) < mccTemplateSyncEvery {
		return false
	}
	mccTemplateSyncedAt.Store(account.ID, time.Now())
	n, err := a.syncTemplatesForAccount(account)
	if err != nil {
		a.Log.Error("MCC template sync failed", "error", err, "account", account.Name)
		return false
	}
	a.Log.Info("MCC synced templates from Meta", "account", account.Name, "count", n)
	return true
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

// sendMCCComplaintCard sends the complaint details with the supervisor's
// action buttons, preceded by the photos when withPhotos is set (the
// template path already delivered the photo as its header).
func (a *App) sendMCCComplaintCard(account *models.WhatsAppAccount, contact *models.Contact, g *models.MCCGrievance, headline string, withPhotos bool) error {
	ref := mccRef(g)
	var photos models.JSONBArray
	if withPhotos {
		photos = g.PhotoURLs
	}
	for i, raw := range photos {
		path, _ := raw.(string)
		data, mime, err := a.readMCCMedia(path)
		if err != nil {
			a.Log.Warn("MCC photo unreadable", "error", err, "path", path)
			continue
		}
		// A photo that Meta rejects must not stop the complaint itself.
		if err := a.sendMCCMessage(OutgoingMessageRequest{
			Account: account, Contact: contact, Type: models.MessageTypeImage,
			MediaData: data, MediaMimeType: mime, MediaURL: path, MediaFilename: filepath.Base(path),
			Caption: fmt.Sprintf("%s – photo %d/%d", ref, i+1, len(photos)),
		}); err != nil {
			a.Log.Error("Failed to send MCC photo", "error", err, "grievance", g.ID, "path", path)
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

// sendMCCLocationPin sends the citizen's shared location as a WhatsApp map
// pin (config supervisor_location_pin) so the supervisor can navigate to it
// in one tap. Nothing is sent when no location was shared; failures are
// logged, the complaint message itself already went out.
func (a *App) sendMCCLocationPin(account *models.WhatsAppAccount, contact *models.Contact, g *models.MCCGrievance) {
	if !a.Config.MCC.SupervisorLocationPin || g.Latitude == nil || g.Longitude == nil {
		return
	}
	err := a.sendMCCMessage(OutgoingMessageRequest{
		Account: account, Contact: contact, Type: models.MessageTypeLocation,
		Latitude: *g.Latitude, Longitude: *g.Longitude,
		LocationName:    mccRef(g) + " – " + mccOneLine(g.ComplaintType, 60),
		LocationAddress: mccOneLine(g.Address, 200),
	})
	if err != nil {
		a.Log.Error("Failed to send MCC location pin", "error", err, "grievance", g.ID, "phone", contact.PhoneNumber)
	}
}

func mccSupervisorButtons(g *models.MCCGrievance) []whatsapp.Button {
	id := mccIDString(g)
	switch g.Status {
	case models.MCCStatusAssigned, models.MCCStatusReopened:
		return []whatsapp.Button{
			{ID: mccBtnProgress + id, Title: "Mark In progress"},
			{ID: mccBtnClose + id, Title: "Mark Close complaint"},
		}
	case models.MCCStatusInProgress:
		return []whatsapp.Button{{ID: mccBtnClose + id, Title: "Mark Close complaint"}}
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
	a.sendMCCSupervisorMenu(account, contact)
}

// mccInboxQuery scopes the supervisor's complaints to a menu filter.
func (a *App) mccInboxQuery(account *models.WhatsAppAccount, contact *models.Contact, filter string) *gorm.DB {
	q := a.DB.Model(&models.MCCGrievance{}).Where("organization_id = ? AND supervisor_contact_id = ?", account.OrganizationID, contact.ID)
	switch filter {
	case mccInboxPending:
		return q.Where("status IN (?)", []string{models.MCCStatusAssigned, models.MCCStatusReopened})
	case mccInboxOpen:
		return q.Where("status = ?", models.MCCStatusInProgress)
	default: // breach
		return q.Where("status IN (?) AND due_at IS NOT NULL AND due_at <= ?",
			[]string{models.MCCStatusAssigned, models.MCCStatusReopened, models.MCCStatusInProgress}, time.Now().Add(mccBreachWindow))
	}
}

// sendMCCSupervisorMenu is the supervisor's home: counts plus a list with
// Pending / Open / To be breached.
func (a *App) sendMCCSupervisorMenu(account *models.WhatsAppAccount, contact *models.Contact) {
	var pending, open, breach int64
	a.mccInboxQuery(account, contact, mccInboxPending).Count(&pending)
	a.mccInboxQuery(account, contact, mccInboxOpen).Count(&open)
	a.mccInboxQuery(account, contact, mccInboxBreach).Count(&breach)

	body := fmt.Sprintf("👷 *Supervisor dashboard*\n\nPending: *%d*  ·  Open: *%d*  ·  To be breached: *%d*\n\n"+
		"Choose a list below.", pending, open, breach)
	if pending+open == 0 {
		body = "👋 You have no complaints assigned right now.\n\nReply *" + mccNewComplaintKeyword + "* to report a complaint yourself."
		_ = a.sendAndSaveTextMessage(account, contact, body)
		return
	}
	err := a.sendMCCMessage(OutgoingMessageRequest{
		Account: account, Contact: contact, Type: models.MessageTypeInteractive, InteractiveType: "list",
		BodyText: body,
		Buttons: []whatsapp.Button{
			{ID: mccBtnInbox + mccInboxPending, Title: fmt.Sprintf("Pending (%d)", pending), Description: "Assigned to you, work not started yet"},
			{ID: mccBtnInbox + mccInboxOpen, Title: fmt.Sprintf("Open (%d)", open), Description: "Marked in progress by you"},
			{ID: mccBtnInbox + mccInboxBreach, Title: fmt.Sprintf("To be breached (%d)", breach), Description: "Pending or open, SLA due within 2 days or overdue"},
		},
	})
	if err != nil {
		a.Log.Error("Failed to send MCC supervisor menu", "error", err, "contact", contact.PhoneNumber)
	}
}

// sendMCCSupervisorList sends the complaints behind one menu entry.
func (a *App) sendMCCSupervisorList(account *models.WhatsAppAccount, contact *models.Contact, filter string) {
	var items []models.MCCGrievance
	order := "created_at ASC"
	if filter == mccInboxBreach {
		order = "due_at ASC"
	}
	a.mccInboxQuery(account, contact, filter).Order(order).Limit(mccInboxMax).Find(&items)

	titles := map[string]string{mccInboxPending: "Pending complaints", mccInboxOpen: "Open complaints", mccInboxBreach: "Complaints to be breached"}
	title := titles[filter]
	if title == "" {
		title = titles[mccInboxBreach]
	}
	if len(items) == 0 {
		_ = a.sendAndSaveTextMessage(account, contact, fmt.Sprintf("✅ No %s right now.\n\nReply anything to go back to the dashboard.", strings.ToLower(title)))
		return
	}

	now := time.Now()
	var b strings.Builder
	fmt.Fprintf(&b, "📋 *%s (%d)*\n", title, len(items))
	buttons := make([]whatsapp.Button, 0, len(items))
	for _, g := range items {
		fmt.Fprintf(&b, "\n• %s – %s, ward %d", mccRef(&g), g.ComplaintType, g.WardNo)
		if g.Status == models.MCCStatusReopened {
			b.WriteString(" (reopened)")
		}
		desc := g.ComplaintType
		if g.DueAt != nil {
			due := "due " + g.DueAt.In(mccIST).Format("02 Jan 15:04")
			if g.DueAt.Before(now) {
				due = "OVERDUE since " + g.DueAt.In(mccIST).Format("02 Jan 15:04")
			}
			fmt.Fprintf(&b, " – %s", due)
			desc = due + " · " + desc
		}
		buttons = append(buttons, whatsapp.Button{ID: mccBtnOpen + mccIDString(&g), Title: mccRef(&g), Description: desc})
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
	if filter, ok := strings.CutPrefix(buttonID, mccBtnInbox); ok {
		if !a.isMCCSupervisor(account.OrganizationID, contact) {
			return false
		}
		a.sendMCCSupervisorList(account, contact, filter)
		return true
	}
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
			if err := a.sendMCCComplaintCard(account, contact, &g, "📄 *Complaint details*", a.Config.MCC.SupervisorTemplate == ""); err != nil {
				a.Log.Error("Failed to send MCC card", "error", err, "grievance", g.ID)
			} else {
				a.sendMCCLocationPin(account, contact, &g)
			}
		case mccBtnProgress:
			if g.Status == models.MCCStatusInProgress {
				_ = a.sendAndSaveTextMessage(account, contact, fmt.Sprintf("%s is already marked in progress.", mccRef(&g)))
			} else if !mccIsFinished(&g) {
				a.requestMCCSupervisorLocation(account, contact, &g, models.MCCStageAwaitingProgressLocation)
			} else {
				_ = a.sendAndSaveTextMessage(account, contact, fmt.Sprintf("%s is already %s.", mccRef(&g), g.Status))
			}
		case mccBtnClose:
			if mccIsFinished(&g) {
				_ = a.sendAndSaveTextMessage(account, contact, fmt.Sprintf("%s is already %s.", mccRef(&g), g.Status))
			} else {
				a.requestMCCSupervisorLocation(account, contact, &g, models.MCCStageAwaitingCloseLocation)
			}
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

func (a *App) mccMarkInProgress(account *models.WhatsAppAccount, contact *models.Contact, g *models.MCCGrievance, at *mccSupervisorLocation) {
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
	note := ""
	if at != nil {
		g.InProgressLatitude, g.InProgressLongitude, g.InProgressDistanceM = &at.Latitude, &at.Longitude, at.DistanceM
		note = at.note()
	}
	g.History = append(g.History, mccHistoryEntry(models.MCCStatusInProgress, "supervisor", note))
	if err := a.DB.Model(g).Select("status", "in_progress_at", "in_progress_latitude", "in_progress_longitude", "in_progress_distance_m", "history").Updates(g).Error; err != nil {
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

// ---------------------------------------------------------------------------
// Supervisor location: before marking a complaint in progress or closing
// it, the supervisor shares where they are via WhatsApp's location request.
// WhatsApp lets the user move the pin, so the position is recorded with its
// distance from the complaint site and optionally geofenced
// (config supervisor_radius_m).
// ---------------------------------------------------------------------------

type mccSupervisorLocation struct {
	Latitude  float64
	Longitude float64
	DistanceM *float64 // nil when the complaint has no coordinates
}

func (l *mccSupervisorLocation) note() string {
	if l.DistanceM == nil {
		return fmt.Sprintf("at %.5f,%.5f", l.Latitude, l.Longitude)
	}
	return fmt.Sprintf("at %.5f,%.5f (%s from site)", l.Latitude, l.Longitude, mccDistanceLabel(*l.DistanceM))
}

func mccIsSupervisorLocationStage(stage string) bool {
	return stage == models.MCCStageAwaitingProgressLocation || stage == models.MCCStageAwaitingCloseLocation
}

const mccCancelKeyword = "cancel"

// requestMCCSupervisorLocation asks the supervisor to share their location
// and remembers which action it is for. If the request can't be sent the
// action proceeds without a location.
func (a *App) requestMCCSupervisorLocation(account *models.WhatsAppAccount, contact *models.Contact, g *models.MCCGrievance, stage string) {
	a.saveMCCSession(&models.MCCSession{
		ContactID:      contact.ID,
		OrganizationID: account.OrganizationID,
		Category:       g.Category,
		Stage:          stage,
		GrievanceID:    &g.ID,
	})
	what := "mark it *in progress*"
	if stage == models.MCCStageAwaitingCloseLocation {
		what = "*close* it"
	}
	err := a.sendMCCMessage(OutgoingMessageRequest{
		Account: account, Contact: contact, Type: models.MessageTypeInteractive, InteractiveType: "location_request",
		BodyText: fmt.Sprintf("📍 To %s, please share your *current location* from the complaint site for %s.\n\n"+
			"Tap *Send location* below (choose your current location, do not move the pin). Type *cancel* to go back.", what, mccRef(g)),
	})
	if err != nil {
		a.Log.Error("Failed to send MCC supervisor location request", "error", err, "grievance", g.ID, "contact", contact.PhoneNumber)
		a.DB.Where("contact_id = ?", contact.ID).Delete(&models.MCCSession{})
		a.continueMCCSupervisorAction(account, contact, g, stage, nil)
	}
}

// receiveMCCSupervisorLocation checks the shared location against the
// complaint site and carries on with the pending action.
func (a *App) receiveMCCSupervisorLocation(account *models.WhatsAppAccount, contact *models.Contact, session *models.MCCSession, raw string) {
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
	var in struct {
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
	}
	if err := json.Unmarshal([]byte(raw), &in); err != nil || (in.Latitude == 0 && in.Longitude == 0) {
		_ = a.sendAndSaveTextMessage(account, contact, "That location could not be read. Please tap *Send location* and try again.")
		return
	}
	at := &mccSupervisorLocation{Latitude: in.Latitude, Longitude: in.Longitude}
	if g.Latitude != nil && g.Longitude != nil {
		d := mccDistanceM(*g.Latitude, *g.Longitude, in.Latitude, in.Longitude)
		at.DistanceM = &d
		if r := a.Config.MCC.SupervisorRadiusM; r > 0 && d > float64(r) {
			_ = a.sendAndSaveTextMessage(account, contact, fmt.Sprintf(
				"⚠️ You appear to be *%s* away from the site of %s. Please go to the site and share your current location again, or type *cancel*.",
				mccDistanceLabel(d), mccRef(&g)))
			return
		}
	}
	a.DB.Delete(session)
	a.continueMCCSupervisorAction(account, contact, &g, session.Stage, at)
}

// remindMCCSupervisorLocation answers text/photos sent while a location is
// awaited; "cancel" drops the pending action.
func (a *App) remindMCCSupervisorLocation(account *models.WhatsAppAccount, contact *models.Contact, session *models.MCCSession, extracted ExtractedMessage) {
	if strings.EqualFold(strings.TrimSpace(extracted.Text), mccCancelKeyword) {
		a.DB.Delete(session)
		_ = a.sendAndSaveTextMessage(account, contact, "Cancelled. Reply anything to see your pending complaints.")
		return
	}
	ref := ""
	if session.GrievanceID != nil {
		ref = fmt.Sprintf(" for complaint #%d", *session.GrievanceID)
	}
	_ = a.sendMCCMessage(OutgoingMessageRequest{
		Account: account, Contact: contact, Type: models.MessageTypeInteractive, InteractiveType: "location_request",
		BodyText: fmt.Sprintf("📍 Please tap *Send location* to share your current location%s, or type *cancel*.", ref),
	})
}

func (a *App) continueMCCSupervisorAction(account *models.WhatsAppAccount, contact *models.Contact, g *models.MCCGrievance, stage string, at *mccSupervisorLocation) {
	switch stage {
	case models.MCCStageAwaitingProgressLocation:
		a.mccMarkInProgress(account, contact, g, at)
	case models.MCCStageAwaitingCloseLocation:
		address := ""
		if at != nil {
			g.ActionLatitude, g.ActionLongitude, g.ActionDistanceM = &at.Latitude, &at.Longitude, at.DistanceM
			if err := a.DB.Model(g).Select("action_latitude", "action_longitude", "action_distance_m").Updates(g).Error; err != nil {
				a.Log.Error("Failed to save MCC action location", "error", err, "grievance", g.ID)
			}
			address = a.mccAddressFor(at.Latitude, at.Longitude)
		}
		a.mccStartClose(account, contact, g, address)
	}
}

// mccAddressFor reverse geocodes a point; "" when the geocoder fails.
func (a *App) mccAddressFor(lat, lon float64) string {
	ctx, cancel := context.WithTimeout(context.Background(), mccGeocodeTimeout)
	defer cancel()
	addr, err := a.reverseGeocodeMCC(ctx, lat, lon)
	if err != nil {
		a.Log.Warn("MCC reverse geocoding failed", "error", err, "lat", lat, "lon", lon)
		return ""
	}
	return addr
}

// mccDistanceM is the haversine distance in metres.
func mccDistanceM(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371000.0
	toRad := func(d float64) float64 { return d * math.Pi / 180 }
	dLat, dLon := toRad(lat2-lat1), toRad(lon2-lon1)
	h := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(toRad(lat1))*math.Cos(toRad(lat2))*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * r * math.Asin(math.Sqrt(h))
}

func mccDistanceLabel(m float64) string {
	if m < 1000 {
		return fmt.Sprintf("%.0f m", m)
	}
	return fmt.Sprintf("%.1f km", m/1000)
}

// mccStartClose asks the supervisor for the action-taken details.
func (a *App) mccStartClose(account *models.WhatsAppAccount, contact *models.Contact, g *models.MCCGrievance, address string) {
	if mccIsFinished(g) {
		_ = a.sendAndSaveTextMessage(account, contact, fmt.Sprintf("%s is already %s.", mccRef(g), g.Status))
		return
	}
	if a.sendMCCCloseForm(account, contact, g, address) {
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

// Closure Flow form (config [mcc] closure_flow_id). Its flow token is
// "mcc:close:<grievance id>:<uuid>" so the nfm_reply is routed here.
const (
	mccCloseFlowCategory = "close"
	mccCloseFlowCTA      = "Fill action report"
	mccCloseDateLayout   = "2006-01-02"
)

var mccIST = time.FixedZone("IST", 5*3600+1800)

// sendMCCCloseForm sends the closure Flow pre-filled with the complaint
// and the address of the supervisor's shared location, and remembers the
// token. The flow is the bot's own (mccCloseForm) unless [mcc]
// closure_flow_id names one with the same data keys. It returns false when
// the form couldn't be sent so the caller can fall back to free text.
func (a *App) sendMCCCloseForm(account *models.WhatsAppAccount, contact *models.Contact, g *models.MCCGrievance, address string) bool {
	flowID, mode := a.Config.MCC.ClosureFlowID, ""
	if flowID == "" {
		flow, err := a.ensureMCCFlow(account, mccCloseForm{})
		if err != nil {
			a.Log.Error("MCC closure flow unavailable", "error", err)
			return false
		}
		flowID = flow.MetaFlowID
		if flow.Status != "PUBLISHED" {
			mode = "draft"
		}
	}
	token := fmt.Sprintf("%s%s:%d:%s", mccFlowTokenPrefix, mccCloseFlowCategory, g.ID, uuid.NewString())
	a.saveMCCSession(&models.MCCSession{
		ContactID:      contact.ID,
		OrganizationID: account.OrganizationID,
		Category:       g.Category,
		Stage:          models.MCCStageCloseFormSent,
		FlowToken:      token,
		GrievanceID:    &g.ID,
	})
	screen := a.Config.MCC.ClosureFlowScreen
	if screen == "" {
		screen = mccCloseScreenID
	}
	err := a.sendMCCMessage(OutgoingMessageRequest{
		Account:         account,
		Contact:         contact,
		Type:            models.MessageTypeFlow,
		FlowID:          flowID,
		FlowMode:        mode,
		FlowHeader:      mccCloseFormHeader,
		BodyText:        fmt.Sprintf("Closing *%s*. Tap the button below and fill in the action taken, the date, the location and photos of the completed work.", mccRef(g)),
		FlowCTA:         mccCloseFlowCTA,
		FlowToken:       token,
		FlowFirstScreen: screen,
		FlowData: map[string]any{
			"heading":           "Complaint " + mccRef(g),
			"complaint_summary": mccOneLine(fmt.Sprintf("%s - %s, %s", g.ComplaintType, mccWardLabel(g), g.Address), 120),
			"today":             time.Now().In(mccIST).Format(mccCloseDateLayout),
			"address":           truncateRunes(address, 300),
		},
	})
	if err != nil {
		a.Log.Error("Failed to send MCC closure form", "error", err, "grievance", g.ID, "contact", contact.PhoneNumber)
		return false
	}
	return true
}

// receiveMCCCloseForm handles the closure Flow submission.
func (a *App) receiveMCCCloseForm(account *models.WhatsAppAccount, contact *models.Contact, waMessageID, token string, data map[string]any) {
	var session models.MCCSession
	err := a.DB.Where("contact_id = ? AND flow_token = ?", contact.ID, token).First(&session).Error
	if err != nil || session.GrievanceID == nil {
		a.Log.Warn("MCC closure form reply without session", "flow_token", token, "contact", contact.PhoneNumber, "error", err)
		_ = a.sendAndSaveTextMessage(account, contact, "Sorry, this action report has expired. Please open the complaint again and tap *Close complaint*.")
		return
	}
	var g models.MCCGrievance
	if err := a.DB.Where("id = ? AND organization_id = ?", *session.GrievanceID, account.OrganizationID).First(&g).Error; err != nil {
		a.DB.Delete(&session)
		_ = a.sendAndSaveTextMessage(account, contact, "Sorry, that complaint could not be found.")
		return
	}
	if mccIsFinished(&g) {
		a.DB.Delete(&session)
		_ = a.sendAndSaveTextMessage(account, contact, fmt.Sprintf("%s is already %s.", mccRef(&g), g.Status))
		return
	}

	action := strings.TrimSpace(mccString(data["action_taken"]))
	if action == "" {
		action = "Resolved (no details given)"
	}
	closure := mccClosure{
		Action:   action,
		Location: strings.TrimSpace(mccString(data["work_location"])),
		FixedOn:  mccParseFlowDate(data["fixed_on"]),
		Photos:   a.downloadMCCPhotos(account, data["action_photos"]),
	}
	a.DB.Delete(&session)
	a.closeMCCGrievanceWith(account, &g, closure, "supervisor")
	_ = a.sendAndSaveTextMessage(account, contact, fmt.Sprintf(
		"✅ *%s* closed. The citizen has been notified of the action taken.\n\nReply anything to see your pending complaints.", mccRef(&g)))
}

func mccString(v any) string {
	s, _ := v.(string)
	return s
}

// mccParseFlowDate accepts a DatePicker value: a Unix-millisecond timestamp
// (as a string or JSON number) or a YYYY-MM-DD date.
func mccParseFlowDate(v any) *time.Time {
	var raw string
	switch x := v.(type) {
	case string:
		raw = strings.TrimSpace(x)
	case json.Number:
		raw = x.String()
	case float64:
		raw = strconv.FormatFloat(x, 'f', 0, 64)
	}
	if raw == "" {
		return nil
	}
	if t, err := time.ParseInLocation(mccCloseDateLayout, raw, mccIST); err == nil {
		return &t
	}
	if ms, err := strconv.ParseInt(raw, 10, 64); err == nil {
		t := time.UnixMilli(ms).In(mccIST)
		return &t
	}
	return nil
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

// mccClosure is what the supervisor reported when closing a complaint.
type mccClosure struct {
	Action   string
	Photos   models.JSONBArray
	FixedOn  *time.Time
	Location string
}

// closeMCCGrievance records a free-text resolution and notifies the citizen.
func (a *App) closeMCCGrievance(account *models.WhatsAppAccount, g *models.MCCGrievance, actionTaken string, photos models.JSONBArray, by string) {
	a.closeMCCGrievanceWith(account, g, mccClosure{Action: actionTaken, Photos: photos}, by)
}

func (a *App) closeMCCGrievanceWith(account *models.WhatsAppAccount, g *models.MCCGrievance, c mccClosure, by string) {
	now := time.Now()
	g.Status = models.MCCStatusResolved
	g.ActionTaken = c.Action
	g.ActionPhotoURLs = c.Photos
	if g.ActionPhotoURLs == nil {
		g.ActionPhotoURLs = models.JSONBArray{}
	}
	g.FixedOn = c.FixedOn
	g.WorkLocation = c.Location
	g.ResolvedAt = &now
	note := c.Action
	if c.FixedOn != nil {
		note += " | fixed on " + c.FixedOn.In(mccIST).Format("02 Jan 2006")
	}
	if c.Location != "" {
		note += " | at " + c.Location
	}
	if g.ActionLatitude != nil && g.ActionLongitude != nil {
		note += " | " + (&mccSupervisorLocation{Latitude: *g.ActionLatitude, Longitude: *g.ActionLongitude, DistanceM: g.ActionDistanceM}).note()
	}
	g.History = append(g.History, mccHistoryEntry(models.MCCStatusResolved, by, note))
	if err := a.DB.Model(g).Select("status", "action_taken", "action_photo_urls", "fixed_on", "work_location", "resolved_at", "history").Updates(g).Error; err != nil {
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
	var err error
	if a.Config.MCC.CitizenClosureTemplate != "" {
		err = a.sendMCCClosureTemplate(account, citizen, g)
		if err != nil {
			a.Log.Error("MCC closure template failed, trying free-form", "error", err, "grievance", g.ID)
		}
	}
	if a.Config.MCC.CitizenClosureTemplate == "" || err != nil {
		err = a.sendMCCClosureFreeForm(account, citizen, g)
	}
	if err != nil {
		a.Log.Error("Failed to notify MCC citizen of closure", "error", err, "grievance", g.ID)
	}
}

// sendMCCClosureTemplate sends the approved closure template: image header
// = before/after composite, body params {{1}} complaint ID, {{2}} type,
// {{3}} action taken, {{4}} date fixed; quick replies Satisfied / Reopen.
func (a *App) sendMCCClosureTemplate(account *models.WhatsAppAccount, citizen *models.Contact, g *models.MCCGrievance) error {
	tpl, err := a.mccTemplate(account, a.Config.MCC.CitizenClosureTemplate)
	if err != nil {
		return err
	}
	id := mccIDString(g)
	req := OutgoingMessageRequest{
		Account: account, Contact: citizen, Type: models.MessageTypeTemplate, Template: tpl,
		BodyParams: map[string]string{
			"1": mccRef(g), "2": g.ComplaintType, "3": mccOneLine(g.ActionTaken, 300), "4": mccFixedOnLabel(g),
		},
		QuickReplyPayloads: mccQuickReplyPayloads(tpl, mccBtnOK+id, mccBtnReopen+id),
	}
	if strings.EqualFold(tpl.HeaderType, "IMAGE") {
		path, err := a.ensureMCCComposite(g)
		if err != nil {
			return fmt.Errorf("before/after image: %w", err)
		}
		mediaID, filename, err := a.uploadMCCPhoto(account, models.JSONBArray{path})
		if err != nil {
			return fmt.Errorf("template header image: %w", err)
		}
		req.HeaderMediaID, req.HeaderMediaFilename = mediaID, filename
	}
	return a.sendMCCMessage(req)
}

// ensureMCCComposite renders and stores the before/after image once.
func (a *App) ensureMCCComposite(g *models.MCCGrievance) (string, error) {
	if g.BeforeAfterURL != "" {
		if _, _, err := a.readMCCMedia(g.BeforeAfterURL); err == nil {
			return g.BeforeAfterURL, nil
		}
	}
	data, err := a.renderMCCBeforeAfter(g)
	if err != nil {
		return "", err
	}
	path, err := a.saveMCCComposite(g, data)
	if err != nil {
		return "", err
	}
	g.BeforeAfterURL = path
	if err := a.DB.Model(g).Update("before_after_url", path).Error; err != nil {
		a.Log.Error("Failed to save MCC composite path", "error", err, "grievance", g.ID)
	}
	return path, nil
}

func mccFixedOnLabel(g *models.MCCGrievance) string {
	when := time.Now()
	if g.FixedOn != nil {
		when = *g.FixedOn
	} else if g.ResolvedAt != nil {
		when = *g.ResolvedAt
	}
	return when.In(mccIST).Format("02 Jan 2006")
}

// sendMCCClosureFreeForm is the closure notice inside the citizen's 24h
// window when no template is configured (or it failed): the before/after
// image, then the text with Satisfied / Reopen buttons.
func (a *App) sendMCCClosureFreeForm(account *models.WhatsAppAccount, citizen *models.Contact, g *models.MCCGrievance) error {
	id := mccIDString(g)
	if path, err := a.ensureMCCComposite(g); err == nil {
		if data, mime, rerr := a.readMCCMedia(path); rerr == nil {
			_ = a.sendMCCMessage(OutgoingMessageRequest{
				Account: account, Contact: citizen, Type: models.MessageTypeImage,
				MediaData: data, MediaMimeType: mime, MediaURL: path, MediaFilename: filepath.Base(path),
				Caption: mccRef(g) + " – before and after",
			})
		}
	} else {
		a.Log.Warn("MCC before/after image unavailable", "error", err, "grievance", g.ID)
	}
	return a.sendMCCMessage(OutgoingMessageRequest{
		Account: account, Contact: citizen, Type: models.MessageTypeInteractive, InteractiveType: "button",
		BodyText: mccClosureText(g),
		Buttons: []whatsapp.Button{
			{ID: mccBtnOK + id, Title: "👍 Satisfied"},
			{ID: mccBtnReopen + id, Title: "🔁 Reopen"},
		},
	})
}

func mccClosureText(g *models.MCCGrievance) string {
	var extra strings.Builder
	if g.FixedOn != nil {
		fmt.Fprintf(&extra, "*Fixed on:* %s\n", g.FixedOn.In(mccIST).Format("02 Jan 2006"))
	}
	if g.WorkLocation != "" {
		fmt.Fprintf(&extra, "*Location:* %s\n", mccOneLine(g.WorkLocation, 120))
	}
	return truncateRunes(fmt.Sprintf("Dear Citizen, your complaint *%s* (%s) has been resolved.\n\n"+
		"*Action taken:* %s\n%s\n"+
		"Are you satisfied with the resolution? If the issue persists, tap *Reopen* and we will send it back to the supervisor.",
		mccRef(g), g.ComplaintType, mccOneLine(g.ActionTaken, 500), extra.String()), 1024)
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
