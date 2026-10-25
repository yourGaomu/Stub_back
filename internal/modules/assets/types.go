package assets

import (
	"mime/multipart"
	"time"

	"github.com/google/uuid"
)

type Problem struct {
	Status  int
	Code    string
	Message string
}

func (p *Problem) Error() string { return p.Message }

type Actor struct {
	UserID      uuid.UUID
	WorkspaceID uuid.UUID
	Role        string
	BusinessID  string
	APIKeyID    *uuid.UUID
}
type Meta struct {
	SourceIP     string
	UserAgent    string
	RequestID    string
	AuditDetails map[string]any
}

type UploadInput struct {
	BusinessID string
	Path       string
	File       *multipart.FileHeader
}

type AssetDTO struct {
	AssetID        string     `json:"assetId"`
	BusinessID     string     `json:"businessId"`
	Path           string     `json:"path"`
	FileName       string     `json:"fileName"`
	ContentType    string     `json:"contentType"`
	SizeBytes      int64      `json:"sizeBytes"`
	ChecksumSHA256 string     `json:"checksumSha256"`
	DeletedAt      *time.Time `json:"deletedAt"`
	CreatedAt      time.Time  `json:"createdAt"`
}

type ListItem struct{ AssetDTO }
type Page struct {
	Items      []ListItem `json:"items"`
	Pagination struct {
		Page     int   `json:"page"`
		PageSize int   `json:"pageSize"`
		Total    int64 `json:"total"`
	} `json:"pagination"`
}

type DeleteResponse struct {
	AssetID    string    `json:"assetId"`
	DeletedAt  time.Time `json:"deletedAt"`
	PurgeAfter time.Time `json:"purgeAfter"`
	Status     string    `json:"status"`
}
type RestoreResponse struct {
	AssetID    string    `json:"assetId"`
	RestoredAt time.Time `json:"restoredAt"`
	Status     string    `json:"status"`
}

type MetaUpload struct {
	AssetID        string `json:"assetId"`
	BusinessID     string `json:"businessId"`
	Path           string `json:"path"`
	FileName       string `json:"fileName"`
	ContentType    string `json:"contentType"`
	SizeBytes      int64  `json:"sizeBytes"`
	ChecksumSHA256 string `json:"checksumSha256"`
}
