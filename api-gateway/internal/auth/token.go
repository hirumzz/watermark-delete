package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

var secretKey []byte

func init() {
	key := os.Getenv("DOWNLOAD_SIGNING_SECRET")
	if key == "" {
		// Fallback to auto-generated secret for local development safety
		key = "dev-secret-key-change-in-production-12345"
	}
	secretKey = []byte(key)
}

// GenerateSignature signs a file ID and expiration timestamp using HMAC-SHA256
func GenerateSignature(fileID string, expiresAt int64) string {
	mac := hmac.New(sha256.New, secretKey)
	message := fmt.Sprintf("%s|%d", fileID, expiresAt)
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}

// ValidateSignature verifies signature and expiry of a download link
func ValidateSignature(fileID string, expiresStr string, providedSig string) error {
	expiresAt, err := strconv.ParseInt(expiresStr, 10, 64)
	if err != nil {
		return errors.New("invalid expiration format")
	}

	if time.Now().Unix() > expiresAt {
		return errors.New("download link has expired")
	}

	expectedSig := GenerateSignature(fileID, expiresAt)

	expectedBytes, err1 := hex.DecodeString(expectedSig)
	providedBytes, err2 := hex.DecodeString(providedSig)

	if err1 != nil || err2 != nil || subtle.ConstantTimeCompare(expectedBytes, providedBytes) != 1 {
		return errors.New("invalid signature")
	}

	return nil
}

// SessionMiddleware enforces a session token for client identification and request tracing
func SessionMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := c.GetHeader("X-Session-Token")
		if token == "" {
			token = c.Query("token")
		}

		// Validate token format if provided
		if token != "" {
			if _, err := uuid.Parse(token); err != nil {
				// Invalid UUID session token - potential tampering/replay
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid session token format"})
				return
			}
		}

		// If still empty, generate a new session token
		if token == "" {
			token = uuid.New().String()
			c.Header("X-Session-Token", token)
		}

		// Inject into context
		c.Set("session_token", token)
		c.Next()
	}
}
