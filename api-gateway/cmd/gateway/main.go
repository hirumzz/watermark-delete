package main

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/hirumzz/watermark-delete/api-gateway/internal/auth"
	"github.com/hirumzz/watermark-delete/api-gateway/internal/queue"
	"github.com/hirumzz/watermark-delete/api-gateway/internal/security"
	"github.com/hirumzz/watermark-delete/api-gateway/internal/storage"
)

func main() {
	// Start the file purge worker in background (checks every 5m, prunes files older than 1h)
	storage.StartPurgeWorker(5*time.Minute, 1*time.Hour)

	// Initialize Redis Connection
	qm, err := queue.NewQueueManager()
	if err != nil {
		log.Fatalf("Critical error initializing queue: %v", err)
	}

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	// Set up security headers and CORS
	r.Use(security.SecurityHeadersMiddleware())
	r.Use(corsMiddleware())

	// Session middleware applies to upload and job polling
	api := r.Group("/api")
	api.Use(auth.SessionMiddleware())
	api.Use(security.RateLimitMiddleware(120)) // Allow 120 reqs/min

	// Endpoints
	api.POST("/upload", handleUpload(qm))
	api.GET("/job/:id", handleJobStatus(qm))
	api.GET("/download-zip/:id", handleZipDownload(qm))

	// Signed downloads - no session validation needed, signature does the auth
	r.GET("/api/download/:id", security.RateLimitMiddleware(60), handleDownload())

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("[AUDIT] API Gateway listening on port %s", port)
	if err := r.Run(":" + port); err != nil {
		log.Fatalf("Failed to run gateway server: %v", err)
	}
}

func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.Request.Header.Get("Origin")
		if origin != "" {
			c.Writer.Header().Set("Access-Control-Allow-Origin", origin)
			c.Writer.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS, PUT, DELETE")
			c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization, X-Session-Token")
			c.Writer.Header().Set("Access-Control-Expose-Headers", "Content-Length, Access-Control-Allow-Origin, X-Session-Token")
			c.Writer.Header().Set("Access-Control-Allow-Credentials", "true")
		}

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

func handleUpload(qm *queue.QueueManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		sessionToken := c.MustGet("session_token").(string)

		// Parse multi-part form (max memory 32MB)
		err := c.Request.ParseMultipartForm(32 << 20)
		if err != nil {
			log.Printf("[ERR] Failed to parse multipart form: %v", err)
			c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to parse upload request"})
			return
		}

		form, err := c.MultipartForm()
		if err != nil {
			log.Printf("[ERR] Failed to get multipart form: %v", err)
			c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to extract uploaded files"})
			return
		}

		files := form.File["images"]
		if len(files) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "No images provided"})
			return
		}

		// Enforce size limit (max 10MB per image)
		const maxImageSize = 10 * 1024 * 1024
		var jobFiles []queue.JobFile

		for _, fileHeader := range files {
			if fileHeader.Size > maxImageSize {
				c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Image %s exceeds size limit of 10MB", fileHeader.Filename)})
				return
			}

			// Open file
			file, err := fileHeader.Open()
			if err != nil {
				log.Printf("[ERR] Failed to open uploaded file %s: %v", fileHeader.Filename, err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to process image"})
				return
			}

			// Sanitize image in memory (Checks magic bytes, Decodes fully, Re-encodes as JPEG/PNG, strips metadata)
			sanitizedBytes, ext, err := security.SanitizeImage(file)
			file.Close()
			if err != nil {
				// Avoid exposing raw CV error leaks to user
				log.Printf("[SECURITY AUDIT] Malformed / attack signature upload rejected for file %s: %v", fileHeader.Filename, err)
				c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid or corrupted image payload"})
				return
			}

			// Generate random secure filename UUID
			fileUUID := uuid.New().String()
			diskFilename := fileUUID + ext

			// Write to secure storage
			if err := storage.WriteFile(diskFilename, sanitizedBytes); err != nil {
				log.Printf("[ERR] Failed to write sanitized image to disk: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal storage failure"})
				return
			}

			jobFiles = append(jobFiles, queue.JobFile{
				ID:       fileUUID,
				Original: diskFilename,
				Status:   "PENDING",
			})
		}

		// Create job state
		jobID := uuid.New().String()
		job := &queue.JobState{
			ID:       jobID,
			UserID:   sessionToken,
			Files:    jobFiles,
			Progress: 0,
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := qm.EnqueueJob(ctx, job); err != nil {
			log.Printf("[ERR] Failed to enqueue job %s: %v", jobID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to queue job"})
			return
		}

		log.Printf("[AUDIT] User session %s successfully created Job %s with %d files", sessionToken, jobID, len(jobFiles))

		c.JSON(http.StatusAccepted, gin.H{
			"job_id": jobID,
		})
	}
}

