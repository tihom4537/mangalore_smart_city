package handlers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/contactutil"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

const (
	mccTestSupervisorPhone = "919900000099"
	mccTestOtherPhone      = "919900000077"
)

// messagesTo returns the bodies of every message POSTed to phone, oldest first.
func (m *mccMetaMock) messagesTo(phone string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for i, r := range m.requests {
		if strings.HasSuffix(r, "/messages") && strings.Contains(m.bodies[i], `"to":"`+phone+`"`) {
			out = append(out, m.bodies[i])
		}
	}
	return out
}

func (m *mccMetaMock) lastMessageTo(phone string) string {
	msgs := m.messagesTo(phone)
	if len(msgs) == 0 {
		return ""
	}
	return msgs[len(msgs)-1]
}

func createMCCSupervisor(t *testing.T, app *App, orgID uuid.UUID, name, phone string, ward int, category string) *models.MCCSupervisor {
	t.Helper()
	sup := &models.MCCSupervisor{OrganizationID: orgID, Name: name, PhoneNumber: phone, WardNo: ward, Category: category, IsActive: true}
	require.NoError(t, app.DB.Create(sup).Error)
	return sup
}

// fileMCCGrievance submits a garbage complaint for ward 37 with one photo
// and returns the stored row. The test database is not wiped between
// tests and wa_message_id is unique, so the ID is made unique here.
func fileMCCGrievance(t *testing.T, app *App, account *models.WhatsAppAccount, contact *models.Contact, waMessageID string) *models.MCCGrievance {
	t.Helper()
	waMessageID += "-" + uuid.NewString()[:8]
	lat, lon := 12.8698, 74.8430
	token := "mcc:solid_waste:" + uuid.NewString()
	require.NoError(t, app.DB.Create(&models.MCCSession{
		ContactID: contact.ID, OrganizationID: account.OrganizationID,
		Category: models.MCCCategorySolidWaste, Stage: models.MCCStageFormSent, FlowToken: token,
		Latitude: &lat, Longitude: &lon,
	}).Error)
	reply := ExtractedMessage{Type: "nfm_reply", FlowResponseData: map[string]any{
		"flow_token": token, "complaint_type": "garbage_burning", "ward": "ward_37",
		"address": "2nd cross, Maroli", "description": "Garbage being burnt every evening",
		"photos": []any{map[string]any{"id": "media-photo-1", "mime_type": "image/jpeg"}},
	}}
	require.True(t, app.handleMCCMessage(account, contact, waMessageID, reply))
	var g models.MCCGrievance
	require.NoError(t, app.DB.Where("wa_message_id = ?", waMessageID).First(&g).Error)
	return &g
}

func reloadMCCGrievance(t *testing.T, app *App, id uint64) *models.MCCGrievance {
	t.Helper()
	var g models.MCCGrievance
	require.NoError(t, app.DB.First(&g, id).Error)
	return &g
}

// inWindow marks a contact as having messaged recently so free-form
// messages are allowed.
func inWindow(t *testing.T, app *App, contact *models.Contact) {
	t.Helper()
	now := time.Now()
	require.NoError(t, app.DB.Model(contact).Update("last_inbound_at", now).Error)
	contact.LastInboundAt = &now
}

