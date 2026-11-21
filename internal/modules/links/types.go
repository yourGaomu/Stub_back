package links

import (
	"encoding/json"
	"time"
)

type Problem struct {
	Status  int
	Code    string
	Message string
}

func (p *Problem) Error() string { return p.Message }

type CreateRequest struct {
	Title          string          `json:"title,omitempty"`
	Tags           []string        `json:"tags,omitempty"`
	GroupID        *string         `json:"groupId,omitempty"`
	Code           string          `json:"code,omitempty"`
	Target         TargetInput     `json:"target"`
	ExpiresAt      *time.Time      `json:"expiresAt"`
	ActiveFrom     *time.Time      `json:"activeFrom"`
	ActiveUntil    *time.Time      `json:"activeUntil"`
	AccessPassword string          `json:"accessPassword,omitempty"`
	RuleSource     RuleSourceInput `json:"ruleSource"`
}

type TargetInput struct {
	Type    string `json:"type"`
	URL     string `json:"url,omitempty"`
	AssetID string `json:"assetId,omitempty"`
}

type RuleSourceInput struct {
	Type     string       `json:"type"`
	Rules    *RulesInput  `json:"rules,omitempty"`
	Template *TemplateRef `json:"template,omitempty"`
}

type RulesInput struct {
	MaxVisits           *int64          `json:"maxVisits,omitempty"`
	AllowedIPCIDRs      []string        `json:"allowedIpCidrs"`
	RequirePassword     bool            `json:"requirePassword"`
	WrongPasswordPolicy *PasswordPolicy `json:"wrongPasswordPolicy,omitempty"`
}

type Rules struct {
	MaxVisits           int64           `json:"maxVisits"`
	AllowedIPCIDRs      []string        `json:"allowedIpCidrs"`
	RequirePassword     bool            `json:"requirePassword"`
	WrongPasswordPolicy *PasswordPolicy `json:"wrongPasswordPolicy,omitempty"`
}

type PasswordPolicy struct {
	Strategy string `json:"strategy"`
}

type TemplateRef struct {
	TemplateID string `json:"templateId"`
	Version    *int32 `json:"version,omitempty"`
}

type LinkDTO struct {
	LinkID       string       `json:"linkId"`
	BusinessID   string       `json:"businessId,omitempty"`
	Title        string       `json:"title"`
	Tags         []string     `json:"tags"`
	GroupID      *string      `json:"groupId"`
	GroupName    *string      `json:"groupName"`
	Code         string       `json:"code"`
	URL          string       `json:"url"`
	Target       TargetDTO    `json:"target"`
	Status       string       `json:"status"`
	Availability string       `json:"availability"`
	Rules        Rules        `json:"rules"`
	ExpiresAt    *time.Time   `json:"expiresAt"`
	ActiveFrom   *time.Time   `json:"activeFrom"`
	ActiveUntil  *time.Time   `json:"activeUntil"`
	VisitsUsed   int64        `json:"visitsUsed"`
	RuleTemplate *TemplateRef `json:"ruleTemplate"`
	CreatedAt    time.Time    `json:"createdAt"`
	UpdatedAt    time.Time    `json:"updatedAt"`
}

type TargetDTO struct {
	Type           string `json:"type"`
	URL            string `json:"url,omitempty"`
	AssetID        string `json:"assetId,omitempty"`
	AssetSizeBytes *int64 `json:"assetSizeBytes,omitempty"`
}

type CreateResponse struct {
	LinkID       string       `json:"linkId"`
	BusinessID   string       `json:"businessId,omitempty"`
	Title        string       `json:"title"`
	Tags         []string     `json:"tags"`
	GroupID      *string      `json:"groupId"`
	GroupName    *string      `json:"groupName"`
	Code         string       `json:"code"`
	URL          string       `json:"url"`
	Target       TargetDTO    `json:"target"`
	ExpiresAt    *time.Time   `json:"expiresAt"`
	ActiveFrom   *time.Time   `json:"activeFrom"`
	ActiveUntil  *time.Time   `json:"activeUntil"`
	Rules        Rules        `json:"rules"`
	RuleTemplate *TemplateRef `json:"ruleTemplate"`
}

type ListItem struct {
	LinkID         string     `json:"linkId"`
	Title          string     `json:"title"`
	Tags           []string   `json:"tags"`
	GroupID        *string    `json:"groupId"`
	GroupName      *string    `json:"groupName"`
	Code           string     `json:"code"`
	URL            string     `json:"url"`
	TargetType     string     `json:"targetType"`
	TargetURL      string     `json:"targetUrl,omitempty"`
	AssetID        string     `json:"assetId,omitempty"`
	AssetSizeBytes *int64     `json:"assetSizeBytes,omitempty"`
	Status         string     `json:"status"`
	Availability   string     `json:"availability"`
	ExpiresAt      *time.Time `json:"expiresAt"`
	ActiveFrom     *time.Time `json:"activeFrom"`
	ActiveUntil    *time.Time `json:"activeUntil"`
	VisitsUsed     int64      `json:"visitsUsed"`
	MaxVisits      int64      `json:"maxVisits"`
	CreatedAt      time.Time  `json:"createdAt"`
}

type Page struct {
	Items      []ListItem `json:"items"`
	Pagination Pagination `json:"pagination"`
}

type Pagination struct {
	Page     int   `json:"page"`
	PageSize int   `json:"pageSize"`
	Total    int64 `json:"total"`
}

type RenewRequest struct {
	ExpiresAt    time.Time `json:"expiresAt"`
	CodeStrategy string    `json:"codeStrategy,omitempty"`
}

type RotateRequest struct {
	Strategy string `json:"strategy"`
	Code     string `json:"code,omitempty"`
}

type UpdateMetadataRequest struct {
	Title   *string         `json:"title"`
	Tags    *[]string       `json:"tags"`
	GroupID json.RawMessage `json:"groupId,omitempty"`
}

type GroupDTO struct {
	GroupID     string    `json:"groupId"`
	WorkspaceID string    `json:"workspaceId"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	LinkCount   int64     `json:"linkCount"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type GroupPage struct {
	Items      []GroupDTO `json:"items"`
	Pagination Pagination `json:"pagination"`
}

type CreateGroupRequest struct {
	Name        string  `json:"name"`
	Description string  `json:"description,omitempty"`
	WorkspaceID *string `json:"workspaceId,omitempty"`
}

type UpdateGroupRequest struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
}

type DeleteGroupResponse struct {
	GroupID       string `json:"groupId"`
	UnlinkedCount int64  `json:"unlinkedCount"`
	Deleted       bool   `json:"deleted"`
}

type UpdateRulesRequest struct {
	ExpiresAt           json.RawMessage `json:"expiresAt,omitempty"`
	ActiveFrom          json.RawMessage `json:"activeFrom,omitempty"`
	ActiveUntil         json.RawMessage `json:"activeUntil,omitempty"`
	MaxVisits           *int64          `json:"maxVisits,omitempty"`
	AllowedIPCIDRs      *[]string       `json:"allowedIpCidrs,omitempty"`
	RequirePassword     *bool           `json:"requirePassword,omitempty"`
	AccessPassword      *string         `json:"accessPassword,omitempty"`
	WrongPasswordPolicy *PasswordPolicy `json:"wrongPasswordPolicy,omitempty"`
}

type PublicResult struct {
	StatusCode int
	Location   string
	PageTitle  string
	PageText   string
	Challenge  bool
}
