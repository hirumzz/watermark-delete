package security

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/image/webp"
	"image/jpeg"
	"image/png"
)

// Initialize webp decoder register
func init() {
	image.RegisterFormat("webp", "RIFF????WEBP", webp.Decode, webp.DecodeConfig)
}

// Allowed MIME types
const (
	MimeJPEG = "image/jpeg"
	MimePNG  = "image/png"
	MimeWEBP = "image/webp"
)

// VerifyMagicBytes checks file signature
func VerifyMagicBytes(header []byte) (string, error) {
	if len(header) < 12 {
		return "", errors.New("file too short to verify magic bytes")
	}

	// JPEG check
	if header[0] == 0xFF && header[1] == 0xD8 && header[2] == 0xFF {
		return MimeJPEG, nil
	}

	// PNG check
	if header[0] == 0x89 && header[1] == 0x50 && header[2] == 0x4E && header[3] == 0x47 &&
		header[4] == 0x0D && header[5] == 0x0A && header[6] == 0x1A && header[7] == 0x0A {
		return MimePNG, nil
	}

	// WEBP check (RIFF....WEBP)
	if header[0] == 'R' && header[1] == 'I' && header[2] == 'F' && header[3] == 'F' &&
		header[8] == 'W' && header[9] == 'E' && header[10] == 'B' && header[11] == 'P' {
		return MimeWEBP, nil
	}

	return "", errors.New("unsupported file signature or format")
}

// SanitizeImage decodes the image fully to strip metadata and prevent polyglot attacks
func SanitizeImage(r io.Reader) ([]byte, string, error) {
	// Read first 12 bytes for signature verification
	var buf bytes.Buffer
	tee := io.TeeReader(r, &buf)

	sigHeader := make([]byte, 12)
	n, err := io.ReadFull(tee, sigHeader)
	if err != nil && err != io.ErrUnexpectedEOF {
		return nil, "", fmt.Errorf("failed to read file header: %w", err)
	}
	if n < 12 {
		return nil, "", errors.New("payload too short for signature validation")
	}

	_, err = VerifyMagicBytes(sigHeader)
	if err != nil {
		return nil, "", err
	}

	// Concatenate signature header and the rest of the stream
	fullReader := io.MultiReader(bytes.NewReader(buf.Bytes()), r)

	// Decode the image fully. This checks for malformed image payloads.
	img, format, err := image.Decode(fullReader)
	if err != nil {
		return nil, "", fmt.Errorf("failed to decode image: %w", err)
	}

	// Re-encode image to completely strip any embedded EXIF or malicious polyglot scripts
	var outBuf bytes.Buffer
	var outExt string

	switch format {
	case "jpeg":
		outExt = ".jpg"
		err = jpeg.Encode(&outBuf, img, &jpeg.Options{Quality: 90})
	case "png":
		outExt = ".png"
		err = png.Encode(&outBuf, img)
	case "webp":
		// Re-encode WebP as high-quality PNG to strip metadata while retaining original quality and aspect ratio
		outExt = ".png"
		err = png.Encode(&outBuf, img)
	default:
		return nil, "", fmt.Errorf("unsupported format decoded: %s", format)
	}

	if err != nil {
		return nil, "", fmt.Errorf("failed to re-encode image: %w", err)
	}

	return outBuf.Bytes(), outExt, nil
}

// FilenameSanitizer strips unsafe characters from filenames (though we store by UUID internally)
func SanitizeFilename(name string) string {
	reg := regexp.MustCompile(`[^a-zA-Z0-9.-]`)
	cleaned := reg.ReplaceAllString(name, "_")
	if len(cleaned) > 100 {
		cleaned = cleaned[:100]
	}
	return cleaned
}

// SecurityHeadersMiddleware enforces secure headers to prevent XSS, clickjacking, and execute restrictions
func SecurityHeadersMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Writer.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; frame-ancestors 'none'; object-src 'none';")
		c.Writer.Header().Set("X-Frame-Options", "DENY")
		c.Writer.Header().Set("X-Content-Type-Options", "nosniff")
		c.Writer.Header().Set("X-XSS-Protection", "1; mode=block")
		c.Writer.Header().Set("Referrer-Policy", "no-referrer-when-downgrade")
		c.Next()
	}
}

// IP-based Rate Limiter configuration
type ipRateLimiter struct {
	sync.RWMutex
	ips map[string]int
}

var limiter = ipRateLimiter{ips: make(map[string]int)}

func init() {
	// Reset rate limits every minute
	go func() {
		for {
			time.Sleep(1 * time.Minute)
			limiter.Lock()
			limiter.ips = make(map[string]int)
			limiter.Unlock()
		}
	}()
}

// RateLimitMiddleware blocks flooding
func RateLimitMiddleware(maxRequests int) gin.HandlerFunc {
	return func(c *gin.Context) {
		ip := c.ClientIP()
		if ip == "" {
			ip = "unknown"
		}

		limiter.Lock()
		limiter.ips[ip]++
		count := limiter.ips[ip]
		limiter.Unlock()

		if count > maxRequests {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": "Too many requests. Please try again later.",
			})
			return
		}
		c.Next()
	}
}

// PathTraversalGuard prevents directory traversal attacks
func PathTraversalGuard(path string) bool {
	return strings.Contains(path, "..") || strings.Contains(path, "/") || strings.Contains(path, "\\")
}
