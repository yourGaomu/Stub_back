package httpapi

import (
	"html/template"
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"
	"github.com/yourGaomu/short_links/back/internal/modules/links"
)

var publicPageTemplate = template.Must(template.New("public-page").Parse(`<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}} · Stub 短链</title>
<style>
:root{--paper:#f2eee4;--paper-2:#e8e2d4;--paper-3:#fbf9f4;--ink:#17150f;--ink-2:#4a463c;--ink-3:#8a8475;--line:#c6bca5;--signal:#ff4d1a;--danger:#c2361b}
*{box-sizing:border-box}
body{margin:0;min-height:100vh;background:var(--paper);color:var(--ink);font-family:"PingFang SC","Microsoft YaHei",system-ui,-apple-system,sans-serif;display:flex;flex-direction:column;justify-content:space-between;padding:24px}
.top{display:flex;align-items:center;justify-content:space-between;max-width:460px;width:100%;margin:0 auto}
.brand{display:inline-flex;align-items:center;gap:8px;font-weight:800;font-size:20px;letter-spacing:-0.02em;color:var(--ink);text-decoration:none}
.code-pill{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:12px;color:var(--ink-2);background:var(--paper-2);border:1px solid var(--line);border-radius:4px;padding:3px 8px}
.ticket{position:relative;max-width:460px;width:100%;margin:auto;background:var(--paper-3);border:1.5px solid var(--ink);border-radius:6px;box-shadow:0 18px 40px -22px rgba(23,21,15,.28);overflow:hidden}
.ticket::before,.ticket::after{content:"";position:absolute;top:74px;width:18px;height:18px;border-radius:50%;background:var(--paper);border:1.5px solid var(--ink);z-index:2}
.ticket::before{left:-10px}
.ticket::after{right:-10px}
.ticket-head{display:flex;align-items:center;justify-content:space-between;padding:16px 24px;background:var(--paper-2);border-bottom:1.5px dashed var(--line);font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:11px;color:var(--ink-2);letter-spacing:.06em}
.badge{display:inline-flex;align-items:center;gap:6px;padding:2px 8px;border-radius:999px;font-weight:600;border:1px solid var(--line);background:var(--paper-3);color:var(--ink)}
.badge.challenge{border-color:#e89a7e;background:#fde6dd;color:#a82a06}
.badge.denied{border-color:#e2a295;background:#fbe7e3;color:var(--danger)}
.ticket-body{padding:28px 26px 26px}
h1{margin:0 0 10px;font-size:24px;font-weight:900;letter-spacing:-0.02em}
p{margin:0;line-height:1.65;font-size:14px;color:var(--ink-2)}
form{margin-top:22px;padding-top:18px;border-top:1px solid #d8d0bd}
label{display:flex;justify-content:space-between;font-size:12px;font-weight:600;color:var(--ink-2);margin-bottom:8px}
label span{font-weight:400;color:var(--ink-3)}
input{width:100%;height:42px;padding:0 12px;border:1px solid var(--line);border-radius:4px;background:#fff;color:var(--ink);font:inherit;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;outline:none}
input:focus{border-color:var(--ink);box-shadow:0 0 0 3px rgba(255,77,26,.15)}
button{margin-top:14px;width:100%;height:42px;border:1px solid var(--signal);border-radius:4px;background:var(--signal);color:#fff;font:inherit;font-size:14px;font-weight:600;cursor:pointer}
button:hover{background:#e83e0e}
.foot{max-width:460px;width:100%;margin:0 auto;text-align:center;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:11px;color:var(--ink-3)}
</style>
</head>
<body>
<header class="top">
  <span class="brand">
    <svg width="24" height="24" viewBox="0 0 64 64" fill="none" xmlns="http://www.w3.org/2000/svg">
      <path d="M12 6H52C55.3 6 58 8.7 58 12V25C54.1 25 51 28.1 51 32C51 35.9 54.1 39 58 39V52C58 55.3 55.3 58 52 58H12C8.7 58 6 55.3 6 52V39C9.9 39 13 35.9 13 32C13 28.1 9.9 25 6 25V12C6 8.7 8.7 6 12 6Z" fill="#17150F"/>
      <path d="M29.5 22.5C29.5 20.5 27.8 19 25 19C22.2 19 20.5 20.4 20.5 22.3C20.5 24.4 22.5 25.2 25.6 26.1C29.8 27.3 32.5 29.1 32.5 33.2C32.5 37.3 29.2 40 24.6 40C19.8 40 17 37.4 17 33.8H21.2C21.2 35.6 22.6 36.6 24.8 36.6C27.1 36.6 28.4 35.4 28.4 33.6C28.4 31.6 26.7 30.8 23.5 29.8C19.6 28.6 16.8 26.9 16.8 22.6C16.8 18.6 20.1 15.8 24.9 15.8C29.6 15.8 32.8 18.3 33 22.5H29.5Z" fill="#F2EEE4"/>
      <path d="M43.5 16L34.5 48H39L48 16H43.5Z" fill="#FF4D1A"/>
    </svg>
    <span>Stub</span>
  </span>
  {{if .Code}}<span class="code-pill">/r/{{.Code}}</span>{{end}}
</header>
<main class="ticket">
  <div class="ticket-head">
    <span>STUB ACCESS GATE</span>
    <span class="badge {{if .Challenge}}challenge{{else}}denied{{end}}">HTTP {{.StatusCode}} · {{if .Challenge}}CHALLENGE{{else}}NOTICE{{end}}</span>
  </div>
  <div class="ticket-body">
    <h1>{{.Title}}</h1>
    <p>{{.Message}}</p>
    {{if .Challenge}}
    <form method="post" action="{{.Action}}">
      <label for="password"><b>访问口令</b><span>验证通过后自动跳转</span></label>
      <input id="password" name="password" type="password" placeholder="请输入分享者提供的访问口令" autocomplete="current-password" maxlength="1024" required autofocus>
      <button type="submit">验证并继续访问 →</button>
    </form>
    {{end}}
  </div>
</main>
<footer class="foot">Stub 受控短链与素材分发服务</footer>
</body>
</html>`))

