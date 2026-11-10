package handlers

import (
	"errors"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

// Admin API for the MCC grievance workflow: the supervisor roster that
// auto-assignment reads, and read/reassign/resolve access to grievances.

type MCCSupervisorRequest struct {
	Name        string `json:"name"`
	PhoneNumber string `json:"phone_number"`
	WardNo      int    `json:"ward_no"`  // 0 = all wards
	Category    string `json:"category"` // "" = all categories
	IsActive    *bool  `json:"is_active"`
}

func (req *MCCSupervisorRequest) validate() string {
	req.Name = strings.TrimSpace(req.Name)
	req.PhoneNumber = strings.TrimPrefix(strings.TrimSpace(req.PhoneNumber), "+")
	req.Category = strings.TrimSpace(req.Category)
	switch {
	case req.Name == "":
		return "name is required"
	case req.PhoneNumber == "" || strings.Trim(req.PhoneNumber, "0123456789") != "":
		return "phone_number must be digits with country code, e.g. 919900000000"
	case req.WardNo < 0 || req.WardNo > len(mccWards):
		return "ward_no must be 0 (all wards) or between 1 and " + strconv.Itoa(len(mccWards))
	case req.Category != "" && mccFormByCategory(req.Category) == nil:
		return "category must be empty (all), " + models.MCCCategorySolidWaste + " or " + models.MCCCategorySewage
	}
	return ""
}

// ListMCCSupervisors returns the supervisor roster.
func (a *App) ListMCCSupervisors(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceMCC, models.ActionRead)
	if err != nil {
		return nil
	}
	var sups []models.MCCSupervisor
	if err := a.DB.Where("organization_id = ?", orgID).Order("ward_no ASC, category ASC, name ASC").Find(&sups).Error; err != nil {
		a.Log.Error("Failed to list MCC supervisors", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list supervisors", nil, "")
	}
	return r.SendEnvelope(map[string]any{"supervisors": sups, "wards": mccWards})
}

// CreateMCCSupervisor adds a supervisor.
func (a *App) CreateMCCSupervisor(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceMCC, models.ActionWrite)
	if err != nil {
		return nil
	}
	var req MCCSupervisorRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if msg := req.validate(); msg != "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, msg, nil, "")
	}
	sup := models.MCCSupervisor{
		OrganizationID: orgID,
		Name:           req.Name,
		PhoneNumber:    req.PhoneNumber,
		WardNo:         req.WardNo,
		Category:       req.Category,
		IsActive:       req.IsActive == nil || *req.IsActive,
	}
	if err := a.DB.Create(&sup).Error; err != nil {
		a.Log.Error("Failed to create MCC supervisor", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create supervisor", nil, "")
	}
	a.logAudit(orgID, userID, models.ResourceMCC, sup.ID, models.AuditActionCreated, nil, sup)
	return r.SendEnvelope(sup)
}

// UpdateMCCSupervisor edits a supervisor.
func (a *App) UpdateMCCSupervisor(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceMCC, models.ActionWrite)
	if err != nil {
		return nil
	}
	id, err := uuid.Parse(r.RequestCtx.UserValue("id").(string))
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid supervisor ID", nil, "")
	}
	sup, err := findByIDAndOrg[models.MCCSupervisor](a.DB, r, id, orgID, "Supervisor")
	if err != nil {
		return nil
	}
	var req MCCSupervisorRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if msg := req.validate(); msg != "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, msg, nil, "")
	}
	old := *sup
	sup.Name, sup.PhoneNumber, sup.WardNo, sup.Category = req.Name, req.PhoneNumber, req.WardNo, req.Category
	if req.IsActive != nil {
		sup.IsActive = *req.IsActive
	}
	if err := a.DB.Save(sup).Error; err != nil {
		a.Log.Error("Failed to update MCC supervisor", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to update supervisor", nil, "")
	}
	a.logAudit(orgID, userID, models.ResourceMCC, sup.ID, models.AuditActionUpdated, old, sup)
	return r.SendEnvelope(sup)
}

// DeleteMCCSupervisor removes a supervisor. Grievances already assigned to
// them keep their assignment.
func (a *App) DeleteMCCSupervisor(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceMCC, models.ActionDelete)
	if err != nil {
		return nil
	}
	id, err := uuid.Parse(r.RequestCtx.UserValue("id").(string))
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid supervisor ID", nil, "")
	}
	sup, err := findByIDAndOrg[models.MCCSupervisor](a.DB, r, id, orgID, "Supervisor")
	if err != nil {
		return nil
	}
	if err := a.DB.Delete(sup).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to delete supervisor", nil, "")
	}
	a.logAudit(orgID, userID, models.ResourceMCC, sup.ID, models.AuditActionDeleted, sup, nil)
	return r.SendEnvelope(map[string]any{"deleted": true})
}

