package models

import (
	"time"

	"github.com/google/uuid"
)

// MCC grievance categories (one per WhatsApp Flow form).
const (
	MCCCategorySolidWaste = "solid_waste"
	MCCCategorySewage     = "sewage"
)

// MCCGrievance is a complaint submitted through the Mangaluru Mahanagara
// Palike WhatsApp Flow forms. The serial ID backs the human-facing
// ReferenceNo (e.g. MMP-SW-000042).
type MCCGrievance struct {
	ID          uint64    `gorm:"primaryKey;autoIncrement" json:"id"`
	ReferenceNo *string   `gorm:"size:32;uniqueIndex" json:"reference_no"` // set right after insert from ID
	CreatedAt   time.Time `gorm:"autoCreateTime;index" json:"created_at"`
	UpdatedAt   time.Time `gorm:"autoUpdateTime" json:"updated_at"`

	OrganizationID  uuid.UUID `gorm:"type:uuid;index;not null" json:"organization_id"`
	WhatsAppAccount string    `gorm:"size:100;not null" json:"whatsapp_account"`
	ContactID       uuid.UUID `gorm:"type:uuid;index;not null" json:"contact_id"`
	PhoneNumber     string    `gorm:"size:32;index;not null" json:"phone_number"`
	ProfileName     string    `gorm:"size:255" json:"profile_name"`

	Category          string     `gorm:"size:20;index;not null" json:"category"` // solid_waste, sewage
	ComplaintTypeCode string     `gorm:"size:50;not null" json:"complaint_type_code"`
	ComplaintType     string     `gorm:"size:255;not null" json:"complaint_type"`
	WardNo            int        `gorm:"index" json:"ward_no"`
	WardName          string     `gorm:"size:100" json:"ward_name"`
	QRCode            string     `gorm:"size:255" json:"qr_code"`
	Address           string     `gorm:"type:text" json:"address"`
	Description       string     `gorm:"type:text" json:"description"`
	PhotoURLs         JSONBArray `gorm:"type:jsonb;default:'[]'" json:"photo_urls"`

	// Location the citizen shared before the form, and its reverse-geocoded
	// address (what the form's Address field was pre-filled with).
	Latitude        *float64 `json:"latitude"`
	Longitude       *float64 `json:"longitude"`
	DetectedAddress string   `gorm:"type:text" json:"detected_address"`

	// Resolution timeline quoted to the citizen; DueAt is nil when unknown.
	SLA   string     `gorm:"size:50" json:"sla"`
	DueAt *time.Time `gorm:"index" json:"due_at"`

	// Lifecycle: open (no supervisor found) -> assigned -> in_progress ->
	// resolved (supervisor closed it) -> closed (citizen satisfied), or
	// resolved -> reopened (citizen dissatisfied) -> resolved again.
	Status string `gorm:"size:20;default:'open';index" json:"status"`

	// Supervisor the complaint was auto-assigned to (by ward/category) and
	// the contact row their WhatsApp notifications go to.
	SupervisorID        *uuid.UUID `gorm:"type:uuid;index" json:"supervisor_id"`
	SupervisorName      string     `gorm:"size:255" json:"supervisor_name"`
	SupervisorPhone     string     `gorm:"size:32" json:"supervisor_phone"`
	SupervisorContactID *uuid.UUID `gorm:"type:uuid;index" json:"supervisor_contact_id"`
	AssignedAt          *time.Time `json:"assigned_at"`
	NotifiedAt          *time.Time `json:"notified_at"`
	NotifyError         string     `gorm:"type:text" json:"notify_error"`

	// Closure details entered by the supervisor on WhatsApp.
	ActionTaken     string     `gorm:"type:text" json:"action_taken"`
	ActionPhotoURLs JSONBArray `gorm:"type:jsonb;default:'[]'" json:"action_photo_urls"`
	FixedOn         *time.Time `json:"fixed_on"`
	WorkLocation    string     `gorm:"type:text" json:"work_location"`
	InProgressAt    *time.Time `json:"in_progress_at"`
	ResolvedAt      *time.Time `json:"resolved_at"`
	ClosedAt        *time.Time `json:"closed_at"`

	// Citizen reopen tracking.
	ReopenCount  int    `gorm:"default:0" json:"reopen_count"`
	ReopenReason string `gorm:"type:text" json:"reopen_reason"`

	// History is an append-only audit of status changes:
	// [{"status":..., "at":..., "by":..., "note":...}].
	History JSONBArray `gorm:"type:jsonb;default:'[]'" json:"history"`

	// WAMessageID of the nfm_reply; unique so webhook retries don't duplicate.
	WAMessageID string `gorm:"size:255;uniqueIndex" json:"wa_message_id"`
	FlowToken   string `gorm:"size:100" json:"flow_token"`
	RawResponse JSONB  `gorm:"type:jsonb" json:"raw_response"`
}