func registerLinkRoutes(router *gin.Engine, deps Dependencies) {
	// 1. 管理 API 统一认证；每个业务路由在 Handler 前执行对应功能权限检查。
	service := deps.Links
	api := router.Group("/api/v1")
	api.Use(authenticate(deps.Auth))
	api.Use(requireAPIKeyBusinessAccess(deps.DB))
	api.POST("/links", requireCreateLinkPermission(deps.DB), func(c *gin.Context) {
		var input links.CreateRequest
		if !decodeJSON(c, &input) {
			return
		}
		result, err := service.Create(c.Request.Context(), linkActor(c), input, linkMeta(c))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusCreated, result)
	})
	api.GET("/links", requirePermission(deps.DB, "links.read"), func(c *gin.Context) {
		page, pageSize, ok := parsePagination(c)
		if !ok {
			return
		}
		filters := map[string]string{}
		for _, key := range []string{"workspaceId", "status", "availability", "targetType", "code", "keyword", "assetId", "createdFrom", "createdTo"} {
			filters[key] = c.Query(key)
		}
		result, err := service.List(c.Request.Context(), linkActor(c), page, pageSize, filters)
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	api.GET("/links/:linkId", requirePermission(deps.DB, "links.read"), func(c *gin.Context) {
		result, err := service.Get(c.Request.Context(), linkActor(c), c.Param("linkId"))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	api.PATCH("/links/:linkId/metadata", requirePermission(deps.DB, "links.manage"), func(c *gin.Context) {
		var input links.UpdateMetadataRequest
		if !decodeJSON(c, &input) {
			return
		}
		result, err := service.UpdateMetadata(c.Request.Context(), linkActor(c), c.Param("linkId"), input, linkMeta(c))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	api.PATCH("/links/:linkId/rules", requirePermission(deps.DB, "links.manage"), func(c *gin.Context) {
		var input links.UpdateRulesRequest
		if !decodeJSON(c, &input) {
			return
		}
		result, err := service.UpdateRules(c.Request.Context(), linkActor(c), c.Param("linkId"), input, linkMeta(c))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	api.POST("/links/:linkId/renew", requirePermission(deps.DB, "links.manage"), func(c *gin.Context) {
		var input links.RenewRequest
		if !decodeJSON(c, &input) {
			return
		}
		result, err := service.Renew(c.Request.Context(), linkActor(c), c.Param("linkId"), input, linkMeta(c))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	api.POST("/links/:linkId/rotate-code", requirePermission(deps.DB, "links.manage"), func(c *gin.Context) {
		var input links.RotateRequest
		if !decodeJSON(c, &input) {
			return
		}
		result, err := service.Rotate(c.Request.Context(), linkActor(c), c.Param("linkId"), input, linkMeta(c))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	api.POST("/links/:linkId/reset-visits", requirePermission(deps.DB, "links.reset_visits"), func(c *gin.Context) {
		result, err := service.ResetVisits(c.Request.Context(), linkActor(c), c.Param("linkId"), linkMeta(c))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	api.POST("/links/:linkId/disable", requirePermission(deps.DB, "links.manage"), linkStatusHandler(service, "disabled"))
	api.POST("/links/:linkId/enable", requirePermission(deps.DB, "links.manage"), linkStatusHandler(service, "enabled"))

	// 2. 公开解析入口不要求用户 Token；HTML 页面与 JSON 管理接口严格分离。
	router.GET("/r/:code", func(c *gin.Context) { publicLinkHandler(c, service, false) })
	router.POST("/r/:code/password", func(c *gin.Context) { publicLinkHandler(c, service, true) })
}

func linkStatusHandler(service *links.Service, status string) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := service.SetStatus(c.Request.Context(), linkActor(c), c.Param("linkId"), status, linkMeta(c))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func linkActor(c *gin.Context) links.Actor {
	identity := currentAuth(c)
	workspaceID, _ := parsePublicID("ws", identity.User.DefaultWorkspaceID)
	return links.Actor{UserID: identity.User.ID, WorkspaceID: workspaceID, Role: identity.User.Role, BusinessID: apiKeyBusinessID(c), APIKeyID: apiKeyID(c)}
}

func linkMeta(c *gin.Context) links.RequestMeta {
	return links.RequestMeta{SourceIP: c.ClientIP(), UserAgent: truncateLogValue(c.Request.UserAgent(), 1024), RequestID: requestID(c), AuditDetails: apiKeyAuditDetails(c)}
}

func publicLinkHandler(c *gin.Context, service *links.Service, submit bool) {
	// 1. 访问入口只接受服务端解析的短码与来源信息，不读任何目标地址参数。
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	// 1. 口令表单提交可能收到跨域 HTTPS 目标的 302；允许本站和 HTTPS，仍禁止明文 HTTP。
	c.Header("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self' https:; base-uri 'none'; frame-ancestors 'none'")
	password := ""
	if submit {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
		if err := c.Request.ParseForm(); err != nil {
			writePublicPage(c, links.PublicResult{StatusCode: http.StatusBadRequest, PageTitle: "请求无效", PageText: "请重新打开短链页面后重试。"}, c.Param("code"))
			return
		}
		password = c.Request.PostForm.Get("password")
		if len(password) > 1024 {
			writePublicPage(c, links.PublicResult{StatusCode: http.StatusBadRequest, PageTitle: "请求无效", PageText: "访问口令格式无效。"}, c.Param("code"))
			return
		}
	}
	result, err := service.Access(c.Request.Context(), c.Param("code"), linkMeta(c), password, submit)
	if err != nil {
		writePublicPage(c, links.PublicResult{StatusCode: http.StatusServiceUnavailable, PageTitle: "服务暂不可用", PageText: "短链服务暂时不可用，请稍后重试。"}, c.Param("code"))
		return
	}
	writePublicPage(c, result, c.Param("code"))
}

func writePublicPage(c *gin.Context, result links.PublicResult, code string) {
	if result.StatusCode == http.StatusFound && result.Location != "" {
		c.Redirect(http.StatusFound, result.Location)
		return
	}
	action := "/r/" + url.PathEscape(code) + "/password"
	c.Status(result.StatusCode)
	c.Header("Content-Type", "text/html; charset=utf-8")
	_ = publicPageTemplate.Execute(c.Writer, struct {
		Title, Message, Action, Code string
		StatusCode                   int
		Challenge                    bool
	}{result.PageTitle, result.PageText, action, code, result.StatusCode, result.Challenge})
}
