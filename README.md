# Watermark Eraser Microservices System

A lightweight, high-performance, and secure microservice-based system for uploading, queueing, and processing images to detect and remove text/logo watermarks asynchronously using OpenCV.

---

## Architecture

```
                                          ┌──────────────┐
                                          │ React UI     │
                                          │ (Port 3000)  │
                                          └──────┬───────┘
                                                 │
                                                 ▼ (HTTP)
                                          ┌──────────────┐
                                          │ API Gateway  │
                                          │ (Port 8080)  │
                                          └─┬──────────┬─┘
                                            │          │
                       Enqueue (Redis List) │          │ Read / Write File
                                            ▼          ▼
┌──────────────┐                          ┌──────────────┐                          ┌──────────────┐
│ Image Worker │ ◄─────────────────────── │ Redis Queue  │                          │ Shared       │
│ (FastAPI)    │   Pop Job ID & State     │ (Private)    │                          │ Storage      │
└──────┬───────┘                          └──────────────┘                          └──────┬───────┘
       │                                                                                   │
       └────────────────────────────────── Writes Processed Images ────────────────────────┘
```

The system comprises four services:
1. **Frontend Service**: A modern, mobile-friendly dashboard built with React, Vite, and custom CSS. Enables drag-and-drop, progress indication, and single or ZIP downloads.
2. **API Gateway (Go)**: Validates incoming uploads (checks magic bytes and decodes fully to check for polyglots, strips EXIF metadata), handles secure session creation, issues short-lived HMAC-signed download links, and streams dynamic zip creations.
3. **Image Worker (Python)**: Subscribes to the Redis queue, uses OpenCV heuristics (contour/edge analysis and morphological processing) to find watermark overlays, removes them via inpainting, enhances details via bilateral filtering, and supports checkpoint resume.
4. **Queue & Caching (Redis)**: Stores job queues and state checkpoints (`UPLOADED`, `QUEUED`, `PROCESSING`, `DONE`, `FAILED`).

---

## Security Implementation Highlights

* **Zero-Trust Input Sanitization**: The API gateway verifies file signature magic bytes (not file extensions). Uploaded images are decoded in-memory and re-encoded back to a clean state. This strips all EXIF metadata and completely neutralizes polyglot files.
* **UUID Isolation**: Images are written to storage using random UUIDs. Raw directory structures and original filenames are never exposed to clients.
* **HMAC Link Signing**: Processed downloads use signed, time-limited URLs validated via HMAC-SHA256. If a URL is tampered with or expires (TTL of 15 minutes), access is blocked.
* **Sandboxed Network Topology**: Only the Frontend and API Gateway expose ports. Redis and the Python worker run inside an `internal` network with no default gateway, preventing outbound/inbound internet access for the worker.
* **Non-Root Execution**: Every Docker container runs as a non-privileged non-root user (`nobody` or `nginx`).
* **Git Hook Checks**: A pre-commit script automatically scans all modified files (especially `.md`) for credentials, private keys, and secrets, aborting the commit if found.

---

## REST API Endpoints

### 1. Upload Images
* **Endpoint**: `POST /api/upload`
* **Headers**: `X-Session-Token` (Optional; created automatically if missing)
* **Payload**: `multipart/form-data` with key `images` (Multiple files allowed, max 10MB per image, JPEG/PNG/WEBP only)
* **Response**: `202 Accepted`
```json
{
  "job_id": "b96c7df3-e99d-4e92-9118-2e06c7104b2c"
}
```

### 2. Job Status Checkpoint
* **Endpoint**: `GET /api/job/:id`
* **Headers**: `X-Session-Token` (Required)
* **Response**: `200 OK`
```json
{
  "id": "b96c7df3-e99d-4e92-9118-2e06c7104b2c",
  "status": "PROCESSING",
  "progress": 60,
  "files": [
    { "id": "4020a597-9e45-4221-a3f1-ef4b5585642d", "status": "DONE", "download_url": "/api/download/proc_4020a597-9e45-4221-a3f1-ef4b5585642d.png?expires=1717755200&sig=ab73..." },
    { "id": "993af751-2a21-4f1e-8fe2-581335cb99b3", "status": "PENDING" }
  ],
  "updated_at": "2026-06-07T13:46:58Z"
}
```

### 3. Signed File Download
* **Endpoint**: `GET /api/download/:id?expires=<timestamp>&sig=<hmac_signature>`
* **Response**: Serves raw processed image file.

### 4. Stream ZIP Download
* **Endpoint**: `GET /api/download-zip/:id?token=<session_token>`
* **Response**: Streams a ZIP archive containing all successfully processed images of the job.

---

## Running Locally

### Prerequisites
* [Docker Desktop](https://www.docker.com/products/docker-desktop/) or Docker Engine with Docker Compose.

### Start the Services
Navigate to the root directory and start the stack:
```bash
docker compose up -d --build
```

Access the UI at: **`http://localhost:3000`**  
The API Gateway is reachable at: **`http://localhost:8080`**

### View Container Logs
```bash
docker compose logs -f
```

### Shutting Down
```bash
docker compose down -v
```

---

## Git Pre-Commit Hook Configuration

To enforce the Git security rules locally, copy the pre-commit script to your `.git/hooks` folder:
```bash
# On Linux/macOS
cp pre-commit.sh .git/hooks/pre-commit
chmod +x .git/hooks/pre-commit

# On Windows (PowerShell)
Copy-Item -Path .\pre-commit.sh -Destination .\.git\hooks\pre-commit -Force
```

---

## Production Swarm Deployment Notes

For deploying this architecture to production via **Docker Swarm**:

1. **Deploy Manifest**: Define a `stack.yml` file configuring replicas, service updates, placement constraints, and networks.
2. **Secrets Management**: Use Docker Swarm Secrets (`docker secret create`) to safely store database connections, private keys, and `DOWNLOAD_SIGNING_SECRET`. Reference them inside service configs as:
   ```yaml
   secrets:
     - download_signing_secret
   ```
3. **S3-Compatible Storage**: In production, replace the local volume `shared-storage` with an S3-compatible backend (e.g. AWS S3 or MinIO). The Go gateway and Python worker should read and write files via an S3 client using IAM roles or Swarm Secrets.
4. **Ingress Inversion**: Deploy an Ingress Controller (like Traefik or Nginx Ingress) as a Swarm global service. Configure routing rules so that ONLY the `frontend-service` and the `/api` route of the `api-gateway` are exposed publicly, mapping routing paths directly through the overlay network.