func (MCCGrievance) TableName() string {
	return "mcc_grievances"
}

// MCC grievance statuses.
const (
	MCCStatusOpen       = "open"
	MCCStatusAssigned   = "assigned"
	MCCStatusInProgress = "in_progress"
	MCCStatusResolved   = "resolved"
	MCCStatusReopened   = "reopened"
	MCCStatusClosed     = "closed"
)

// MCC conversation stages.
const (
	MCCStageAwaitingLocation = "awaiting_location"
	MCCStageFormSent         = "form_sent"
	// Supervisor tapped "Close complaint" and owes the action-taken details.
	MCCStageAwaitingAction = "awaiting_action"
	// Supervisor was sent the closure Flow form (FlowToken set).
	MCCStageCloseFormSent = "close_form_sent"
	// Citizen tapped "Reopen" and owes the reason.
	MCCStageAwaitingReopenReason = "awaiting_reopen_reason"
)

// MCCSession tracks one citizen's in-progress complaint between WhatsApp
// messages: the category picked, then the location shared for the form.
type MCCSession struct {
	ContactID       uuid.UUID `gorm:"type:uuid;primaryKey" json:"contact_id"`
	OrganizationID  uuid.UUID `gorm:"type:uuid;index;not null" json:"organization_id"`
	Category        string    `gorm:"size:20;not null" json:"category"`
	Stage           string    `gorm:"size:40;not null" json:"stage"`
	FlowToken       string    `gorm:"size:100" json:"flow_token"`
	Latitude        *float64  `json:"latitude"`
	Longitude       *float64  `json:"longitude"`
	DetectedAddress string    `gorm:"type:text" json:"detected_address"`
	// GrievanceID is the complaint an action/reopen stage refers to, and
	// PendingPhotos the action photos received before the text details.
	// No default tag: the session is upserted with OnConflict UpdateAll,
	// which skips columns that have one.
	GrievanceID   *uint64    `json:"grievance_id"`
	PendingPhotos JSONBArray `gorm:"type:jsonb" json:"pending_photos"`
	UpdatedAt     time.Time  `gorm:"autoUpdateTime" json:"updated_at"`
}

func (MCCSession) TableName() string {
	return "mcc_sessions"
}

// MCCSupervisor is a field supervisor complaints are auto-assigned to.
// WardNo 0 matches every ward and an empty Category matches every
// category; the most specific active match wins.
type MCCSupervisor struct {
	BaseModel
	OrganizationID uuid.UUID `gorm:"type:uuid;index;not null" json:"organization_id"`
	Name           string    `gorm:"size:255;not null" json:"name"`
	PhoneNumber    string    `gorm:"size:32;not null;index" json:"phone_number"`
	WardNo         int       `gorm:"index;default:0" json:"ward_no"`
	Category       string    `gorm:"size:20;default:''" json:"category"`
	IsActive       bool      `gorm:"default:true" json:"is_active"`
}

func (MCCSupervisor) TableName() string {
	return "mcc_supervisors"
}
