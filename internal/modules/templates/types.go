package templates

import (
	"time"

	"github.com/yourGaomu/short_links/back/internal/modules/links"
)

type CreateRequest struct {
	WorkspaceID string           `json:"workspaceId"`
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Rules       links.RulesInput `json:"rules"`
}

type UpdateRequest struct {
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
}

type VersionRequest struct {
	ChangeNote string           `json:"changeNote,omitempty"`
	Rules      links.RulesInput `json:"rules"`
}

type VersionDTO struct {
	Version    int32       `json:"version"`
	Rules      links.Rules `json:"rules"`
	ChangeNote string      `json:"changeNote,omitempty"`
	CreatedAt  time.Time   `json:"createdAt"`
}

type TemplateDTO struct {
	TemplateID          string      `json:"templateId"`
	WorkspaceID         string      `json:"workspaceId"`
	Name                string      `json:"name"`
	Description         string      `json:"description"`
	Status              string      `json:"status"`
	LatestVersion       int32       `json:"latestVersion"`
	ReferencedLinkCount int64       `json:"referencedLinkCount"`
	LatestRules         links.Rules `json:"latestRules"`
	DeletedAt           *time.Time  `json:"deletedAt"`
	CreatedAt           time.Time   `json:"createdAt"`
	UpdatedAt           time.Time   `json:"updatedAt"`
}

type ListItem struct {
	TemplateID          string    `json:"templateId"`
	WorkspaceID         string    `json:"workspaceId"`
	Name                string    `json:"name"`
	Status              string    `json:"status"`
	LatestVersion       int32     `json:"latestVersion"`
	ReferencedLinkCount int64     `json:"referencedLinkCount"`
	UpdatedAt           time.Time `json:"updatedAt"`
}

type Page struct {
	Items      []ListItem       `json:"items"`
	Pagination links.Pagination `json:"pagination"`
}

type VersionList struct {
	TemplateID string               `json:"templateId"`
	Versions   []VersionListItemDTO `json:"versions"`
}

type VersionListItemDTO struct {
	Version    int32     `json:"version"`
	ChangeNote string    `json:"changeNote"`
	CreatedAt  time.Time `json:"createdAt"`
}

type Meta struct {
	SourceIP     string
	UserAgent    string
	RequestID    string
	AuditDetails map[string]any
}
