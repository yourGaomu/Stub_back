package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/yourGaomu/short_links/back/internal/config"
	"github.com/yourGaomu/short_links/back/internal/modules/assets"
	"github.com/yourGaomu/short_links/back/internal/modules/auth"
	"github.com/yourGaomu/short_links/back/internal/modules/links"
)

type mcpTestStore struct {
	signedAt time.Time
	ttl      time.Duration
	put      []byte
	parts    []minio.ObjectPart
}

func (s *mcpTestStore) PresignedGet(_ context.Context, bucket, key string, ttl time.Duration, contentType, name string) (string, error) {
	s.signedAt, s.ttl = time.Now().UTC(), ttl
	if bucket != "private-bucket" || key != "internal/object" || contentType != "text/plain" || name != "report.txt" {
		return "", errors.New("unexpected signing arguments")
	}
	return "https://files.example.test/download?signature=test", nil
}
func (s *mcpTestStore) ObjectExists(context.Context, string, string) (bool, error) {
	return false, errors.New("unexpected ObjectExists")
}
func (s *mcpTestStore) ObjectSize(context.Context, string, string) (int64, error) {
	return 0, errors.New("not merged")
}
func (s *mcpTestStore) Put(_ context.Context, _, _, _ string, body io.Reader, size int64) error {
	var err error
	s.put, err = io.ReadAll(body)
	if err == nil && int64(len(s.put)) != size {
		return errors.New("incorrect upload length")
	}
	return err
}
func (s *mcpTestStore) Remove(context.Context, string, string) error {
	return errors.New("unexpected Remove")
}
func (s *mcpTestStore) BeginMultipart(context.Context, string, string, string) (string, error) {
	return "remote-upload", nil
}
func (s *mcpTestStore) PutMultipartPart(_ context.Context, _, _, _ string, part int, body io.Reader, size int64) (string, error) {
	data, err := io.ReadAll(body)
	if err != nil || int64(len(data)) != size {
		return "", errors.New("incorrect part bytes")
	}
	s.put = data
	s.parts = []minio.ObjectPart{{PartNumber: part, Size: size, ETag: "part-etag"}}
	return "part-etag", nil
}
func (s *mcpTestStore) ListMultipartParts(context.Context, string, string, string) ([]minio.ObjectPart, error) {
	return s.parts, nil
}
func (s *mcpTestStore) CompleteMultipart(context.Context, string, string, string, []minio.CompletePart, string) error {
	return errors.New("unexpected CompleteMultipart")
}
func (s *mcpTestStore) AbortMultipart(context.Context, string, string, string) error {
	return nil
}
func (s *mcpTestStore) AbortMultipartForObject(context.Context, string, string) error {
	return nil
}

type mcpFixture struct {
	deps    Dependencies
	mock    sqlmock.Sqlmock
	ctx     mcpContext
	store   *mcpTestStore
	assetID uuid.UUID
	created time.Time
}

func newMCPFixture(t *testing.T) *mcpFixture {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
		db.Close()
	})
	user, workspace := uuid.New(), uuid.New()
	store := &mcpTestStore{}
	cfg := config.Config{PublicBaseURL: "https://short.example.test"}
	return &mcpFixture{
		deps: Dependencies{DB: db, Assets: assets.NewService(db, store), Links: links.NewService(db, cfg, nil), Auth: auth.NewService(db, nil, cfg)},
		mock: mock, store: store, assetID: uuid.New(), created: time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC),
		ctx: mcpContext{actor: links.Actor{UserID: user, WorkspaceID: workspace, Role: "admin"}, assetActor: assets.Actor{UserID: user, WorkspaceID: workspace, Role: "admin"}},
	}
}

func (f *mcpFixture) asset() assets.AssetDTO {
	return assets.AssetDTO{AssetID: "ast_" + f.assetID.String(), BusinessID: "canvas", Path: "agent-uploads", FileName: "report.txt", ContentType: "text/plain", SizeBytes: 7, ChecksumSHA256: strings.Repeat("a", 64), CreatedAt: f.created}
}

