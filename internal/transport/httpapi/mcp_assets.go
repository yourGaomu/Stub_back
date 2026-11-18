package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yourGaomu/short_links/back/internal/modules/assets"
	"github.com/yourGaomu/short_links/back/internal/modules/links"
)

type mcpToolResult struct {
	Content           []mcpTextContent `json:"content"`
	StructuredContent any              `json:"structuredContent,omitempty"`
	IsError           bool             `json:"isError"`
}

type mcpTextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func mcpTextResult(text string, isError bool) mcpToolResult {
	return mcpToolResult{Content: []mcpTextContent{{Type: "text", Text: text}}, IsError: isError}
}

// The MCP asset contract intentionally exposes only these stable metadata fields.
// Multipart tools return the existing UploadSessionDTO unchanged.
type mcpAssetDTO struct {
	AssetID        string    `json:"assetId"`
	BusinessID     string    `json:"businessId"`
	FileName       string    `json:"fileName"`
	ContentType    string    `json:"contentType"`
	SizeBytes      int64     `json:"sizeBytes"`
	ChecksumSHA256 string    `json:"checksumSha256"`
	CreatedAt      time.Time `json:"createdAt"`
}

type mcpAssetDownloadDTO struct {
	Asset       mcpAssetDTO `json:"asset"`
	DownloadURL string      `json:"downloadUrl"`
	ExpiresAt   time.Time   `json:"expiresAt"`
}

type mcpAssetShareDTO struct {
	Asset     mcpAssetDTO `json:"asset"`
	ShortURL  string      `json:"shortUrl"`
	Code      string      `json:"code"`
	ExpiresAt *time.Time  `json:"expiresAt,omitempty"`
	MaxVisits int64       `json:"maxVisits"`
}

func mcpAsset(dto assets.AssetDTO) mcpAssetDTO {
	return mcpAssetDTO{AssetID: dto.AssetID, BusinessID: dto.BusinessID, FileName: dto.FileName, ContentType: dto.ContentType, SizeBytes: dto.SizeBytes, ChecksumSHA256: dto.ChecksumSHA256, CreatedAt: dto.CreatedAt.UTC()}
}

func mcpObjectSchema(properties map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}

func mcpPublicIDSchema(prefix string) map[string]any {
	return map[string]any{"type": "string", "pattern": "^" + prefix + `_[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`}
}

func mcpAssetSchema() map[string]any {
	return mcpObjectSchema(map[string]any{
		"assetId": mcpPublicIDSchema("ast"), "businessId": map[string]any{"type": "string"},
		"fileName": map[string]any{"type": "string"}, "contentType": map[string]any{"type": "string"},
		"sizeBytes": map[string]any{"type": "integer", "minimum": int64(0)}, "checksumSha256": map[string]any{"type": "string"},
		"createdAt": map[string]any{"type": "string", "format": "date-time"},
	}, "assetId", "businessId", "fileName", "contentType", "sizeBytes", "checksumSha256", "createdAt")
}

func mcpAssetShareSchema() map[string]any {
	return mcpObjectSchema(map[string]any{
		"asset": mcpAssetSchema(), "shortUrl": map[string]any{"type": "string", "format": "uri"}, "code": map[string]any{"type": "string"},
		"expiresAt": map[string]any{"type": "string", "format": "date-time"}, "maxVisits": map[string]any{"type": "integer", "minimum": int64(-1)},
	}, "asset", "shortUrl", "code", "maxVisits")
}

func mcpUploadSessionSchema() map[string]any {
	asset := mcpAssetSchema()
	properties := asset["properties"].(map[string]any)
	properties["path"] = map[string]any{"type": "string"}
	properties["deletedAt"] = map[string]any{"anyOf": []any{map[string]any{"type": "string", "format": "date-time"}, map[string]any{"type": "null"}}}
	asset["required"] = append(asset["required"].([]string), "path", "deletedAt")
	return mcpObjectSchema(map[string]any{
		"uploadSessionId": mcpPublicIDSchema("upl"), "idempotencyKey": map[string]any{"type": "string"},
		"status":   map[string]any{"type": "string", "enum": []string{"initializing", "uploading", "completing", "completed", "cleanup_pending", "cancelled", "expired", "failed"}},
		"fileName": map[string]any{"type": "string"}, "businessId": map[string]any{"type": "string"}, "path": map[string]any{"type": "string"},
		"sizeBytes": map[string]any{"type": "integer"}, "partSizeBytes": map[string]any{"type": "integer"}, "partCount": map[string]any{"type": "integer"},
		"receivedParts": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}, "receivedBytes": map[string]any{"type": "integer"},
		"expiresAt": map[string]any{"type": "string", "format": "date-time"}, "asset": asset,
	}, "uploadSessionId", "status", "fileName", "businessId", "path", "sizeBytes", "partSizeBytes", "partCount", "receivedParts", "receivedBytes", "expiresAt")
}

