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

	Status string `gorm:"size:20;default:'open';index" json:"status"` // open, in_progress, resolved, rejected

	// WAMessageID of the nfm_reply; unique so webhook retries don't duplicate.
	WAMessageID string `gorm:"size:255;uniqueIndex" json:"wa_message_id"`
	FlowToken   string `gorm:"size:100" json:"flow_token"`
	RawResponse JSONB  `gorm:"type:jsonb" json:"raw_response"`
}

func (MCCGrievance) TableName() string {
	return "mcc_grievances"
}

// MCC conversation stages.
const (
	MCCStageAwaitingLocation = "awaiting_location"
	MCCStageFormSent         = "form_sent"
)

// MCCSession tracks one citizen's in-progress complaint between WhatsApp
// messages: the category picked, then the location shared for the form.
type MCCSession struct {
	ContactID       uuid.UUID `gorm:"type:uuid;primaryKey" json:"contact_id"`
	OrganizationID  uuid.UUID `gorm:"type:uuid;index;not null" json:"organization_id"`
	Category        string    `gorm:"size:20;not null" json:"category"`
	Stage           string    `gorm:"size:20;not null" json:"stage"`
	FlowToken       string    `gorm:"size:100" json:"flow_token"`
	Latitude        *float64  `json:"latitude"`
	Longitude       *float64  `json:"longitude"`
	DetectedAddress string    `gorm:"type:text" json:"detected_address"`
	UpdatedAt       time.Time `gorm:"autoUpdateTime" json:"updated_at"`
}

func (MCCSession) TableName() string {
	return "mcc_sessions"
}