func (f *mcpFixture) expectAsset(business string, workspace uuid.UUID, deleted any) {
	f.mock.ExpectQuery(`SELECT asset_id,workspace_id,owner_user_id,business_id,.* FROM assets WHERE asset_id=\$1 FOR UPDATE`).WithArgs(f.assetID).
		WillReturnRows(sqlmock.NewRows([]string{"asset", "workspace", "owner", "business", "bucket", "key", "path", "name", "type", "size", "checksum", "deleted", "purged", "created"}).
			AddRow(f.assetID, workspace, f.ctx.assetActor.UserID, business, "private-bucket", "internal/object", "agent-uploads", "report.txt", "text/plain", 7, strings.Repeat("a", 64), deleted, nil, f.created))
}

func (f *mcpFixture) expectPermission(permission string, allowed bool) {
	f.mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM user_permissions WHERE user_id=\$1 AND permission_code=\$2`).
		WithArgs(f.ctx.assetActor.UserID, permission).WillReturnRows(sqlmock.NewRows([]string{"allowed"}).AddRow(allowed))
}

func mcpCall(t *testing.T, f *mcpFixture, name string, args map[string]any) mcpToolResult {
	t.Helper()
	params, err := json.Marshal(map[string]any{"name": name, "arguments": args})
	if err != nil {
		t.Fatal(err)
	}
	response := handleMCPMethod(context.Background(), jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: params}, f.ctx, f.deps)
	if response.Error != nil {
		t.Fatalf("RPC error: %+v", response.Error)
	}
	result, ok := response.Result.(mcpToolResult)
	if !ok {
		t.Fatalf("unexpected result: %#v", response.Result)
	}
	return result
}

func mcpStructured(t *testing.T, result mcpToolResult) map[string]any {
	t.Helper()
	if result.IsError || result.StructuredContent == nil {
		t.Fatalf("tool failed or omitted structuredContent: %+v", result)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 || result.Content[0].Type != "text" || result.Content[0].Text != string(encoded) {
		t.Fatalf("text must be identical JSON: %+v, structured=%s", result.Content, encoded)
	}
	var object map[string]any
	if err := json.Unmarshal(encoded, &object); err != nil {
		t.Fatal(err)
	}
	return object
}

func assertMCPKeys(t *testing.T, object map[string]any, keys ...string) {
	t.Helper()
	if len(object) != len(keys) {
		t.Fatalf("keys = %v, want exactly %v", reflect.ValueOf(object).MapKeys(), keys)
	}
	for _, key := range keys {
		if _, ok := object[key]; !ok {
			t.Fatalf("missing %s in %v", key, object)
		}
	}
}

func TestMCPAssetCatalogSchemas(t *testing.T) {
	response := handleMCPMethod(context.Background(), jsonRPCRequest{Method: "tools/list"}, mcpContext{}, Dependencies{})
	tools := response.Result.(map[string]any)["tools"].([]mcpToolDefinition)
	byName := map[string]mcpToolDefinition{}
	for _, tool := range tools {
		if _, exists := byName[tool.Name]; exists {
			t.Fatalf("duplicate tool %s", tool.Name)
		}
		byName[tool.Name] = tool
	}
	for _, name := range []string{"create_url_shortlink", "create_burn_link", "get_link_info", "list_my_links"} {
		if _, ok := byName[name]; !ok {
			t.Fatalf("existing tool missing: %s", name)
		}
	}
	for _, name := range []string{"get_asset_info", "get_asset_download_url", "create_asset_shortlink", "upload_file_and_share", "begin_asset_upload", "upload_asset_part", "get_asset_upload_status", "complete_asset_upload", "cancel_asset_upload"} {
		tool, ok := byName[name]
		if !ok || tool.InputSchema["additionalProperties"] != false || tool.OutputSchema["type"] != "object" || tool.OutputSchema["additionalProperties"] != false {
			t.Fatalf("incomplete schema for %s: %+v", name, tool)
		}
	}
	assertMCPKeys(t, byName["get_asset_info"].OutputSchema["properties"].(map[string]any), "assetId", "businessId", "fileName", "contentType", "sizeBytes", "checksumSha256", "createdAt")
	assertMCPKeys(t, byName["get_asset_download_url"].OutputSchema["properties"].(map[string]any), "asset", "downloadUrl", "expiresAt")
	assertMCPKeys(t, byName["create_asset_shortlink"].InputSchema["properties"].(map[string]any), "assetId", "title", "expiresAt", "maxVisits")
	if !reflect.DeepEqual(byName["create_asset_shortlink"].InputSchema["required"], []string{"assetId"}) || !reflect.DeepEqual(byName["upload_file_and_share"].OutputSchema, byName["create_asset_shortlink"].OutputSchema) {
		t.Fatal("share schemas differ or assetId is not required")
	}
	for _, name := range []string{"begin_asset_upload", "upload_asset_part", "get_asset_upload_status", "complete_asset_upload", "cancel_asset_upload"} {
		if !reflect.DeepEqual(byName[name].OutputSchema, mcpUploadSessionSchema()) {
			t.Fatalf("%s output must be direct UploadSessionDTO", name)
		}
	}
}

func TestMCPHTTPAssetStructuredResults(t *testing.T) {
	for _, name := range []string{"get_asset_info", "get_asset_download_url"} {
		t.Run(name, func(t *testing.T) {
			f := newMCPFixture(t)
			gin.SetMode(gin.TestMode)
			router := gin.New()
			registerMCPRoutes(router, f.deps)
			digest := sha256.Sum256([]byte("mcp-test-session"))
			f.mock.ExpectQuery(`SELECT u.user_id, u.default_workspace_id, s.session_id, .* FROM user_sessions s JOIN users u`).WithArgs(hex.EncodeToString(digest[:])).
				WillReturnRows(sqlmock.NewRows([]string{"user", "workspace", "session", "email", "role", "verified", "invited", "created"}).AddRow(f.ctx.actor.UserID, f.ctx.actor.WorkspaceID, uuid.New(), "admin@example.test", "admin", f.created, nil, f.created))
			f.expectAsset("canvas", f.ctx.assetActor.WorkspaceID, nil)
			if name == "get_asset_download_url" {
				f.expectAsset("canvas", f.ctx.assetActor.WorkspaceID, nil)
				f.mock.ExpectBegin()
				f.mock.ExpectExec(`INSERT INTO audit_logs`).WithArgs(f.ctx.actor.UserID, f.ctx.actor.WorkspaceID, "asset.content", f.assetID.String(), "192.0.2.1", "", sqlmock.AnyArg(), "").WillReturnResult(sqlmock.NewResult(0, 1))
				f.mock.ExpectCommit()
			}
			params, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/call", "params": map[string]any{"name": name, "arguments": map[string]any{"assetId": f.asset().AssetID}}})
			request := httptest.NewRequest(http.MethodPost, "/api/mcp", bytes.NewReader(params))
			request.Header.Set("Authorization", "Bearer mcp-test-session")
			response := httptest.NewRecorder()
			before := time.Now().UTC().Truncate(time.Second)
			router.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("HTTP %d: %s", response.Code, response.Body.String())
			}
			var rpc struct {
				ID     int           `json:"id"`
				Result mcpToolResult `json:"result"`
				Error  *jsonRPCError `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &rpc); err != nil || rpc.Error != nil || rpc.ID != 7 {
				t.Fatalf("invalid RPC response: %s; error=%v", response.Body.String(), err)
			}
			// Compare text and structured objects after JSON decoding (map key order differs).
			var text any
			if rpc.Result.IsError || len(rpc.Result.Content) != 1 || json.Unmarshal([]byte(rpc.Result.Content[0].Text), &text) != nil || !reflect.DeepEqual(text, rpc.Result.StructuredContent) {
				t.Fatalf("structured result/text mismatch: %s", response.Body.String())
			}
			object := rpc.Result.StructuredContent.(map[string]any)
			asset := object
			if name == "get_asset_download_url" {
				assertMCPKeys(t, object, "asset", "downloadUrl", "expiresAt")
				asset = object["asset"].(map[string]any)
				expires, err := time.Parse(time.RFC3339, object["expiresAt"].(string))
				if err != nil || expires.Before(before.Add(15*time.Minute)) || expires.After(f.store.signedAt.Add(15*time.Minute)) || f.store.ttl != 15*time.Minute || object["downloadUrl"] != "https://files.example.test/download?signature=test" {
					t.Fatalf("invalid download URL or unsafe expiry: %v, signed=%v, TTL=%v, error=%v", object, f.store.signedAt, f.store.ttl, err)
				}
			}
			assertMCPKeys(t, asset, "assetId", "businessId", "fileName", "contentType", "sizeBytes", "checksumSha256", "createdAt")
			if asset["assetId"] != f.asset().AssetID || asset["businessId"] != "canvas" || asset["sizeBytes"] != float64(7) || asset["createdAt"] != f.created.Format(time.RFC3339) {
				t.Fatalf("incorrect asset metadata: %v", asset)
			}
		})
	}
}