func TestMCC_AutoAssignNotifiesWardSupervisor(t *testing.T) {
	app, meta, account, citizen := newMCCTestApp(t)
	createMCCSupervisor(t, app, account.OrganizationID, "Catch-all", mccTestOtherPhone, 0, "")
	sup := createMCCSupervisor(t, app, account.OrganizationID, "Ward 37 SW", mccTestSupervisorPhone, 37, models.MCCCategorySolidWaste)
	supContact, _, err := contactutil.GetOrCreateContact(app.DB, account.OrganizationID, mccTestSupervisorPhone, "")
	require.NoError(t, err)
	inWindow(t, app, supContact)

	g := fileMCCGrievance(t, app, account, citizen, "wamid.flow1")

	assert.Equal(t, models.MCCStatusAssigned, g.Status)
	require.NotNil(t, g.SupervisorID)
	assert.Equal(t, sup.ID, *g.SupervisorID, "most specific supervisor wins over the catch-all")
	assert.Equal(t, supContact.ID, *g.SupervisorContactID)
	assert.NotNil(t, g.AssignedAt)
	assert.NotNil(t, g.NotifiedAt)
	assert.Empty(t, g.NotifyError)
	assert.Len(t, g.History, 2)

	msgs := meta.messagesTo(mccTestSupervisorPhone)
	require.Len(t, msgs, 2, "photo then card")
	assert.Contains(t, msgs[0], `"type":"image"`)
	assert.Contains(t, msgs[0], *g.ReferenceNo+" – photo 1/1")
	card := msgs[1]
	assert.Contains(t, card, "New complaint assigned to you")
	assert.Contains(t, card, *g.ReferenceNo)
	assert.Contains(t, card, "Burning of garbage")
	assert.Contains(t, card, "37 - Maroli")
	assert.Contains(t, card, "maps?q=12.869800,74.843000")
	assert.Contains(t, card, mccBtnProgress+mccIDString(g))
	assert.Contains(t, card, mccBtnClose+mccIDString(g))
	assert.Empty(t, meta.messagesTo(mccTestOtherPhone), "catch-all supervisor not notified")
}

func TestMCC_NoSupervisorLeavesGrievanceOpen(t *testing.T) {
	app, _, account, citizen := newMCCTestApp(t)
	g := fileMCCGrievance(t, app, account, citizen, "wamid.flow1")
	assert.Equal(t, models.MCCStatusOpen, g.Status)
	assert.Nil(t, g.SupervisorID)
	assert.Contains(t, g.NotifyError, "no supervisor configured")
}

func TestMCC_OutsideWindowUsesSupervisorTemplate(t *testing.T) {
	app, meta, account, citizen := newMCCTestApp(t)
	app.Config.MCC.SupervisorTemplate = "mcc_new_complaint"
	createMCCSupervisor(t, app, account.OrganizationID, "Sup", mccTestSupervisorPhone, 37, "")
	tpl := testutil.CreateTestTemplate(t, app.DB, account.OrganizationID, account.Name)
	tpl.Name = "mcc_new_complaint"
	tpl.Buttons = models.JSONBArray{map[string]any{"type": "QUICK_REPLY", "text": "View complaint"}}
	require.NoError(t, app.DB.Save(tpl).Error)

	g := fileMCCGrievance(t, app, account, citizen, "wamid.flow1")

	msgs := meta.messagesTo(mccTestSupervisorPhone)
	require.Len(t, msgs, 1, "template only, no free-form card")
	assert.Contains(t, msgs[0], `"type":"template"`)
	assert.Contains(t, msgs[0], `"name":"mcc_new_complaint"`)
	assert.Contains(t, msgs[0], `"text":"`+*g.ReferenceNo+`"`)
	assert.Contains(t, msgs[0], `"sub_type":"quick_reply"`)
	assert.Contains(t, msgs[0], `"payload":"`+mccBtnOpen+mccIDString(g)+`"`)
	assert.NotNil(t, reloadMCCGrievance(t, app, g.ID).NotifiedAt)

	// Tapping the template button (now in-window) sends the full card.
	supContact, _, _ := contactutil.GetOrCreateContact(app.DB, account.OrganizationID, mccTestSupervisorPhone, "")
	inWindow(t, app, supContact)
	require.True(t, app.handleMCCMessage(account, supContact, "wamid.tap", ExtractedMessage{Type: "button_reply", ButtonID: mccBtnOpen + mccIDString(g)}))
	assert.Contains(t, meta.lastMessageTo(mccTestSupervisorPhone), mccBtnClose+mccIDString(g))
}

