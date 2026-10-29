package httpapi

import (
	"bytes"
	"mime/multipart"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/yourGaomu/short_links/back/internal/modules/auth"
)

func TestMultipartUploadMiddlewareRejectsBodyAbovePerUserLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	userID := uuid.New()
	mock.ExpectQuery("SELECT max_single_file_bytes FROM users WHERE user_id=\\$1").
		WithArgs(userID).WillReturnRows(sqlmock.NewRows([]string{"max_single_file_bytes"}).AddRow(1))

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("businessId", "canvas"); err != nil {
		t.Fatal(err)
	}
	file, err := writer.CreateFormFile("file", "large.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(bytes.Repeat([]byte{'x'}, 3<<20)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("authContext", authContext{User: auth.User{ID: userID, Role: "user"}})
		c.Next()
	})
	router.POST("/api/v1/assets", requireMultipartBusinessAccess(db), func(c *gin.Context) { c.Status(200) })
	request := httptest.NewRequest("POST", "/api/v1/assets", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != 413 {
		t.Fatalf("status = %d, want 413; body=%s", response.Code, response.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