func (f *mcpFixture) expectCreateLink(maxVisits int64, expires *time.Time, title string, assetMatcher sqlmock.Argument) {
	f.mock.ExpectBegin()
	f.mock.ExpectQuery(`SELECT role, url_link_quota, minio_asset_link_quota FROM users WHERE user_id=\$1 FOR (NO KEY )?UPDATE`).WithArgs(f.ctx.actor.UserID).
		WillReturnRows(sqlmock.NewRows([]string{"role", "url_quota", "asset_quota"}).AddRow("admin", 100, 100))
	f.mock.ExpectQuery(`SELECT a.workspace_id, a.size_bytes, a.business_id, .* FROM assets a JOIN users u`).WithArgs(assetMatcher, f.ctx.actor.UserID).
		WillReturnRows(sqlmock.NewRows([]string{"workspace", "size", "business", "allowed"}).AddRow(f.ctx.actor.WorkspaceID, 7, "canvas", true))
	f.mock.ExpectQuery(`SELECT workspace_id FROM assets WHERE asset_id=\$1`).WithArgs(assetMatcher).
		WillReturnRows(sqlmock.NewRows([]string{"workspace"}).AddRow(f.ctx.actor.WorkspaceID))
	args := []driver.Value{sqlmock.AnyArg(), f.ctx.actor.WorkspaceID, "canvas", title, "[]", "minio_asset", "", assetMatcher, expires, maxVisits, "{}", false, nil, nil, nil, nil, f.ctx.actor.UserID}
	f.mock.ExpectExec(`INSERT INTO short_links .*`).WithArgs(mcpLinkInsertArgs(args)...).
		WillReturnResult(sqlmock.NewResult(0, 1))
	f.mock.ExpectExec(`INSERT INTO short_link_codes`).WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	f.mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))
	f.mock.ExpectCommit()
}