func TestMCC_SupervisorClosesWithDetailsAndCitizenIsNotified(t *testing.T) {
	app, meta, account, citizen := newMCCTestApp(t)
	createMCCSupervisor(t, app, account.OrganizationID, "Sup", mccTestSupervisorPhone, 37, "")
	supContact, _, _ := contactutil.GetOrCreateContact(app.DB, account.OrganizationID, mccTestSupervisorPhone, "")
	inWindow(t, app, supContact)
	inWindow(t, app, citizen)
	g := fileMCCGrievance(t, app, account, citizen, "wamid.flow1")
	id := mccIDString(g)

	// In progress: citizen hears work has started.
	require.True(t, app.handleMCCMessage(account, supContact, "wamid.s1", ExtractedMessage{Type: "button_reply", ButtonID: mccBtnProgress + id}))
	g = reloadMCCGrievance(t, app, g.ID)
	assert.Equal(t, models.MCCStatusInProgress, g.Status)
	assert.NotNil(t, g.InProgressAt)
	assert.Contains(t, meta.lastMessageTo(citizen.PhoneNumber), "has started")
	assert.Contains(t, meta.lastMessageTo(mccTestSupervisorPhone), mccBtnClose+id)

	// Close: bot asks for details.
	require.True(t, app.handleMCCMessage(account, supContact, "wamid.s2", ExtractedMessage{Type: "button_reply", ButtonID: mccBtnClose + id}))
	assert.Contains(t, meta.lastMessageTo(mccTestSupervisorPhone), "action taken")
	var session models.MCCSession
	require.NoError(t, app.DB.Where("contact_id = ?", supContact.ID).First(&session).Error)
	assert.Equal(t, models.MCCStageAwaitingAction, session.Stage)
	require.NotNil(t, session.GrievanceID)
	assert.Equal(t, g.ID, *session.GrievanceID)

	// A photo without caption is kept and text is asked for.
	photoPath := filepath.Join("images", "done.jpg")
	require.NoError(t, os.MkdirAll(filepath.Join(app.Config.Storage.LocalPath, "images"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(app.Config.Storage.LocalPath, photoPath), []byte("jpeg"), 0o644))
	require.True(t, app.handleMCCMessage(account, supContact, "wamid.s3", ExtractedMessage{Type: "image", Media: &MediaInfo{MediaURL: photoPath, MediaMimeType: "image/jpeg"}}))
	assert.Contains(t, meta.lastMessageTo(mccTestSupervisorPhone), "Photo received")
	assert.Equal(t, models.MCCStatusInProgress, reloadMCCGrievance(t, app, g.ID).Status, "not closed yet")

	// Text closes it.
	citizenBefore := len(meta.messagesTo(citizen.PhoneNumber))
	require.True(t, app.handleMCCMessage(account, supContact, "wamid.s4", ExtractedMessage{Type: "text", Text: "Garbage cleared and warning issued on 10 Nov"}))
	g = reloadMCCGrievance(t, app, g.ID)
	assert.Equal(t, models.MCCStatusResolved, g.Status)
	assert.Equal(t, "Garbage cleared and warning issued on 10 Nov", g.ActionTaken)
	assert.Equal(t, models.JSONBArray{photoPath}, g.ActionPhotoURLs)
	assert.NotNil(t, g.ResolvedAt)
	assert.Contains(t, meta.lastMessageTo(mccTestSupervisorPhone), "closed")
	var sessions int64
	app.DB.Model(&models.MCCSession{}).Where("contact_id = ?", supContact.ID).Count(&sessions)
	assert.Zero(t, sessions)

	citizenMsgs := meta.messagesTo(citizen.PhoneNumber)[citizenBefore:]
	require.Len(t, citizenMsgs, 2, "work photo then closure notice")
	assert.Contains(t, citizenMsgs[0], `"type":"image"`)
	assert.Contains(t, citizenMsgs[1], "has been resolved")
	assert.Contains(t, citizenMsgs[1], "Garbage cleared and warning issued")
	assert.Contains(t, citizenMsgs[1], mccBtnOK+id)
	assert.Contains(t, citizenMsgs[1], mccBtnReopen+id)

	// Satisfied closes for good.
	require.True(t, app.handleMCCMessage(account, citizen, "wamid.c1", ExtractedMessage{Type: "button_reply", ButtonID: mccBtnOK + id}))
	g = reloadMCCGrievance(t, app, g.ID)
	assert.Equal(t, models.MCCStatusClosed, g.Status)
	assert.NotNil(t, g.ClosedAt)
	assert.Contains(t, meta.lastMessageTo(citizen.PhoneNumber), "now closed")

	// Tapping Satisfied again is answered, not re-applied.
	require.True(t, app.handleMCCMessage(account, citizen, "wamid.c2", ExtractedMessage{Type: "button_reply", ButtonID: mccBtnOK + id}))
	assert.Contains(t, meta.lastMessageTo(citizen.PhoneNumber), "already closed")
}

func TestMCC_CitizenReopensAndSupervisorIsRenotified(t *testing.T) {
	app, meta, account, citizen := newMCCTestApp(t)
	createMCCSupervisor(t, app, account.OrganizationID, "Sup", mccTestSupervisorPhone, 37, "")
	supContact, _, _ := contactutil.GetOrCreateContact(app.DB, account.OrganizationID, mccTestSupervisorPhone, "")
	inWindow(t, app, supContact)
	inWindow(t, app, citizen)
	g := fileMCCGrievance(t, app, account, citizen, "wamid.flow1")
	id := mccIDString(g)

	// Reopen before resolution is refused.
	require.True(t, app.handleMCCMessage(account, citizen, "wamid.c0", ExtractedMessage{Type: "button_reply", ButtonID: mccBtnReopen + id}))
	assert.Contains(t, meta.lastMessageTo(citizen.PhoneNumber), "cannot be reopened yet")

	app.closeMCCGrievance(account, g, "Cleared", nil, "supervisor")

	require.True(t, app.handleMCCMessage(account, citizen, "wamid.c1", ExtractedMessage{Type: "button_reply", ButtonID: mccBtnReopen + id}))
	assert.Contains(t, meta.lastMessageTo(citizen.PhoneNumber), "tell us briefly")

	supBefore := len(meta.messagesTo(mccTestSupervisorPhone))
	require.True(t, app.handleMCCMessage(account, citizen, "wamid.c2", ExtractedMessage{Type: "text", Text: "Still burning near the same spot"}))
	g = reloadMCCGrievance(t, app, g.ID)
	assert.Equal(t, models.MCCStatusReopened, g.Status)
	assert.Equal(t, 1, g.ReopenCount)
	assert.Equal(t, "Still burning near the same spot", g.ReopenReason)
	assert.Nil(t, g.ResolvedAt)
	assert.Contains(t, meta.lastMessageTo(citizen.PhoneNumber), "has been reopened")

	supMsgs := meta.messagesTo(mccTestSupervisorPhone)[supBefore:]
	require.NotEmpty(t, supMsgs)
	card := supMsgs[len(supMsgs)-1]
	assert.Contains(t, card, "reopened by the citizen")
	assert.Contains(t, card, "Still burning near the same spot")
	assert.Contains(t, card, mccBtnClose+id)

	// The citizen's next plain text goes back to the menu, not the reopen flow.
	require.True(t, app.handleMCCMessage(account, citizen, "wamid.c3", ExtractedMessage{Type: "text", Text: "hi"}))
	assert.Contains(t, meta.lastMessageTo(citizen.PhoneNumber), "mcc_solid_waste")

	// Closure outside the window with a template carries both button payloads.
	app.Config.MCC.CitizenClosureTemplate = "mcc_resolved"
	tpl := testutil.CreateTestTemplate(t, app.DB, account.OrganizationID, account.Name)
	tpl.Name = "mcc_resolved"
	tpl.Buttons = models.JSONBArray{
		map[string]any{"type": "QUICK_REPLY", "text": "Satisfied"},
		map[string]any{"type": "QUICK_REPLY", "text": "Reopen"},
	}
	require.NoError(t, app.DB.Save(tpl).Error)
	require.NoError(t, app.DB.Model(citizen).Update("last_inbound_at", nil).Error)
	app.closeMCCGrievance(account, g, "Cleared again", nil, "supervisor")
	last := meta.lastMessageTo(citizen.PhoneNumber)
	assert.Contains(t, last, `"name":"mcc_resolved"`)
	assert.Contains(t, last, `"payload":"`+mccBtnOK+id+`"`)
	assert.Contains(t, last, `"payload":"`+mccBtnReopen+id+`"`)
}

func TestMCC_SupervisorInboxAndAuthorization(t *testing.T) {
	app, meta, account, citizen := newMCCTestApp(t)
	createMCCSupervisor(t, app, account.OrganizationID, "Sup", mccTestSupervisorPhone, 37, "")
	supContact, _, _ := contactutil.GetOrCreateContact(app.DB, account.OrganizationID, mccTestSupervisorPhone, "")
	inWindow(t, app, supContact)
	g := fileMCCGrievance(t, app, account, citizen, "wamid.flow1")
	id := mccIDString(g)

	// Supervisor text -> inbox list, not the citizen menu.
	require.True(t, app.handleMCCMessage(account, supContact, "wamid.s1", ExtractedMessage{Type: "text", Text: "hello"}))
	inbox := meta.lastMessageTo(mccTestSupervisorPhone)
	assert.Contains(t, inbox, "pending complaints (1)")
	assert.Contains(t, inbox, mccBtnOpen+id)
	assert.NotContains(t, inbox, "mcc_solid_waste")

	// "new" still reaches the citizen menu.
	require.True(t, app.handleMCCMessage(account, supContact, "wamid.s2", ExtractedMessage{Type: "text", Text: "New"}))
	assert.Contains(t, meta.lastMessageTo(mccTestSupervisorPhone), "mcc_solid_waste")

	// Someone else cannot act on the complaint.
	other, _, _ := contactutil.GetOrCreateContact(app.DB, account.OrganizationID, mccTestOtherPhone, "")
	require.True(t, app.handleMCCMessage(account, other, "wamid.o1", ExtractedMessage{Type: "button_reply", ButtonID: mccBtnClose + id}))
	assert.Contains(t, meta.lastMessageTo(mccTestOtherPhone), "not assigned to you")
	require.True(t, app.handleMCCMessage(account, other, "wamid.o2", ExtractedMessage{Type: "button_reply", ButtonID: mccBtnReopen + id}))
	assert.Contains(t, meta.lastMessageTo(mccTestOtherPhone), "not filed from your number")
	assert.Equal(t, models.MCCStatusAssigned, reloadMCCGrievance(t, app, g.ID).Status)

	// Unknown buttons fall through to the chatbot.
	assert.False(t, app.handleMCCMessage(account, other, "wamid.o3", ExtractedMessage{Type: "button_reply", ButtonID: "btn_1"}))
}

func TestMCC_SupervisorAPI(t *testing.T) {
	app, meta, account, citizen := newMCCTestApp(t)
	org := account.OrganizationID
	role := testutil.CreateAdminRole(t, app.DB, org)
	user := testutil.CreateTestUser(t, app.DB, org, testutil.WithRoleID(&role.ID))

	// Create
	req := testutil.NewJSONRequest(t, map[string]any{"name": "Ward 37", "phone_number": "+" + mccTestSupervisorPhone, "ward_no": 37})
	testutil.SetAuthContext(req, org, user.ID)
	require.NoError(t, app.CreateMCCSupervisor(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))
	var created struct {
		Data models.MCCSupervisor `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &created))
	assert.Equal(t, mccTestSupervisorPhone, created.Data.PhoneNumber, "+ stripped")
	assert.True(t, created.Data.IsActive)

	// Validation
	req = testutil.NewJSONRequest(t, map[string]any{"name": "X", "phone_number": "abc", "ward_no": 99})
	testutil.SetAuthContext(req, org, user.ID)
	require.NoError(t, app.CreateMCCSupervisor(req))
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))

	// List
	req = testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org, user.ID)
	require.NoError(t, app.ListMCCSupervisors(req))
	assert.Contains(t, string(testutil.GetResponseBody(req)), `"Ward 37"`)

	// Grievance filed -> auto-assigned to the API-created supervisor.
	g := fileMCCGrievance(t, app, account, citizen, "wamid.flow1")
	require.NotNil(t, g.SupervisorID)
	assert.Equal(t, created.Data.ID, *g.SupervisorID)

	// List / get grievances
	req = testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org, user.ID)
	testutil.SetQueryParam(req, "status", models.MCCStatusAssigned)
	require.NoError(t, app.ListMCCGrievances(req))
	assert.Contains(t, string(testutil.GetResponseBody(req)), *g.ReferenceNo)

	req = testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org, user.ID)
	testutil.SetPathParam(req, "id", *g.ReferenceNo)
	require.NoError(t, app.GetMCCGrievance(req))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	// Reassign to another supervisor, who gets notified.
	other := createMCCSupervisor(t, app, org, "Other", mccTestOtherPhone, 0, "")
	req = testutil.NewJSONRequest(t, map[string]any{"supervisor_id": other.ID.String()})
	testutil.SetAuthContext(req, org, user.ID)
	testutil.SetPathParam(req, "id", mccIDString(g))
	require.NoError(t, app.AssignMCCGrievance(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))
	g = reloadMCCGrievance(t, app, g.ID)
	assert.Equal(t, other.ID, *g.SupervisorID)
	assert.NotEmpty(t, meta.messagesTo(mccTestOtherPhone))

	// Resolve from the dashboard notifies the citizen.
	req = testutil.NewJSONRequest(t, map[string]any{"action_taken": "Cleared by MCC truck"})
	testutil.SetAuthContext(req, org, user.ID)
	testutil.SetPathParam(req, "id", mccIDString(g))
	require.NoError(t, app.ResolveMCCGrievance(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
	assert.Equal(t, models.MCCStatusResolved, reloadMCCGrievance(t, app, g.ID).Status)
	assert.Contains(t, meta.lastMessageTo(citizen.PhoneNumber), "Cleared by MCC truck")

	// Delete
	req = testutil.NewRequest(t)
	testutil.SetAuthContext(req, org, user.ID)
	testutil.SetPathParam(req, "id", other.ID.String())
	require.NoError(t, app.DeleteMCCSupervisor(req))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	// Agents lack mcc permissions.
	agentRole := testutil.CreateAgentRole(t, app.DB, org)
	agent := testutil.CreateTestUser(t, app.DB, org, testutil.WithRoleID(&agentRole.ID))
	req = testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org, agent.ID)
	require.NoError(t, app.ListMCCSupervisors(req))
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))
}

func TestMCC_QuickReplyPayloads(t *testing.T) {
	tpl := &models.Template{Buttons: models.JSONBArray{
		map[string]any{"type": "URL", "text": "Site"},
		map[string]any{"type": "QUICK_REPLY", "text": "A"},
		map[string]any{"type": "QUICK_REPLY", "text": "B"},
	}}
	assert.Equal(t, map[string]string{"1": "x", "2": "y"}, mccQuickReplyPayloads(tpl, "x", "y", "z"))
}
