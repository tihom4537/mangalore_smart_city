package handlers

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/contactutil"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mccMetaMock is a fake Graph API that records requests and serves the
// flow-management, media and messages endpoints the grievance bot uses.
type mccMetaMock struct {
	mu       sync.Mutex
	requests []string // "METHOD /path"
	bodies   []string
	server   *httptest.Server
}

func newMCCMetaMock(t *testing.T) *mccMetaMock {
	m := &mccMetaMock{}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.requests = append(m.requests, r.Method+" "+r.URL.Path)
		m.bodies = append(m.bodies, string(body))
		m.mu.Unlock()

		p := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/flows"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/flows"):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "meta-flow-1"})
		case strings.HasSuffix(p, "/assets"), strings.HasSuffix(p, "/publish"):
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
		case strings.HasSuffix(p, "/messages"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"messages": []map[string]string{{"id": "wamid.out_" + uuid.New().String()[:8]}},
			})
		case strings.HasSuffix(p, "/media-photo-1"):
			_ = json.NewEncoder(w).Encode(map[string]any{"url": m.server.URL + "/cdn/photo-1"})
		case p == "/reverse":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"display_name": "Hampankatta, Mangaluru, Dakshina Kannada, Karnataka, 575001, India",
			})
		case strings.HasPrefix(p, "/cdn/"):
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte("fake-jpeg"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(m.server.Close)
	return m
}

func (m *mccMetaMock) count(prefix string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, r := range m.requests {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

func (m *mccMetaMock) lastMessageBody() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.requests) - 1; i >= 0; i-- {
		if strings.HasSuffix(m.requests[i], "/messages") {
			return m.bodies[i]
		}
	}
	return ""
}

func newMCCTestApp(t *testing.T) (*App, *mccMetaMock, *models.WhatsAppAccount, *models.Contact) {
	t.Helper()
	db := testutil.SetupTestDB(t)
	log := testutil.NopLogger()
	meta := newMCCMetaMock(t)

	app := &App{
		DB:         db,
		Log:        log,
		WhatsApp:   whatsapp.NewWithBaseURL(log, meta.server.URL),
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
		Config: &config.Config{
			MCC:     config.MCCConfig{Enabled: true, GeocoderURL: meta.server.URL},
			Storage: config.StorageConfig{LocalPath: t.TempDir()},
		},
	}
	org := testutil.CreateTestOrganization(t, db)
	account := testutil.CreateTestWhatsAppAccount(t, db, org.ID)
	contact, _, err := contactutil.GetOrCreateContact(db, org.ID, "919900000001", "Test Citizen")
	require.NoError(t, err)
	return app, meta, account, contact
}

func TestMCC_DisabledFallsThrough(t *testing.T) {
	app, meta, account, contact := newMCCTestApp(t)
	app.Config.MCC.Enabled = false

	handled := app.handleMCCMessage(account, contact, "wamid.in1", ExtractedMessage{Type: "text", Text: "hi"})
	assert.False(t, handled)
	assert.Zero(t, meta.count("POST"), "no Meta calls when disabled")
}

func TestMCC_TextSendsMenu(t *testing.T) {
	app, meta, account, contact := newMCCTestApp(t)

	handled := app.handleMCCMessage(account, contact, "wamid.in1", ExtractedMessage{Type: "text", Text: "hi"})
	require.True(t, handled)

	body := meta.lastMessageBody()
	assert.Contains(t, body, `"type":"button"`)
	assert.Contains(t, body, "mcc_solid_waste")
	assert.Contains(t, body, "mcc_sewage")
}

func TestMCC_ButtonAsksForLocation(t *testing.T) {
	app, meta, account, contact := newMCCTestApp(t)

	click := ExtractedMessage{Type: "button_reply", ButtonID: "mcc_sewage", Text: "Sewage / Drainage"}
	require.True(t, app.handleMCCMessage(account, contact, "wamid.in1", click))

	body := meta.lastMessageBody()
	assert.Contains(t, body, `"type":"location_request_message"`)
	assert.Contains(t, body, `"name":"send_location"`)
	assert.Zero(t, meta.count("POST /v18.0/"+account.BusinessID+"/flows"), "form not sent yet")

	var session models.MCCSession
	require.NoError(t, app.DB.Where("contact_id = ?", contact.ID).First(&session).Error)
	assert.Equal(t, models.MCCCategorySewage, session.Category)
	assert.Equal(t, models.MCCStageAwaitingLocation, session.Stage)
}