// Optional link grouping/window columns are owned by the link service. Keep the
// MCP rule assertions exact while supporting a checkout with that feature applied.
func mcpLinkInsertArgs(args []driver.Value) []driver.Value {
	if _, ok := reflect.TypeOf(links.CreateRequest{}).FieldByName("GroupID"); ok {
		return append(args, nil, nil, nil)
	}
	return args
}

type mcpUUIDArg struct{ id uuid.UUID }

func (a mcpUUIDArg) Match(value driver.Value) bool { return value == a.id.String() }

func TestMCPCreateAssetShortlinkStructuredAndExactInt64(t *testing.T) {
	for _, maxVisits := range []int64{-1, 0, 9007199254740993, 9223372036854775807} {
		t.Run(json.Number(stringInt64(maxVisits)).String(), func(t *testing.T) {
			f := newMCPFixture(t)
			expires := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
			f.expectAsset("canvas", f.ctx.assetActor.WorkspaceID, nil)
			f.expectCreateLink(maxVisits, &expires, "Share", mcpUUIDArg{f.assetID})
			result := mcpCall(t, f, "create_asset_shortlink", map[string]any{"assetId": f.asset().AssetID, "title": "Share", "expiresAt": expires.Format(time.RFC3339), "maxVisits": maxVisits})
			object := mcpStructured(t, result)
			assertMCPKeys(t, object, "asset", "shortUrl", "code", "expiresAt", "maxVisits")
			share := result.StructuredContent.(mcpAssetShareDTO)
			if share.MaxVisits != maxVisits || share.Asset.AssetID != f.asset().AssetID || !share.ExpiresAt.Equal(expires) || share.Code == "" || !strings.HasSuffix(share.ShortURL, "/r/"+share.Code) {
				t.Fatalf("incorrect share result: %+v", share)
			}
		})
	}
}

func stringInt64(number int64) string {
	encoded, _ := json.Marshal(number)
	return string(encoded)
}

