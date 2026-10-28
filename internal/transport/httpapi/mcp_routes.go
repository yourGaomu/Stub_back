package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yourGaomu/short_links/back/internal/modules/assets"
	"github.com/yourGaomu/short_links/back/internal/modules/auth"
	"github.com/yourGaomu/short_links/back/internal/modules/links"
)

type mcpContext struct {
	actor        links.Actor
	linkMeta     links.RequestMeta
	assetActor   assets.Actor
	assetMeta    assets.Meta
	apiKeyScopes map[string]bool
}

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      any           `json:"id"`
	Result  any           `json:"result,omitempty"`
	Error   *jsonRPCError `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

var mcpAvailableTools = []mcpToolDefinition{
	{
		Name:        "create_url_shortlink",
		Description: "缩短普通网页长链接（HTTPS/HTTP）。支持设置有效天数、最大访问次数限制（-1 表示不限）和访问口令保护。",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url": map[string]any{
					"type":        "string",
					"description": "需要缩短的目标网页完整 URL（必须包含 http:// 或 https://）",
				},
				"title": map[string]any{
					"type":        "string",
					"description": "短链标题或备注说明（可选）",
				},
				"expireDays": map[string]any{
					"type":        "integer",
					"description": "有效期天数（可选），如 7 或 30，留空或 0 表示永久有效",
				},
				"maxVisits": map[string]any{
					"type":        "integer",
					"description": "最大允许访问次数（可选），-1 或留空表示不限制，填正整数表示达到次数后失效",
				},
				"password": map[string]any{
					"type":        "string",
					"description": "访问口令保护（可选，4-32位字符），留空表示公开无密访问",
				},
				"code": map[string]any{
					"type":        "string",
					"description": "自定义短码（可选，4-16位字母或数字）",
				},
			},
			"required": []string{"url"},
		},
	},
	{
		Name:        "create_burn_link",
		Description: "创建【阅后即焚】短链：访问 1 次后立即自动销毁，且 24 小时后自动到期。极适合用于安全交付一次性密码、API Token、临时授权凭据或私密网页。",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url": map[string]any{
					"type":        "string",
					"description": "目标网页 URL 或凭据展示页面地址",
				},
				"title": map[string]any{
					"type":        "string",
					"description": "短链标题（可选，默认：阅后即焚安全分享）",
				},
				"password": map[string]any{
					"type":        "string",
					"description": "额外的访问口令保护（可选，4-32位字符）",
				},
			},
			"required": []string{"url"},
		},
	},
	{
		Name:        "upload_file_and_share",
		Description: "将不超过 7 MiB 的小文件内容（Base64 编码）上传至云端素材库并自动生成下载短链。大文件请使用分片上传工具。",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"fileName": map[string]any{
					"type":        "string",
					"description": "包含后缀的文件名（如 report.pdf, output.png, test.txt）",
				},
				"fileContentBase64": map[string]any{
					"type":        "string",
					"description": "文件完整内容的 Base64 编码字符串",
				},
				"title": map[string]any{
					"type":        "string",
					"description": "短链展示标题（可选）",
				},
				"maxVisits": map[string]any{
					"type":        "integer",
					"description": "最大允许下载次数限制（可选），-1 或留空表示不限制",
				},
			},
			"required": []string{"fileName", "fileContentBase64"},
		},
	},
	{
		Name: "begin_asset_upload", Description: "为文件内容创建分片上传会话并预留用户存储额度。随后用 upload_asset_part 逐片发送实际内容。",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"fileName": map[string]any{"type": "string"}, "contentType": map[string]any{"type": "string"}, "sizeBytes": map[string]any{"type": "integer"}, "path": map[string]any{"type": "string"}, "businessId": map[string]any{"type": "string"}, "idempotencyKey": map[string]any{"type": "string"},
		}, "required": []string{"fileName", "sizeBytes"}},
	},
	{
		Name: "upload_asset_part", Description: "上传一个分片的实际文件内容。contentBase64 仅是单个分片，不能传入本地路径或整个大文件。",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"uploadSessionId": map[string]any{"type": "string"}, "partNumber": map[string]any{"type": "integer"}, "contentBase64": map[string]any{"type": "string"},
		}, "required": []string{"uploadSessionId", "partNumber", "contentBase64"}},
	},
	{
		Name: "get_asset_upload_status", Description: "查询分片上传已接收的分片、总进度和会话状态。",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"uploadSessionId": map[string]any{"type": "string"}}, "required": []string{"uploadSessionId"}},
	},
	{
		Name: "complete_asset_upload", Description: "校验并合并全部分片，将预留额度结算为已用额度。",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"uploadSessionId": map[string]any{"type": "string"}}, "required": []string{"uploadSessionId"}},
	},
	{
		Name: "cancel_asset_upload", Description: "取消未完成的分片上传，清理 MinIO 分片并释放预留额度。",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"uploadSessionId": map[string]any{"type": "string"}}, "required": []string{"uploadSessionId"}},
	},
	{
		Name:        "get_link_info",
		Description: "根据短码（如 /r/k8x9z 中的 k8x9z）或短链 ID 查询短链的当前可用状态、访问次数、剩余配额及目标地址。",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"code": map[string]any{
					"type":        "string",
					"description": "短码（code）或短链 ID 或完整短链 URL",
				},
			},
			"required": []string{"code"},
		},
	},
	{
		Name:        "list_my_links",
		Description: "查询当前凭据授权下的最新短链列表、近期访问量与到期状态。",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pageSize": map[string]any{
					"type":        "integer",
					"description": "返回条数（默认 10，最大 50）",
				},
			},
		},
	},
}

func registerMCPRoutes(router *gin.Engine, deps Dependencies) {
	// 最新 MCP 官方规范：Streamable HTTP 单一端点（无状态、单次请求自闭环）
	mcp := router.Group("/api/mcp")
	mcp.Use(mcpAuthenticate(deps.Auth))

	mcp.POST("", func(c *gin.Context) {
		bodyBytes, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 48<<20))
		if err != nil {
			c.JSON(http.StatusBadRequest, jsonRPCResponse{
				JSONRPC: "2.0",
				Error:   &jsonRPCError{Code: -32700, Message: "请求体解析失败"},
			})
			return
		}

		var rpcReq jsonRPCRequest
		if err := json.Unmarshal(bodyBytes, &rpcReq); err != nil {
			c.JSON(http.StatusBadRequest, jsonRPCResponse{
				JSONRPC: "2.0",
				Error:   &jsonRPCError{Code: -32700, Message: "无效的 JSON-RPC 报文"},
			})
			return
		}

		mCtx := mcpContext{
			actor:      linkActor(c),
			linkMeta:   linkMeta(c),
			assetActor: assetActor(c),
			assetMeta:  assetMeta(c),
		}
		if identity := currentAuth(c); identity.APIKey != nil {
			mCtx.apiKeyScopes = identity.APIKey.Scopes
		}

		resp := handleMCPMethod(c.Request.Context(), rpcReq, mCtx, deps)
		if resp != nil {
			c.JSON(http.StatusOK, resp)
			return
		}

		c.Status(http.StatusAccepted)
	})
}

func mcpAuthenticate(service *auth.Service) gin.HandlerFunc {
	baseAuth := authenticate(service)
	return func(c *gin.Context) {
		// 1. 如果请求头未带凭据，支持从 Query 提取（方便只支持标准 EventSource URL 的客户端）
		if c.GetHeader("Authorization") == "" && c.GetHeader("X-API-Key") == "" {
			token := strings.TrimSpace(c.Query("apiKey"))
			if token == "" {
				token = strings.TrimSpace(c.Query("token"))
			}
			if token != "" {
				if strings.HasPrefix(token, "stub_sk_") {
					c.Request.Header.Set("X-API-Key", token)
				} else {
					c.Request.Header.Set("Authorization", "Bearer "+token)
				}
			}
		}
		baseAuth(c)
	}
}

func handleMCPMethod(ctx context.Context, req jsonRPCRequest, mCtx mcpContext, deps Dependencies) *jsonRPCResponse {
	switch req.Method {
	case "initialize":
		protoVer := "2024-11-05"
		var initParams struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if len(req.Params) > 0 {
			if err := json.Unmarshal(req.Params, &initParams); err == nil && initParams.ProtocolVersion != "" {
				protoVer = initParams.ProtocolVersion
			}
		}
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"protocolVersion": protoVer,
				"capabilities": map[string]any{
					"tools": map[string]any{},
				},
				"serverInfo": map[string]any{
					"name":    "short-links-mcp",
					"version": "1.0.0",
				},
			},
		}

	case "notifications/initialized":
		return nil

	case "ping":
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  map[string]any{},
		}

	case "tools/list":
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"tools": mcpAvailableTools,
			},
		}

	case "tools/call":
		var callParams struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &callParams); err != nil {
			return &jsonRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   &jsonRPCError{Code: -32602, Message: "工具调用参数格式无效"},
			}
		}

		resultText, isErr := executeMCPTool(ctx, callParams.Name, callParams.Arguments, mCtx, deps)
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"content": []map[string]any{
					{
						"type": "text",
						"text": resultText,
					},
				},
				"isError": isErr,
			},
		}

	default:
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &jsonRPCError{Code: -32601, Message: fmt.Sprintf("未知方法: %s", req.Method)},
		}
	}
}

func executeMCPTool(ctx context.Context, name string, args map[string]any, mCtx mcpContext, deps Dependencies) (string, bool) {
	if args == nil {
		args = make(map[string]any)
	}

	switch name {
	case "create_url_shortlink":
		rawURL := getArgString(args, "url")
		if rawURL == "" {
			return "错误: 必须提供 url 参数", true
		}

		var maxVisits int64 = -1
		if mv, ok := args["maxVisits"].(float64); ok && int64(mv) > 0 {
			maxVisits = int64(mv)
		}

		var expiresAt *time.Time
		if days, ok := args["expireDays"].(float64); ok && days > 0 {
			t := time.Now().Add(time.Duration(days*24) * time.Hour)
			expiresAt = &t
		}

		pwd := getArgString(args, "password")
		input := links.CreateRequest{
			Title:          getArgString(args, "title"),
			Code:           getArgString(args, "code"),
			AccessPassword: pwd,
			ExpiresAt:      expiresAt,
			Target: links.TargetInput{
				Type: "url",
				URL:  rawURL,
			},
			RuleSource: links.RuleSourceInput{
				Type: "custom",
				Rules: &links.RulesInput{
					MaxVisits:       &maxVisits,
					RequirePassword: pwd != "",
				},
			},
		}

		res, err := deps.Links.Create(ctx, mCtx.actor, input, mCtx.linkMeta)
		if err != nil {
			return fmt.Sprintf("创建短链失败: %v", err), true
		}

		expireDesc := "不过期"
		if res.ExpiresAt != nil {
			expireDesc = res.ExpiresAt.Format("2006-01-02 15:04:05")
		}
		visitsDesc := "不限"
		if res.Rules.MaxVisits > 0 {
			visitsDesc = fmt.Sprintf("最多 %d 次", res.Rules.MaxVisits)
		}

		return fmt.Sprintf("短链创建成功！\n- 短链地址：%s\n- 短码：/r/%s\n- 目标地址：%s\n- 有效期：%s\n- 访问上限：%s\n- 口令保护：%s",
			res.URL, res.Code, res.Target.URL, expireDesc, visitsDesc, formatPasswordDesc(pwd)), false

	case "create_burn_link":
		rawURL := getArgString(args, "url")
		if rawURL == "" {
			return "错误: 必须提供 url 参数", true
		}

		var maxVisits int64 = 1
		expiresAt := time.Now().Add(24 * time.Hour)
		title := getArgString(args, "title")
		if title == "" {
			title = "阅后即焚安全分享"
		}
		pwd := getArgString(args, "password")

		input := links.CreateRequest{
			Title:          title,
			AccessPassword: pwd,
			ExpiresAt:      &expiresAt,
			Target: links.TargetInput{
				Type: "url",
				URL:  rawURL,
			},
			RuleSource: links.RuleSourceInput{
				Type: "custom",
				Rules: &links.RulesInput{
					MaxVisits:       &maxVisits,
					RequirePassword: pwd != "",
				},
			},
		}

		res, err := deps.Links.Create(ctx, mCtx.actor, input, mCtx.linkMeta)
		if err != nil {
			return fmt.Sprintf("创建阅后即焚短链失败: %v", err), true
		}

		return fmt.Sprintf("【阅后即焚】短链已创建成功！\n- 短链地址：%s\n- 短码：/r/%s\n- 目标地址：%s\n- 特性：仅限访问 1 次（被打开后立即永久销毁）\n- 有效期：24 小时自动到期\n- 口令保护：%s",
			res.URL, res.Code, res.Target.URL, formatPasswordDesc(pwd)), false

	case "upload_file_and_share":
		fileName := getArgString(args, "fileName")
		if fileName == "" {
			return "错误: 必须提供 fileName 参数（如 report.pdf）", true
		}
		b64Content := getArgString(args, "fileContentBase64")
		if b64Content == "" {
			return "错误: 必须提供 fileContentBase64 参数", true
		}
		if len(b64Content) > 10<<20 {
			return "错误: 单次 MCP 上传仅支持不超过 7 MiB 的小文件，大文件请使用分片上传工具", true
		}

		// 兼容标准 Base64 及带有 data:image/png;base64, 前缀的数据
		if idx := strings.Index(b64Content, ";base64,"); idx != -1 {
			b64Content = b64Content[idx+8:]
		}
		fileBytes, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64Content))
		if err != nil {
			fileBytes, err = base64.RawStdEncoding.DecodeString(strings.TrimSpace(b64Content))
			if err != nil {
				return "错误: Base64 数据解码失败，请确认格式无误", true
			}
		}

		fileHeader, err := bytesToMultipartFileHeader(fileBytes, fileName)
		if err != nil {
			return fmt.Sprintf("构造上传文件头失败: %v", err), true
		}

		bizID := mCtx.assetActor.BusinessID
		if bizID == "" {
			bizID = "default"
		}

		assetRes, err := deps.Assets.Upload(ctx, mCtx.assetActor, assets.UploadInput{
			BusinessID: bizID,
			Path:       "agent-uploads",
			File:       fileHeader,
		}, mCtx.assetMeta)
		if err != nil {
			return fmt.Sprintf("素材上传失败: %v", err), true
		}

		var maxVisits int64 = -1
		if mv, ok := args["maxVisits"].(float64); ok && int64(mv) > 0 {
			maxVisits = int64(mv)
		}

		title := getArgString(args, "title")
		if title == "" {
			title = fileName
		}

		linkInput := links.CreateRequest{
			Title: title,
			Target: links.TargetInput{
				Type:    "minio_asset",
				AssetID: assetRes.AssetID,
			},
			RuleSource: links.RuleSourceInput{
				Type: "custom",
				Rules: &links.RulesInput{
					MaxVisits: &maxVisits,
				},
			},
		}

		linkRes, err := deps.Links.Create(ctx, mCtx.actor, linkInput, mCtx.linkMeta)
		if err != nil {
			return fmt.Sprintf("素材已上传(ID: %s)，但创建直连短链失败: %v", assetRes.AssetID, err), true
		}

		return fmt.Sprintf("文件已成功上传至素材库并生成下载短链！\n- 文件名：%s (大小: %d 字节)\n- 短链直链：%s\n- 短码：/r/%s\n- 素材 ID：%s\n- 存储业务桶：%s",
			fileName, len(fileBytes), linkRes.URL, linkRes.Code, assetRes.AssetID, assetRes.BusinessID), false

	case "begin_asset_upload":
		if err := requireMCPAssetUpload(ctx, mCtx, deps); err != nil {
			return err.Error(), true
		}
		businessID := mCtx.assetActor.BusinessID
		if businessID == "" {
			businessID = getArgString(args, "businessId")
		}
		path := getArgString(args, "path")
		if path == "" {
			path = "agent-uploads"
		}
		result, err := deps.Assets.BeginMultipartUpload(ctx, mCtx.assetActor, assets.BeginUploadInput{BusinessID: businessID, Path: path, FileName: getArgString(args, "fileName"), ContentType: getArgString(args, "contentType"), SizeBytes: int64(getArgNumber(args, "sizeBytes")), IdempotencyKey: getArgString(args, "idempotencyKey")})
		if err != nil {
			return fmt.Sprintf("创建上传会话失败: %v", err), true
		}
		return marshalMCPResult(result)

	case "upload_asset_part":
		if err := requireMCPAssetUpload(ctx, mCtx, deps); err != nil {
			return err.Error(), true
		}
		raw := strings.TrimSpace(getArgString(args, "contentBase64"))
		if idx := strings.Index(raw, ";base64,"); idx >= 0 {
			raw = raw[idx+8:]
		}
		decodedSize := int64(base64.StdEncoding.DecodedLen(len(raw)))
		if strings.HasSuffix(raw, "==") {
			decodedSize -= 2
		} else if strings.HasSuffix(raw, "=") {
			decodedSize--
		}
		if decodedSize <= 0 || decodedSize > 32*1024*1024 {
			return "错误: 单个分片内容必须大于 0 且不超过 32 MiB", true
		}
		reader := base64.NewDecoder(base64.StdEncoding, strings.NewReader(raw))
		result, err := deps.Assets.UploadMultipartPart(ctx, mCtx.assetActor, assets.UploadPartInput{SessionID: getArgString(args, "uploadSessionId"), PartNumber: int(getArgNumber(args, "partNumber")), Reader: reader, ContentLength: decodedSize})
		if err != nil {
			return fmt.Sprintf("上传分片失败: %v", err), true
		}
		return marshalMCPResult(result)

	case "get_asset_upload_status":
		if err := requireMCPAssetUpload(ctx, mCtx, deps); err != nil {
			return err.Error(), true
		}
		result, err := deps.Assets.GetMultipartUpload(ctx, mCtx.assetActor, getArgString(args, "uploadSessionId"))
		if err != nil {
			return fmt.Sprintf("查询上传状态失败: %v", err), true
		}
		return marshalMCPResult(result)

	case "complete_asset_upload":
		if err := requireMCPAssetUpload(ctx, mCtx, deps); err != nil {
			return err.Error(), true
		}
		result, err := deps.Assets.CompleteMultipartUpload(ctx, mCtx.assetActor, getArgString(args, "uploadSessionId"), mCtx.assetMeta)
		if err != nil {
			return fmt.Sprintf("完成上传失败: %v", err), true
		}
		return marshalMCPResult(result)

	case "cancel_asset_upload":
		if err := requireMCPAssetUpload(ctx, mCtx, deps); err != nil {
			return err.Error(), true
		}
		result, err := deps.Assets.CancelMultipartUpload(ctx, mCtx.assetActor, getArgString(args, "uploadSessionId"))
		if err != nil {
			return fmt.Sprintf("取消上传失败: %v", err), true
		}
		return marshalMCPResult(result)

	case "get_link_info":
		code := cleanCodeParam(getArgString(args, "code"))
		if code == "" {
			return "错误: 必须提供 code 参数", true
		}

		list, err := deps.Links.List(ctx, mCtx.actor, 1, 1, map[string]string{"code": code})
		if err != nil || len(list.Items) == 0 {
			// 尝试以 linkId 查询
			dto, getErr := deps.Links.Get(ctx, mCtx.actor, code)
			if getErr != nil {
				return fmt.Sprintf("未查询到短码为「%s」的短链记录", code), true
			}
			return formatLinkDetail(linkDTOToListItem(dto)), false
		}

		return formatLinkDetail(list.Items[0]), false

	case "list_my_links":
		pageSize := 10
		if ps, ok := args["pageSize"].(float64); ok && int(ps) > 0 {
			pageSize = int(ps)
			if pageSize > 50 {
				pageSize = 50
			}
		}

		list, err := deps.Links.List(ctx, mCtx.actor, 1, pageSize, map[string]string{})
		if err != nil {
			return fmt.Sprintf("查询短链列表失败: %v", err), true
		}

		if len(list.Items) == 0 {
			return "当前凭证下尚未创建任何短链。", false
		}

		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("最新短链列表（共 %d 条）：\n", list.Pagination.Total))
		for i, it := range list.Items {
			expire := "不过期"
			if it.ExpiresAt != nil {
				expire = it.ExpiresAt.Format("01-02 15:04")
			}
			sb.WriteString(fmt.Sprintf("%d. /r/%s -> %s (状态: %s, 访问: %d/%d, 到期: %s)\n",
				i+1, it.Code, it.URL, it.Availability, it.VisitsUsed, it.MaxVisits, expire))
		}
		return sb.String(), false

	default:
		return fmt.Sprintf("未知工具: %s", name), true
	}
}

func getArgString(args map[string]any, key string) string {
	if val, ok := args[key].(string); ok {
		return strings.TrimSpace(val)
	}
	return ""
}

func getArgNumber(args map[string]any, key string) float64 {
	if value, ok := args[key].(float64); ok {
		return value
	}
	return 0
}

func marshalMCPResult(value any) (string, bool) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("序列化上传结果失败: %v", err), true
	}
	return string(encoded), false
}

func requireMCPAssetUpload(ctx context.Context, mCtx mcpContext, deps Dependencies) error {
	if mCtx.assetActor.APIKeyID != nil {
		if !mCtx.apiKeyScopes["assets.upload"] {
			return fmt.Errorf("当前 API Key 没有 assets.upload 权限")
		}
		return nil
	}
	if mCtx.assetActor.Role == "admin" {
		return nil
	}
	var allowed bool
	if err := deps.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM user_permissions WHERE user_id=$1 AND permission_code='assets.upload' AND granted_at IS NOT NULL AND revoked_at IS NULL)`, mCtx.assetActor.UserID).Scan(&allowed); err != nil {
		return fmt.Errorf("检查素材上传权限失败")
	}
	if !allowed {
		return fmt.Errorf("当前账号没有素材上传权限")
	}
	return nil
}