// ListMCCGrievances lists grievances, newest first, with optional
// status / ward_no / category / supervisor_id / search filters.
func (a *App) ListMCCGrievances(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceMCC, models.ActionRead)
	if err != nil {
		return nil
	}
	pg := parsePagination(r)
	args := r.RequestCtx.QueryArgs()
	q := a.DB.Where("organization_id = ?", orgID)
	if v := string(args.Peek("status")); v != "" {
		q = q.Where("status IN (?)", strings.Split(v, ","))
	}
	if v, _ := strconv.Atoi(string(args.Peek("ward_no"))); v > 0 {
		q = q.Where("ward_no = ?", v)
	}
	if v := string(args.Peek("category")); v != "" {
		q = q.Where("category = ?", v)
	}
	if v := string(args.Peek("supervisor_id")); v != "" {
		if v == "none" {
			q = q.Where("supervisor_id IS NULL")
		} else if id, err := uuid.Parse(v); err == nil {
			q = q.Where("supervisor_id = ?", id)
		}
	}
	if v := strings.TrimSpace(string(args.Peek("search"))); v != "" {
		like := "%" + v + "%"
		q = q.Where("reference_no ILIKE ? OR phone_number ILIKE ? OR address ILIKE ? OR description ILIKE ?", like, like, like, like)
	}

	var total int64
	q.Model(&models.MCCGrievance{}).Count(&total)
	var items []models.MCCGrievance
	if err := pg.Apply(q.Order("created_at DESC")).Find(&items).Error; err != nil {
		a.Log.Error("Failed to list MCC grievances", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list grievances", nil, "")
	}
	return r.SendEnvelope(listEnvelope("grievances", items, total, pg))
}

// GetMCCGrievance returns one grievance by serial ID or reference number.
func (a *App) GetMCCGrievance(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceMCC, models.ActionRead)
	if err != nil {
		return nil
	}
	g, err := a.mccGrievanceByRef(orgID, r.RequestCtx.UserValue("id").(string))
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Grievance not found", nil, "")
		}
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load grievance", nil, "")
	}
	return r.SendEnvelope(g)
}

// AssignMCCGrievance (re)assigns a grievance to a supervisor and notifies
// them on WhatsApp. Body: {"supervisor_id": "<uuid>"}; omit it to re-run
// the automatic ward/category lookup.
func (a *App) AssignMCCGrievance(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceMCC, models.ActionWrite)
	if err != nil {
		return nil
	}
	g, err := a.mccGrievanceByRef(orgID, r.RequestCtx.UserValue("id").(string))
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Grievance not found", nil, "")
	}
	if g.Status == models.MCCStatusClosed {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Grievance is closed", nil, "")
	}
	var req struct {
		SupervisorID string `json:"supervisor_id"`
	}
	_ = r.Decode(&req, "json")

	account, err := a.mccAccountFor(g)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "WhatsApp account not found", nil, "")
	}

	var sup *models.MCCSupervisor
	if req.SupervisorID != "" {
		id, perr := uuid.Parse(req.SupervisorID)
		if perr != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid supervisor ID", nil, "")
		}
		sup, err = findByIDAndOrg[models.MCCSupervisor](a.DB, r, id, orgID, "Supervisor")
		if err != nil {
			return nil
		}
	} else if sup, err = a.findMCCSupervisor(orgID, g.WardNo, g.Category); err != nil || sup == nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "No supervisor matches this ward and category", nil, "")
	}

	old := *g
	a.assignMCCGrievanceTo(account, g, sup, "user:"+userID.String())
	a.logAudit(orgID, userID, models.ResourceMCC, uuid.Nil, models.AuditActionUpdated, old, g,
		map[string]any{"grievance_id": g.ID, "reference_no": mccRef(g)})
	return r.SendEnvelope(g)
}

// ResolveMCCGrievance closes a grievance from the dashboard with the action
// taken, notifying the citizen exactly as a supervisor closure would.
func (a *App) ResolveMCCGrievance(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceMCC, models.ActionWrite)
	if err != nil {
		return nil
	}
	g, err := a.mccGrievanceByRef(orgID, r.RequestCtx.UserValue("id").(string))
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Grievance not found", nil, "")
	}
	if mccIsFinished(g) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Grievance is already "+g.Status, nil, "")
	}
	var req struct {
		ActionTaken string `json:"action_taken"`
	}
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	req.ActionTaken = strings.TrimSpace(req.ActionTaken)
	if req.ActionTaken == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "action_taken is required", nil, "")
	}
	account, err := a.mccAccountFor(g)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "WhatsApp account not found", nil, "")
	}
	old := *g
	a.closeMCCGrievance(account, g, req.ActionTaken, nil, "user:"+userID.String())
	a.logAudit(orgID, userID, models.ResourceMCC, uuid.Nil, models.AuditActionUpdated, old, g,
		map[string]any{"grievance_id": g.ID, "reference_no": mccRef(g)})
	return r.SendEnvelope(g)
}

func (a *App) mccAccountFor(g *models.MCCGrievance) (*models.WhatsAppAccount, error) {
	var account models.WhatsAppAccount
	err := a.DB.Where("organization_id = ? AND name = ?", g.OrganizationID, g.WhatsAppAccount).First(&account).Error
	if err != nil {
		return nil, err
	}
	return &account, nil
}