func TestMCC_LocationPrefillsAddressAndProvisionsFlowOnce(t *testing.T) {
	app, meta, account, contact := newMCCTestApp(t)
	mccPublishTried = sync.Map{}

	click := ExtractedMessage{Type: "button_reply", ButtonID: "mcc_sewage", Text: "Sewage / Drainage"}
	location := ExtractedMessage{Type: "location", Text: `{"latitude":12.8698,"longitude":74.8430}`}
	require.True(t, app.handleMCCMessage(account, contact, "wamid.in1", click))
	require.True(t, app.handleMCCMessage(account, contact, "wamid.loc1", location))
	assert.Equal(t, 1, meta.count("GET /reverse"), "location reverse geocoded")

	assert.Equal(t, 1, meta.count("POST /v18.0/"+account.BusinessID+"/flows"), "flow created")
	assert.Equal(t, 1, meta.count("POST /v18.0/meta-flow-1/assets"), "flow JSON uploaded")
	assert.Equal(t, 1, meta.count("POST /v18.0/meta-flow-1/publish"), "flow published")

	var flow models.WhatsAppFlow
	require.NoError(t, app.DB.Where("whats_app_account = ? AND name = ?", account.Name, "MMP Sewage Complaint v1").First(&flow).Error)
	assert.Equal(t, "meta-flow-1", flow.MetaFlowID)
	assert.Equal(t, "PUBLISHED", flow.Status)

	body := meta.lastMessageBody()
	assert.Contains(t, body, `"flow_id":"meta-flow-1"`)
	assert.Contains(t, body, `"screen":"SEWAGE_COMPLAINT"`)
	assert.Contains(t, body, `"flow_token":"mcc:sewage:`)
	assert.NotContains(t, body, `"mode":"draft"`)
	assert.Contains(t, body, `"data":{"address":"Hampankatta, Mangaluru, Dakshina Kannada, Karnataka, 575001"}`)

	var session models.MCCSession
	require.NoError(t, app.DB.Where("contact_id = ?", contact.ID).First(&session).Error)
	assert.Equal(t, models.MCCStageFormSent, session.Stage)
	require.NotNil(t, session.Latitude)
	assert.InDelta(t, 12.8698, *session.Latitude, 1e-9)
	assert.Contains(t, body, session.FlowToken)

	// A second complaint reuses the stored flow.
	require.True(t, app.handleMCCMessage(account, contact, "wamid.in2", click))
	require.True(t, app.handleMCCMessage(account, contact, "wamid.loc2", location))
	assert.Equal(t, 1, meta.count("POST /v18.0/"+account.BusinessID+"/flows"), "flow not recreated")
}

func TestMCC_SkipLocationSendsBlankForm(t *testing.T) {
	app, meta, account, contact := newMCCTestApp(t)

	require.True(t, app.handleMCCMessage(account, contact, "wamid.in1",
		ExtractedMessage{Type: "button_reply", ButtonID: "mcc_solid_waste", Text: "Garbage / Waste"}))
	require.True(t, app.handleMCCMessage(account, contact, "wamid.in2", ExtractedMessage{Type: "text", Text: "skip"}))

	body := meta.lastMessageBody()
	assert.Contains(t, body, `"screen":"GARBAGE_COMPLAINT"`)
	assert.Contains(t, body, `"data":{"address":""}`)
	assert.Zero(t, meta.count("GET /reverse"))

	// Once the form is out, text goes back to the menu.
	require.True(t, app.handleMCCMessage(account, contact, "wamid.in3", ExtractedMessage{Type: "text", Text: "hi"}))
	assert.Contains(t, meta.lastMessageBody(), "mcc_sewage")
}

