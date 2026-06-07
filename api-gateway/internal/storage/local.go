package storage

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"time"
)

var StorageDir string

func init() {
	dir := os.Getenv("STORAGE_DIR")
	if dir == "" {
		dir = "./storage"
	}
	StorageDir = filepath.Clean(dir)

	// Ensure the directory exists
	if err := os.MkdirAll(StorageDir, 0750); err != nil {
		log.Fatalf("failed to create storage directory: %v", err)
	}
}

// WriteFile writes bytes to the secure storage folder
func WriteFile(filename string, data []byte) error {
	destPath := filepath.Join(StorageDir, filepath.Base(filename))
	// Hard check against path traversal
	if filepath.Dir(destPath) != StorageDir {
		return fmt.Errorf("security block: attempted path traversal for file %s", filename)
	}

	return ioutil.WriteFile(destPath, data, 0600)
}

// ReadFile retrieves file bytes from storage folder
func ReadFile(filename string) ([]byte, error) {
	srcPath := filepath.Join(StorageDir, filepath.Base(filename))
	if filepath.Dir(srcPath) != StorageDir {
		return nil, fmt.Errorf("security block: attempted path traversal for file %s", filename)
	}

	return ioutil.ReadFile(srcPath)
}

// GetFilePath returns the absolute/clean path of a file in the storage directory
func GetFilePath(filename string) (string, error) {
	srcPath := filepath.Join(StorageDir, filepath.Base(filename))
	if filepath.Dir(srcPath) != StorageDir {
		return "", fmt.Errorf("security block: attempted path traversal")
	}
	return srcPath, nil
}

// DeleteFile immediately erases a file
func DeleteFile(filename string) error {
	destPath := filepath.Join(StorageDir, filepath.Base(filename))
	if filepath.Dir(destPath) != StorageDir {
		return fmt.Errorf("security block: attempted path traversal for file %s", filename)
	}
	return os.Remove(destPath)
}

// StartPurgeWorker runs in a background thread to prune files older than 1 hour (TTL hard delete)
func StartPurgeWorker(checkInterval time.Duration, maxAge time.Duration) {
	ticker := time.NewTicker(checkInterval)
	go func() {
		log.Printf("Purge worker started. Checking storage every %s, pruning files older than %s", checkInterval, maxAge)
		for range ticker.C {
			files, err := ioutil.ReadDir(StorageDir)
			if err != nil {
				log.Printf("[ERR] Purge worker failed to read storage directory: %v", err)
				continue
			}

			now := time.Now()
			deletedCount := 0

			for _, f := range files {
				if f.IsDir() {
					continue
				}

				filePath := filepath.Join(StorageDir, f.Name())
				// Check mod time
				if now.Sub(f.ModTime()) > maxAge {
					if err := os.Remove(filePath); err != nil {
						log.Printf("[ERR] Purge worker failed to delete %s: %v", f.Name(), err)
					} else {
						deletedCount++
					}
				}
			}

			if deletedCount > 0 {
				log.Printf("[AUDIT] Purge worker automatically deleted %d expired files (TTL elapsed)", deletedCount)
			}
		}
	}()
}
