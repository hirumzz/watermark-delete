import os
import time
import json
import logging
import threading
import numpy as np
import cv2
from fastapi import FastAPI
from redis import Redis

# Configure structured-like logging
logging.basicConfig(
    level=logging.INFO,
    format='{"time": "%(asctime)s", "level": "%(levelname)s", "message": "%(message)s"}'
)
logger = logging.getLogger("worker")

app = FastAPI(title="Image Worker Service")
redis_addr = os.getenv("REDIS_ADDR", "localhost:6379")
storage_dir = os.getenv("STORAGE_DIR", "./storage")

# Initialize Redis client
r_client = None
try:
    host, port = redis_addr.split(":")
    r_client = Redis(host=host, port=int(port), socket_connect_timeout=5)
    logger.info(f"Connected to Redis at {redis_addr}")
except Exception as e:
    logger.error(f"Failed to connect to Redis: {e}")

@app.get("/health")
def health_check():
    """Health check endpoint for Docker container status monitoring."""
    if r_client is None:
        return {"status": "unhealthy", "redis": "disconnected"}
    try:
        r_client.ping()
        return {"status": "healthy", "redis": "connected"}
    except Exception as e:
        return {"status": "unhealthy", "redis": str(e)}

def detect_and_remove_watermark(image_path: str, output_path: str):
    """
    OpenCV Watermark detection heuristic and inpaint recovery logic.
    Maintains the original resolution and aspect ratio.
    """
    # Load image safely
    img = cv2.imread(image_path)
    if img is None:
        raise ValueError(f"Could not read image at {image_path}")

    h, w, c = img.shape

    # 1. Convert to grayscale
    gray = cv2.cvtColor(img, cv2.COLOR_BGR2GRAY)

    # 2. Heuristic: Apply Canny edge detection
    edges = cv2.Canny(gray, 40, 150)

    # 3. Morphological closing to group text elements/letters
    # Using a rectangular structuring element (horizontal structure for lines/text)
    kernel = cv2.getStructuringElement(cv2.MORPH_RECT, (15, 6))
    closed = cv2.morphologyEx(edges, cv2.MORPH_CLOSE, kernel)

    # 4. Find contours
    contours, _ = cv2.findContours(closed, cv2.RETR_EXTERNAL, cv2.CHAIN_APPROX_SIMPLE)

    # Generate binary mask of potential watermark areas
    mask = np.zeros((h, w), dtype=np.uint8)
    watermark_found = False

    # Watermarks are typically located near the borders/margins (outer 15%)
    margin_x = int(w * 0.15)
    margin_y = int(h * 0.15)

    for cnt in contours:
        x, y, w_cnt, h_cnt = cv2.boundingRect(cnt)

        # Skip tiny noise or massive layers
        area = w_cnt * h_cnt
        if area < 80 or area > (h * w * 0.10):
            continue

        # Check if the contour is located within the outer margins (top, bottom, left, or right)
        is_near_margin = (
            (x < margin_x) or 
            (y < margin_y) or 
            (x + w_cnt > w - margin_x) or 
            (y + h_cnt > h - margin_y)
        )

        if not is_near_margin:
            continue

        # Draw bounding box onto mask with small padding
        padding = 3
        x1 = max(0, x - padding)
        y1 = max(0, y - padding)
        x2 = min(w, x + w_cnt + padding)
        y2 = min(h, y + h_cnt + padding)

        cv2.rectangle(mask, (x1, y1), (x2, y2), 255, -1)
        watermark_found = True

    # 5. Restore original image via inpainting if watermark overlays were found
    if watermark_found:
        # Inpaint with small radius (3px) to prevent smudging details
        restored = cv2.inpaint(img, mask, inpaintRadius=3, flags=cv2.INPAINT_TELEA)
    else:
        restored = img.copy()

    # Save output file (bilateral filter completely removed to retain original sharpness)
    success = cv2.imwrite(output_path, restored)
    if not success:
        raise ValueError(f"Failed to write output image to {output_path}")

def process_job(job_id: str):
    """Processes a single image queue job and updates progress checkpoints."""
    job_key = f"job:{job_id}"
    try:
        # Fetch job state
        job_data = r_client.get(job_key)
        if not job_data:
            logger.error(f"Job state not found for {job_id}")
            return

        job = json.loads(job_data.decode("utf-8"))
        if job["status"] in ["DONE", "FAILED"]:
            logger.info(f"Job {job_id} already completed. Skipping.")
            return

        logger.info(f"Starting execution of Job {job_id}")

        # Update checkpoint to PROCESSING
        job["status"] = "PROCESSING"
        job["progress"] = 10
        r_client.set(job_key, json.dumps(job), ex=86400)

        files = job.get("files", [])
        total_files = len(files)
        completed_count = 0

        for i, file_info in enumerate(files):
            file_id = file_info["id"]

            # Checkpoint resume: skip files that are already completed
            if file_info.get("status") == "DONE":
                completed_count += 1
                logger.info(f"File {file_id} already processed. Skipping.")
                continue

            orig_filename = file_info["original"]
            orig_path = os.path.join(storage_dir, orig_filename)
            processed_filename = f"proc_{orig_filename}"
            processed_path = os.path.join(storage_dir, processed_filename)

            # Mark file processing state
            file_info["status"] = "PROCESSING"
            job["progress"] = int(10 + (completed_count / total_files) * 80)
            r_client.set(job_key, json.dumps(job), ex=86400)

            try:
                # Perform CV watermark detection and deletion
                detect_and_remove_watermark(orig_path, processed_path)

                file_info["status"] = "DONE"
                file_info["processed"] = processed_filename
                completed_count += 1
            except Exception as fe:
                # Record error inside file state to inform UI, without crashing whole pipeline
                logger.error(f"Failed processing file {file_id}: {fe}")
                file_info["status"] = "FAILED"
                file_info["error"] = "Processing failed"

            # Save progress update
            job["progress"] = int(10 + (completed_count / total_files) * 80)
            r_client.set(job_key, json.dumps(job), ex=86400)

        # Mark final status
        if completed_count == total_files:
            job["status"] = "DONE"
        elif completed_count == 0:
            job["status"] = "FAILED"
        else:
            # Partially completed jobs are marked DONE to allow downloading whatever succeeded
            job["status"] = "DONE"

        job["progress"] = 100
        r_client.set(job_key, json.dumps(job), ex=86400)
        logger.info(f"Completed execution of Job {job_id} with status {job['status']}")

    except Exception as je:
        logger.error(f"Critical failure in Job {job_id}: {je}")
        # Mark job as failed on general crashes
        try:
            job_data = r_client.get(job_key)
            if job_data:
                job = json.loads(job_data.decode("utf-8"))
                job["status"] = "FAILED"
                r_client.set(job_key, json.dumps(job), ex=86400)
        except Exception:
            pass

def queue_worker_loop():
    """Background loop listening for incoming job items from the Redis list."""
    if r_client is None:
        logger.error("Queue worker cannot start without Redis client connection")
        return

    logger.info("Background queue worker thread successfully started")
    while True:
        try:
            # BRPOP blocks until an item is pushed onto the 'job_queue'
            result = r_client.brpop("job_queue", timeout=3)
            if result:
                _, job_id_bytes = result
                job_id = job_id_bytes.decode("utf-8")
                process_job(job_id)
        except Exception as e:
            logger.error(f"Error in queue worker iteration: {e}")
            time.sleep(2)

# Start background queue thread
worker_thread = threading.Thread(target=queue_worker_loop, daemon=True)
worker_thread.start()