func TestMCC_LocationWithoutPendingFormSendsMenu(t *testing.T) {
	app, meta, account, contact := newMCCTestApp(t)
	require.True(t, app.handleMCCMessage(account, contact, "wamid.loc", ExtractedMessage{Type: "location", Text: `{"latitude":1,"longitude":2}`}))
	assert.Contains(t, meta.lastMessageBody(), "mcc_solid_waste")
	assert.Zero(t, meta.count("GET /reverse"))
}

func TestMCC_FlowReplySavesGrievanceOnce(t *testing.T) {
	app, meta, account, contact := newMCCTestApp(t)

	token := "mcc:solid_waste:" + uuid.NewString()
	lat, lon := 12.8698, 74.8430
	require.NoError(t, app.DB.Create(&models.MCCSession{
		ContactID: contact.ID, OrganizationID: account.OrganizationID,
		Category: models.MCCCategorySolidWaste, Stage: models.MCCStageFormSent, FlowToken: token,
		Latitude: &lat, Longitude: &lon, DetectedAddress: "Hampankatta, Mangaluru",
	}).Error)

	reply := ExtractedMessage{
		Type: "nfm_reply",
		FlowResponseData: map[string]any{
			"flow_token":     token,
			"complaint_type": "garbage_burning",
			"ward":           "ward_37",
			"qr_code":        "MCC-H-778",
			"address":        "2nd cross, Maroli",
			"description":    "Garbage being burnt every evening",
			"photos": []any{
				map[string]any{"id": "media-photo-1", "mime_type": "image/jpeg", "file_name": "p.jpg"},
			},
		},
	}
	require.True(t, app.handleMCCMessage(account, contact, "wamid.flow1", reply))

	var g models.MCCGrievance
	require.NoError(t, app.DB.Where("wa_message_id = ?", "wamid.flow1").First(&g).Error)
	require.NotNil(t, g.ReferenceNo)
	assert.Regexp(t, `^MMP-SW-\d{6}$`, *g.ReferenceNo)
	assert.Equal(t, models.MCCCategorySolidWaste, g.Category)
	assert.Equal(t, "Burning of garbage", g.ComplaintType)
	assert.Equal(t, 37, g.WardNo)
	assert.Equal(t, "Maroli", g.WardName)
	assert.Equal(t, "MCC-H-778", g.QRCode)
	assert.Equal(t, contact.ID, g.ContactID)
	assert.Equal(t, "open", g.Status)
	require.Len(t, g.PhotoURLs, 1, "photo downloaded and stored")
	require.NotNil(t, g.Latitude)
	assert.InDelta(t, lat, *g.Latitude, 1e-9)
	assert.Equal(t, "Hampankatta, Mangaluru", g.DetectedAddress)
	assert.Equal(t, "12 Hours", g.SLA)
	require.NotNil(t, g.DueAt)
	assert.WithinDuration(t, g.CreatedAt.Add(12*time.Hour), *g.DueAt, time.Minute)

	confirmation := meta.lastMessageBody()
	assert.Contains(t, confirmation, "Complaint ID: "+*g.ReferenceNo)
	assert.Contains(t, confirmation, "Resolution Timeline: 12 Hours")

	var sessions int64
	app.DB.Model(&models.MCCSession{}).Where("contact_id = ?", contact.ID).Count(&sessions)
	assert.Zero(t, sessions, "session cleared after submission")

	// A webhook retry of the same message must not create a second row or reply.
	sent := meta.count("POST /v18.0/" + account.PhoneID + "/messages")
	require.True(t, app.handleMCCMessage(account, contact, "wamid.flow1", reply))
	var n int64
	app.DB.Model(&models.MCCGrievance{}).Where("wa_message_id = ?", "wamid.flow1").Count(&n)
	assert.EqualValues(t, 1, n)
	assert.Equal(t, sent, meta.count("POST /v18.0/"+account.PhoneID+"/messages"), "no second confirmation")
}

func TestMCC_NonMCCFlowReplyFallsThrough(t *testing.T) {
	app, _, account, contact := newMCCTestApp(t)
	reply := ExtractedMessage{Type: "nfm_reply", FlowResponseData: map[string]any{"flow_token": "flow_123"}}
	assert.False(t, app.handleMCCMessage(account, contact, "wamid.x", reply), "other flows stay with the chatbot")
}