func handleJobStatus(qm *queue.QueueManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		jobID := c.Param("id")
		sessionToken := c.MustGet("session_token").(string)

		if _, err := uuid.Parse(jobID); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid job ID format"})
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		job, err := qm.GetJobState(ctx, jobID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Job not found"})
			return
		}

		// Ensure user can only access their own jobs
		if job.UserID != sessionToken {
			log.Printf("[SECURITY AUDIT] Unauthorized job access attempt! User: %s, Job: %s (owned by %s)", sessionToken, job.ID, job.UserID)
			c.JSON(http.StatusForbidden, gin.H{"error": "Unauthorized access"})
			return
		}

		// Enrich response files with short-lived signed download URLs if they are completed
		type fileResp struct {
			ID          string `json:"id"`
			Status      string `json:"status"`
			DownloadURL string `json:"download_url,omitempty"`
			Error       string `json:"error,omitempty"`
		}

		files := make([]fileResp, len(job.Files))
		for i, f := range job.Files {
			files[i] = fileResp{
				ID:     f.ID,
				Status: f.Status,
				Error:  f.Error,
			}
			if f.Status == "DONE" && f.Processed != "" {
				// Expire download link in 15 minutes (900 seconds)
				expiresAt := time.Now().Add(15 * time.Minute).Unix()
				sig := auth.GenerateSignature(f.Processed, expiresAt)
				files[i].DownloadURL = fmt.Sprintf("/api/download/%s?expires=%d&sig=%s", f.Processed, expiresAt, sig)
			}
		}

		c.JSON(http.StatusOK, gin.H{
			"id":         job.ID,
			"status":     job.Status,
			"progress":   job.Progress,
			"files":      files,
			"updated_at": job.UpdatedAt,
		})
	}
}

func handleDownload() gin.HandlerFunc {
	return func(c *gin.Context) {
		fileID := c.Param("id")
		expires := c.Query("expires")
		sig := c.Query("sig")

		if fileID == "" || expires == "" || sig == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Missing signature parameters"})
			return
		}

		// Security: Validate path traversal
		if security.PathTraversalGuard(fileID) {
			log.Printf("[SECURITY AUDIT] Path traversal download blocked: %s", fileID)
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
			return
		}

		// Validate token link validity
		if err := auth.ValidateSignature(fileID, expires, sig); err != nil {
			log.Printf("[SECURITY AUDIT] Invalid signed download request for file %s: %v", fileID, err)
			c.JSON(http.StatusForbidden, gin.H{"error": "Download link is expired or invalid"})
			return
		}

		filePath, err := storage.GetFilePath(fileID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid filename"})
			return
		}

		// Verify file actually exists
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "File not found"})
			return
		}

		c.File(filePath)
	}
}

func handleZipDownload(qm *queue.QueueManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		jobID := c.Param("id")
		sessionToken := c.MustGet("session_token").(string)

		if _, err := uuid.Parse(jobID); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid job ID format"})
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		job, err := qm.GetJobState(ctx, jobID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Job not found"})
			return
		}

		if job.UserID != sessionToken {
			log.Printf("[SECURITY AUDIT] Unauthorized ZIP download attempt! User: %s, Job: %s", sessionToken, job.ID)
			c.JSON(http.StatusForbidden, gin.H{"error": "Unauthorized access"})
			return
		}

		if job.Status != "DONE" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Job processing not completed yet"})
			return
		}

		// Setup streaming headers
		c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"processed_%s.zip\"", job.ID[:8]))
		c.Header("Content-Type", "application/zip")
		c.Header("Transfer-Encoding", "chunked")

		// Create streaming response
		c.Stream(func(w io.Writer) bool {
			zipWriter := zip.NewWriter(w)
			defer zipWriter.Close()

			for _, f := range job.Files {
				if f.Status != "DONE" || f.Processed == "" {
					continue
				}

				filePath, err := storage.GetFilePath(f.Processed)
				if err != nil {
					log.Printf("[ERR] ZIP stream: invalid filepath resolution: %v", err)
					continue
				}

				file, err := os.Open(filePath)
				if err != nil {
					log.Printf("[ERR] ZIP stream: failed to open file %s: %v", f.Processed, err)
					continue
				}

				// Create file in ZIP
				zipFileEntry, err := zipWriter.Create(f.Processed)
				if err != nil {
					file.Close()
					log.Printf("[ERR] ZIP stream: failed to create ZIP entry: %v", err)
					return false
				}

				// Copy file contents without buffering whole file in memory
				_, err = io.Copy(zipFileEntry, file)
				file.Close()
				if err != nil {
					log.Printf("[ERR] ZIP stream: failed to stream file data: %v", err)
					return false
				}
			}

			return false // End of stream
		})
	}
}
