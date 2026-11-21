package notifications

import "time"

const (
	KindExpiry24h = "expiry_24h"
	KindVisits80  = "visits_80"
)

type Problem struct {
	Status  int
	Code    string
	Message string
}

func (p *Problem) Error() string { return p.Message }

type Notification struct {
	NotificationID string     `json:"notificationId"`
	WorkspaceID    string     `json:"workspaceId"`
	LinkID         *string    `json:"linkId"`
	Kind           string     `json:"kind"`
	Title          string     `json:"title"`
	Summary        string     `json:"summary"`
	CreatedAt      time.Time  `json:"createdAt"`
	ReadAt         *time.Time `json:"readAt"`
}

type Page struct {
	Items      []Notification `json:"items"`
	Pagination Pagination     `json:"pagination"`
}

type Pagination struct {
	Page     int   `json:"page"`
	PageSize int   `json:"pageSize"`
	Total    int64 `json:"total"`
}

type Settings struct {
	ExpiryEnabled bool `json:"expiryEnabled"`
	VisitsEnabled bool `json:"visitsEnabled"`
}

type UpdateSettingsRequest struct {
	ExpiryEnabled *bool `json:"expiryEnabled"`
	VisitsEnabled *bool `json:"visitsEnabled"`
}