// Validate the advertised flat input schemas before any service call or side effect.
func validateMCPArguments(args map[string]any, schema map[string]any) error {
	properties := schema["properties"].(map[string]any)
	if required, ok := schema["required"].([]string); ok {
		for _, key := range required {
			if _, exists := args[key]; !exists {
				return fmt.Errorf("错误: 必须提供 %s 参数", key)
			}
		}
	}
	for key, value := range args {
		raw, ok := properties[key]
		if !ok {
			return fmt.Errorf("错误: 未知参数 %s", key)
		}
		property := raw.(map[string]any)
		switch property["type"] {
		case "string":
			text, ok := value.(string)
			if !ok {
				return fmt.Errorf("错误: %s 必须是字符串", key)
			}
			text = strings.TrimSpace(text)
			length := utf8.RuneCountInString(text)
			if minimum, ok := property["minLength"].(int); ok && length < minimum {
				return fmt.Errorf("错误: %s 不能为空", key)
			}
			if maximum, ok := property["maxLength"].(int); ok && length > maximum {
				return fmt.Errorf("错误: %s 超过允许长度", key)
			}
			if pattern, ok := property["pattern"].(string); ok {
				if matches, _ := regexp.MatchString(pattern, text); !matches {
					return fmt.Errorf("错误: %s 格式无效", key)
				}
			}
			if property["format"] == "date-time" {
				if _, err := time.Parse(time.RFC3339, text); err != nil {
					return fmt.Errorf("错误: %s 必须是 RFC3339 时间", key)
				}
			}
		case "integer":
			number, err := mcpInt64(value)
			if err != nil {
				return fmt.Errorf("错误: %s 必须是 int64 整数", key)
			}
			if minimum, ok := property["minimum"]; ok {
				bound, _ := mcpInt64(minimum)
				if number < bound {
					return fmt.Errorf("错误: %s 不能小于 %d", key, bound)
				}
			}
			if maximum, ok := property["maximum"]; ok {
				bound, _ := mcpInt64(maximum)
				if number > bound {
					return fmt.Errorf("错误: %s 不能大于 %d", key, bound)
				}
			}
		}
	}
	return nil
}

func mcpInt64(value any) (int64, error) {
	switch number := value.(type) {
	case json.Number:
		return number.Int64()
	case int64:
		return number, nil
	case int:
		return int64(number), nil
	case float64:
		if !math.IsNaN(number) && !math.IsInf(number, 0) && math.Trunc(number) == number && number >= -0x1p63 && number < 0x1p63 {
			return int64(number), nil
		}
	}
	return 0, fmt.Errorf("invalid integer")
}