func TestMCPAssetReadRejectsDeletedAndOutOfScope(t *testing.T) {
	for _, name := range []string{"get_asset_info", "get_asset_download_url", "create_asset_shortlink"} {
		for _, reason := range []string{"deleted", "other_business", "other_workspace", "revoked_business", "missing"} {
			t.Run(name+"/"+reason, func(t *testing.T) {
				f := newMCPFixture(t)
				workspace := f.ctx.assetActor.WorkspaceID
				var deleted any
				if reason == "other_business" {
					f.ctx.assetActor.BusinessID = "other"
					f.ctx.actor.BusinessID = "other"
				}
				if reason == "other_workspace" || reason == "revoked_business" {
					f.ctx.assetActor.Role, f.ctx.actor.Role = "user", "user"
					f.expectPermission("assets.read", true)
					if name == "create_asset_shortlink" {
						f.expectPermission("links.create_asset", true)
					}
				}
				if reason == "other_workspace" {
					workspace = uuid.New()
				}
				if reason == "deleted" {
					deleted = time.Now()
				}
				if reason == "missing" {
					f.mock.ExpectQuery(`SELECT asset_id,workspace_id,owner_user_id,business_id,.* FROM assets`).WithArgs(f.assetID).WillReturnError(sql.ErrNoRows)
				} else {
					f.expectAsset("canvas", workspace, deleted)
				}
				if reason == "revoked_business" {
					f.mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM business_integrations bi JOIN user_business_access`).WithArgs("canvas", f.ctx.assetActor.UserID).
						WillReturnRows(sqlmock.NewRows([]string{"allowed"}).AddRow(false))
				}
				result := mcpCall(t, f, name, map[string]any{"assetId": f.asset().AssetID})
				if !result.IsError || result.StructuredContent != nil || !strings.Contains(result.Content[0].Text, "素材不存在或无权访问") {
					t.Fatalf("expected scope/deleted rejection: %+v", result)
				}
			})
		}
	}
}

func TestMCPAssetPermissions(t *testing.T) {
	cases := []struct {
		name        string
		args        map[string]any
		permissions []string
	}{
		{"get_asset_info", map[string]any{"assetId": "ast_" + uuid.NewString()}, []string{"assets.read"}},
		{"get_asset_download_url", map[string]any{"assetId": "ast_" + uuid.NewString()}, []string{"assets.read"}},
		{"create_asset_shortlink", map[string]any{"assetId": "ast_" + uuid.NewString()}, []string{"assets.read", "links.create_asset"}},
		{"upload_file_and_share", map[string]any{"fileName": "report.txt", "fileContentBase64": "YWI="}, []string{"assets.upload", "links.create_asset"}},
		{"begin_asset_upload", map[string]any{"fileName": "report.txt", "sizeBytes": 7}, []string{"assets.upload"}},
		{"upload_asset_part", map[string]any{"uploadSessionId": "upl_" + uuid.NewString(), "partNumber": 1, "contentBase64": "YWI="}, []string{"assets.upload"}},
		{"get_asset_upload_status", map[string]any{"uploadSessionId": "upl_" + uuid.NewString()}, []string{"assets.upload"}},
		{"complete_asset_upload", map[string]any{"uploadSessionId": "upl_" + uuid.NewString()}, []string{"assets.upload"}},
		{"cancel_asset_upload", map[string]any{"uploadSessionId": "upl_" + uuid.NewString()}, []string{"assets.upload"}},
	}
	for _, tc := range cases {
		for index, denied := range tc.permissions {
			for _, apiKey := range []bool{false, true} {
				t.Run(tc.name+"/"+denied+"/apiKey="+stringInt64(int64(map[bool]int{false: 0, true: 1}[apiKey])), func(t *testing.T) {
					f := newMCPFixture(t)
					if apiKey {
						key := uuid.New()
						f.ctx.assetActor.APIKeyID, f.ctx.actor.APIKeyID = &key, &key
						f.ctx.apiKeyScopes = map[string]bool{}
						for _, permission := range tc.permissions[:index] {
							f.ctx.apiKeyScopes[permission] = true
						}
					} else {
						f.ctx.assetActor.Role, f.ctx.actor.Role = "user", "user"
						for _, permission := range tc.permissions[:index] {
							f.expectPermission(permission, true)
						}
						f.expectPermission(denied, false)
					}
					result := mcpCall(t, f, tc.name, tc.args)
					if !result.IsError || result.StructuredContent != nil || !strings.Contains(result.Content[0].Text, denied) {
						t.Fatalf("missing permission was not rejected: %+v", result)
					}
				})
			}
		}
	}
	// A granted key scope cannot override a revoked user permission.
	f := newMCPFixture(t)
	key := uuid.New()
	f.ctx.assetActor.APIKeyID = &key
	f.ctx.assetActor.Role = "user"
	f.ctx.apiKeyScopes = map[string]bool{"assets.read": true}
	f.expectPermission("assets.read", false)
	if result := mcpCall(t, f, "get_asset_info", map[string]any{"assetId": f.asset().AssetID}); !result.IsError {
		t.Fatal("API Key bypassed current user permissions")
	}
}

func TestMCPAssetMalformedArguments(t *testing.T) {
	assetID, sessionID := "ast_"+uuid.NewString(), "upl_"+uuid.NewString()
	cases := []struct {
		name string
		args map[string]any
	}{
		{"get_asset_info", nil},
		{"get_asset_info", map[string]any{"assetId": nil}},
		{"get_asset_info", map[string]any{"assetId": 42}},
		{"get_asset_info", map[string]any{"assetId": "wrong"}},
		{"get_asset_download_url", map[string]any{"assetId": assetID, "businessId": "other"}},
		{"create_asset_shortlink", map[string]any{"assetId": assetID, "maxVisits": 1.5}},
		{"create_asset_shortlink", map[string]any{"assetId": assetID, "maxVisits": "1"}},
		{"create_asset_shortlink", map[string]any{"assetId": assetID, "maxVisits": -2}},
		{"create_asset_shortlink", map[string]any{"assetId": assetID, "maxVisits": json.Number("9223372036854775808")}},
		{"create_asset_shortlink", map[string]any{"assetId": assetID, "expiresAt": "tomorrow"}},
		{"create_asset_shortlink", map[string]any{"assetId": assetID, "expiresAt": nil}},
		{"create_asset_shortlink", map[string]any{"assetId": assetID, "expiresAt": "2020-01-01T00:00:00Z"}},
		{"create_asset_shortlink", map[string]any{"assetId": assetID, "title": strings.Repeat("x", 121)}},
		{"begin_asset_upload", map[string]any{"fileName": "report.txt", "sizeBytes": 1.25}},
		{"begin_asset_upload", map[string]any{"fileName": "report.txt", "sizeBytes": 0}},
		{"begin_asset_upload", map[string]any{"fileName": "report.txt", "sizeBytes": json.Number("9223372036854775808")}},
		{"upload_asset_part", map[string]any{"uploadSessionId": sessionID, "partNumber": 1.5, "contentBase64": "YWI="}},
		{"upload_asset_part", map[string]any{"uploadSessionId": sessionID, "partNumber": 0, "contentBase64": "YWI="}},
		{"upload_asset_part", map[string]any{"uploadSessionId": sessionID, "partNumber": 1, "contentBase64": "!!!!"}},
		{"upload_asset_part", map[string]any{"uploadSessionId": sessionID, "partNumber": 1, "contentBase64": " "}},
		{"upload_file_and_share", map[string]any{"fileName": "report.txt", "fileContentBase64": "!!!!"}},
		{"get_asset_upload_status", map[string]any{"uploadSessionId": false}},
		{"complete_asset_upload", map[string]any{"uploadSessionId": "wrong"}},
		{"cancel_asset_upload", map[string]any{"uploadSessionId": nil}},
	}
	for index, tc := range cases {
		t.Run(tc.name+"/"+stringInt64(int64(index)), func(t *testing.T) {
			f := newMCPFixture(t)
			result := mcpCall(t, f, tc.name, tc.args)
			if !result.IsError || result.StructuredContent != nil {
				t.Fatalf("malformed arguments accepted: %+v", result)
			}
		})
	}
	for _, params := range []string{`{"name":"get_asset_info","arguments":[]}`, `{"name":"get_asset_info","arguments":"bad"}`} {
		response := handleMCPMethod(context.Background(), jsonRPCRequest{Method: "tools/call", Params: json.RawMessage(params)}, mcpContext{}, Dependencies{})
		if response.Error == nil || response.Error.Code != -32602 {
			t.Fatalf("malformed RPC params accepted: %+v", response)
		}
	}
}

func TestMCPBeginUploadRejectsBusinessOverride(t *testing.T) {
	f := newMCPFixture(t)
	f.ctx.assetActor.BusinessID = "canvas"
	result := mcpCall(t, f, "begin_asset_upload", map[string]any{"fileName": "report.txt", "sizeBytes": 7, "businessId": "other"})
	if !result.IsError || !strings.Contains(result.Content[0].Text, "未授权该业务") {
		t.Fatalf("business override accepted: %+v", result)
	}
}

func TestMCPHTTPRequiresAuthentication(t *testing.T) {
	f := newMCPFixture(t)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	registerMCPRoutes(router, f.deps)
	request := httptest.NewRequest(http.MethodPost, "/api/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated MCP = %d, %s", response.Code, response.Body.String())
	}
}

type mcpSessionFixture struct {
	id      uuid.UUID
	status  string
	expires time.Time
}

func (f *mcpFixture) expectSession(session mcpSessionFixture, lock string, completed bool) {
	var asset any
	if completed {
		asset = f.assetID.String()
	}
	query := `SELECT upload_session_id,workspace_id,owner_user_id,business_id,api_key_id,.* FROM asset_upload_sessions WHERE upload_session_id=\$1` + lock + `$`
	f.mock.ExpectQuery(query).WithArgs(session.id).WillReturnRows(sqlmock.NewRows([]string{
		"session", "workspace", "owner", "business", "api_key", "bucket", "key", "remote", "name", "type", "path", "size", "part_size", "count", "reserved", "status", "expires", "asset", "idempotency", "cancelled",
	}).AddRow(session.id, f.ctx.assetActor.WorkspaceID, f.ctx.assetActor.UserID, "canvas", nil, "private-bucket", "internal/object", "remote-upload", "report.txt", "text/plain", "agent-uploads", 7, 32<<20, 1, 7, session.status, session.expires, asset, "request-1", nil))
}

func TestMCPMultipartSuccessReturnsDirectUploadSessionDTO(t *testing.T) {
	for _, name := range []string{"begin_asset_upload", "upload_asset_part", "get_asset_upload_status", "complete_asset_upload", "cancel_asset_upload"} {
		t.Run(name, func(t *testing.T) {
			f := newMCPFixture(t)
			session := mcpSessionFixture{id: uuid.New(), status: "completed", expires: time.Now().UTC().Add(time.Hour).Truncate(time.Second)}
			args := map[string]any{"uploadSessionId": "upl_" + session.id.String()}
			switch name {
			case "begin_asset_upload":
				// A matching idempotency key must return the existing DTO unchanged.
				session.status = "uploading"
				args = map[string]any{"fileName": "report.txt", "contentType": "text/plain", "sizeBytes": 7, "businessId": "canvas", "idempotencyKey": "request-1"}
				f.mock.ExpectBegin()
				f.mock.ExpectQuery(`SELECT role,max_single_file_bytes,storage_used_bytes,storage_reserved_bytes,storage_quota_bytes FROM users`).WithArgs(f.ctx.assetActor.UserID).
					WillReturnRows(sqlmock.NewRows([]string{"role", "max", "used", "reserved", "quota"}).AddRow("admin", 100, 0, 0, 1000))
				f.mock.ExpectQuery(`SELECT upload_session_id::text,business_id,logical_path,original_name,content_type,expected_size_bytes FROM asset_upload_sessions`).WithArgs(f.ctx.assetActor.UserID, "request-1").
					WillReturnRows(sqlmock.NewRows([]string{"id", "business", "path", "name", "type", "size"}).AddRow(session.id.String(), "canvas", "agent-uploads", "report.txt", "text/plain", 7))
				f.mock.ExpectRollback()
			case "upload_asset_part":
				session.status = "uploading"
				args["partNumber"], args["contentBase64"] = 1, base64.StdEncoding.EncodeToString([]byte("payload"))
				f.mock.ExpectBegin()
				f.expectSession(session, " FOR SHARE", false)
				f.mock.ExpectExec(`INSERT INTO asset_upload_parts`).WithArgs(session.id, 1, int64(7), "part-etag").WillReturnResult(sqlmock.NewResult(0, 1))
				f.mock.ExpectCommit()
			case "complete_asset_upload":
				f.mock.ExpectBegin()
				f.expectSession(session, " FOR UPDATE", true)
				f.mock.ExpectRollback()
			case "cancel_asset_upload":
				session.status = "cancelled"
				f.mock.ExpectBegin()
				f.expectSession(session, " FOR UPDATE", false)
				f.mock.ExpectRollback()
			}
			if name != "cancel_asset_upload" {
				f.expectSession(session, "", session.status == "completed")
				if session.status == "completed" {
					f.expectAsset("canvas", f.ctx.assetActor.WorkspaceID, nil)
				}
			}
			result := mcpCall(t, f, name, args)
			object := mcpStructured(t, result)
			dto, ok := result.StructuredContent.(assets.UploadSessionDTO)
			if !ok || dto.UploadSessionID != "upl_"+session.id.String() || dto.Status != session.status || dto.BusinessID != "canvas" || dto.SizeBytes != 7 || dto.PartSizeBytes != 32<<20 || dto.PartCount != 1 {
				t.Fatalf("incorrect multipart DTO: %#v", result.StructuredContent)
			}
			if _, wrapped := object["result"]; wrapped {
				t.Fatal("multipart DTO was wrapped")
			}
			if session.status == "completed" && (dto.Asset == nil || dto.Asset.AssetID != f.asset().AssetID || dto.ReceivedBytes != 7 || !reflect.DeepEqual(dto.ReceivedParts, []int{1})) {
				t.Fatalf("completed DTO lost asset/progress: %+v", dto)
			}
			if name == "upload_asset_part" && (string(f.store.put) != "payload" || dto.ReceivedBytes != 7 || !reflect.DeepEqual(dto.ReceivedParts, []int{1})) {
				t.Fatalf("incorrect part/progress: %q, %+v", f.store.put, dto)
			}
		})
	}
}

func TestMCPUploadFileAndShareReturnsSameShareDTO(t *testing.T) {
	f := newMCPFixture(t)
	f.ctx.assetActor.BusinessID, f.ctx.actor.BusinessID = "canvas", "canvas"
	f.mock.ExpectQuery(regexp.QuoteMeta(`SELECT max_single_file_bytes FROM users WHERE user_id=$1`)).WithArgs(f.ctx.assetActor.UserID).
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(100))
	f.mock.ExpectBegin()
	f.mock.ExpectQuery(`SELECT role,max_single_file_bytes,storage_used_bytes,storage_reserved_bytes,storage_quota_bytes FROM users`).WithArgs(f.ctx.assetActor.UserID).
		WillReturnRows(sqlmock.NewRows([]string{"role", "max", "used", "reserved", "quota"}).AddRow("admin", 100, 0, 0, 1000))
	f.mock.ExpectQuery(`SELECT bucket_name FROM business_integrations`).WithArgs("canvas").WillReturnRows(sqlmock.NewRows([]string{"bucket"}).AddRow("private-bucket"))
	f.mock.ExpectExec(`UPDATE users SET storage_used_bytes=storage_used_bytes\+\$2`).WithArgs(f.ctx.assetActor.UserID, int64(7)).WillReturnResult(sqlmock.NewResult(0, 1))
	f.mock.ExpectExec(`INSERT INTO assets`).WillReturnResult(sqlmock.NewResult(0, 1))
	f.mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))
	f.mock.ExpectCommit()
	f.expectCreateLink(-1, nil, "report.txt", sqlmock.AnyArg())
	result := mcpCall(t, f, "upload_file_and_share", map[string]any{"fileName": "report.txt", "fileContentBase64": base64.StdEncoding.EncodeToString([]byte("payload"))})
	object := mcpStructured(t, result)
	assertMCPKeys(t, object, "asset", "shortUrl", "code", "maxVisits")
	share, ok := result.StructuredContent.(mcpAssetShareDTO)
	if !ok || share.MaxVisits != -1 || share.ExpiresAt != nil || share.Asset.BusinessID != "canvas" || share.Asset.SizeBytes != 7 || !strings.HasPrefix(share.Asset.AssetID, "ast_") || string(f.store.put) != "payload" {
		t.Fatalf("incorrect share/upload DTO: %+v, bytes=%q", share, f.store.put)
	}
	assertMCPKeys(t, object["asset"].(map[string]any), "assetId", "businessId", "fileName", "contentType", "sizeBytes", "checksumSha256", "createdAt")
}

// Keep a compile-time check on the argument matcher used for generated asset IDs.
var _ sqlmock.Argument = mcpUUIDArg{}