func cleanCodeParam(raw string) string {
	raw = strings.TrimSpace(raw)
	if idx := strings.LastIndex(raw, "/r/"); idx != -1 {
		return raw[idx+3:]
	}
	return raw
}

func formatPasswordDesc(pwd string) string {
	if pwd == "" {
		return "无（公开访问）"
	}
	return "已设置口令"
}

func linkDTOToListItem(dto links.LinkDTO) links.ListItem {
	return links.ListItem{
		LinkID:         dto.LinkID,
		Title:          dto.Title,
		Tags:           dto.Tags,
		Code:           dto.Code,
		URL:            dto.URL,
		TargetType:     dto.Target.Type,
		TargetURL:      dto.Target.URL,
		AssetID:        dto.Target.AssetID,
		AssetSizeBytes: dto.Target.AssetSizeBytes,
		Status:         dto.Status,
		Availability:   dto.Availability,
		ExpiresAt:      dto.ExpiresAt,
		VisitsUsed:     dto.VisitsUsed,
		MaxVisits:      dto.Rules.MaxVisits,
		CreatedAt:      dto.CreatedAt,
	}
}

func formatLinkDetail(item links.ListItem) string {
	expire := "不过期"
	if item.ExpiresAt != nil {
		expire = item.ExpiresAt.Format("2006-01-02 15:04:05")
	}
	maxV := "不限"
	if item.MaxVisits > 0 {
		maxV = fmt.Sprintf("%d", item.MaxVisits)
	}
	targetDesc := item.TargetURL
	if item.TargetType == "minio_asset" || item.TargetType == "asset" {
		targetDesc = fmt.Sprintf("云端素材 (ID: %s)", item.AssetID)
	}
	return fmt.Sprintf("短链详情：\n- 完整地址：%s\n- 短码：/r/%s\n- 目标类型：%s\n- 目标地址：%s\n- 可用状态：%s (生效状态: %s)\n- 访问次数：%d / %s\n- 到期时间：%s\n- 创建时间：%s",
		item.URL, item.Code, item.TargetType, targetDesc, item.Availability, item.Status, item.VisitsUsed, maxV, expire, item.CreatedAt.Format("2006-01-02 15:04:05"))
}

func bytesToMultipartFileHeader(data []byte, filename string) (*multipart.FileHeader, error) {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	part, err := w.CreateFormFile("file", filename)
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}

	r := multipart.NewReader(&b, w.Boundary())
	form, err := r.ReadForm(int64(len(data)) + 4096)
	if err != nil {
		return nil, err
	}
	files := form.File["file"]
	if len(files) == 0 {
		return nil, fmt.Errorf("构造文件头失败")
	}
	return files[0], nil
}