func executeMCPAssetTool(ctx context.Context, name string, args map[string]any, mCtx mcpContext, deps Dependencies) (any, error) {
	permission := "assets.upload"
	if name == "get_asset_info" || name == "get_asset_download_url" || name == "create_asset_shortlink" {
		permission = "assets.read"
	}
	if err := requireMCPPermission(ctx, mCtx, deps, permission); err != nil {
		return nil, err
	}
	if name == "create_asset_shortlink" || name == "upload_file_and_share" {
		if err := requireMCPPermission(ctx, mCtx, deps, "links.create_asset"); err != nil {
			return nil, err
		}
	}
	switch name {
	case "get_asset_info", "get_asset_download_url", "create_asset_shortlink":
		var expiresAt *time.Time
		if name == "create_asset_shortlink" {
			if raw, ok := args["expiresAt"]; ok {
				parsed, _ := time.Parse(time.RFC3339, strings.TrimSpace(raw.(string)))
				if !parsed.After(time.Now()) {
					return nil, fmt.Errorf("到期时间必须晚于当前时间")
				}
				parsed = parsed.UTC()
				expiresAt = &parsed
			}
		}
		asset, err := deps.Assets.Get(ctx, mCtx.assetActor, getArgString(args, "assetId"))
		if err != nil {
			return nil, fmt.Errorf("查询素材失败: %w", err)
		}
		if asset.DeletedAt != nil {
			return nil, fmt.Errorf("素材不存在或无权访问")
		}
		if name == "get_asset_info" {
			return mcpAsset(asset), nil
		}
		if name == "get_asset_download_url" {
			// Capture before Content signs the URL, so the advertised expiry never
			// exceeds the service's 15-minute TTL, even when signing/audit is slow.
			expiresAt := time.Now().UTC().Truncate(time.Second).Add(15 * time.Minute)
			location, err := deps.Assets.Content(ctx, mCtx.assetActor, asset.AssetID, mCtx.assetMeta)
			if err != nil {
				return nil, fmt.Errorf("获取素材下载地址失败: %w", err)
			}
			return mcpAssetDownloadDTO{Asset: mcpAsset(asset), DownloadURL: location, ExpiresAt: expiresAt}, nil
		}
		return createMCPAssetShare(ctx, asset, args, expiresAt, mCtx, deps)

	case "upload_file_and_share":
		data, err := decodeMCPFileContent(getArgString(args, "fileContentBase64"))
		if err != nil {
			return nil, fmt.Errorf("小文件上传内容无效（Base64，最大 7 MiB）: %w", err)
		}
		file, err := bytesToMultipartFileHeader(data, getArgString(args, "fileName"))
		if err != nil {
			return nil, fmt.Errorf("构造上传文件头失败: %w", err)
		}
		businessID := mCtx.assetActor.BusinessID
		if businessID == "" {
			businessID = "default"
		}
		asset, err := deps.Assets.Upload(ctx, mCtx.assetActor, assets.UploadInput{BusinessID: businessID, Path: "agent-uploads", File: file}, mCtx.assetMeta)
		if err != nil {
			return nil, fmt.Errorf("素材上传失败: %w", err)
		}
		result, err := createMCPAssetShare(ctx, asset, args, nil, mCtx, deps)
		if err != nil {
			return nil, fmt.Errorf("素材已上传(ID: %s)，但创建短链失败: %w", asset.AssetID, err)
		}
		return result, nil

	case "begin_asset_upload":
		businessID := getArgString(args, "businessId")
		if mCtx.assetActor.BusinessID != "" {
			if businessID != "" && businessID != mCtx.assetActor.BusinessID {
				return nil, fmt.Errorf("API Key 未授权该业务")
			}
			businessID = mCtx.assetActor.BusinessID
		}
		path := getArgString(args, "path")
		if path == "" {
			path = "agent-uploads"
		}
		size, _ := mcpInt64(args["sizeBytes"])
		return deps.Assets.BeginMultipartUpload(ctx, mCtx.assetActor, assets.BeginUploadInput{BusinessID: businessID, Path: path, FileName: getArgString(args, "fileName"), ContentType: getArgString(args, "contentType"), SizeBytes: size, IdempotencyKey: getArgString(args, "idempotencyKey")})
	case "upload_asset_part":
		reader, size, err := prepareMCPPartContent(getArgString(args, "contentBase64"))
		if err != nil {
			return nil, fmt.Errorf("分片内容无效（Base64，最大 32 MiB）: %w", err)
		}
		if size == 0 {
			return nil, fmt.Errorf("单个分片内容必须大于 0 且不超过 32 MiB")
		}
		part, _ := mcpInt64(args["partNumber"])
		return deps.Assets.UploadMultipartPart(ctx, mCtx.assetActor, assets.UploadPartInput{SessionID: getArgString(args, "uploadSessionId"), PartNumber: int(part), Reader: reader, ContentLength: size})
	case "get_asset_upload_status":
		return deps.Assets.GetMultipartUpload(ctx, mCtx.assetActor, getArgString(args, "uploadSessionId"))
	case "complete_asset_upload":
		return deps.Assets.CompleteMultipartUpload(ctx, mCtx.assetActor, getArgString(args, "uploadSessionId"), mCtx.assetMeta)
	case "cancel_asset_upload":
		return deps.Assets.CancelMultipartUpload(ctx, mCtx.assetActor, getArgString(args, "uploadSessionId"))
	}
	return nil, fmt.Errorf("未知工具: %s", name)
}

func createMCPAssetShare(ctx context.Context, asset assets.AssetDTO, args map[string]any, expiresAt *time.Time, mCtx mcpContext, deps Dependencies) (mcpAssetShareDTO, error) {
	maxVisits := int64(-1)
	if value, ok := args["maxVisits"]; ok {
		maxVisits, _ = mcpInt64(value)
	}
	title := getArgString(args, "title")
	if title == "" {
		title = string([]rune(asset.FileName)[:min(120, len([]rune(asset.FileName)))])
	}
	link, err := deps.Links.Create(ctx, mCtx.actor, links.CreateRequest{
		Title: title, ExpiresAt: expiresAt, Target: links.TargetInput{Type: "minio_asset", AssetID: asset.AssetID},
		RuleSource: links.RuleSourceInput{Type: "inline", Rules: &links.RulesInput{MaxVisits: &maxVisits}},
	}, mCtx.linkMeta)
	if err != nil {
		return mcpAssetShareDTO{}, err
	}
	return mcpAssetShareDTO{Asset: mcpAsset(asset), ShortURL: link.URL, Code: link.Code, ExpiresAt: link.ExpiresAt, MaxVisits: link.Rules.MaxVisits}, nil
}
