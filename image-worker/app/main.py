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

    # Define margins (outer 15%)
    margin_x = int(w * 0.15)
    margin_y = int(h * 0.15)

    # Initialize final binary mask for inpainted areas
    mask = np.zeros((h, w), dtype=np.uint8)
    watermark_found = False

    # 2. Heuristic: Scan at multiple threshold levels to handle varying transparency/backgrounds
    thresholds = [100, 130, 160, 190]
    for thresh_val in thresholds:
        _, thresh = cv2.threshold(gray, thresh_val, 255, cv2.THRESH_BINARY)

        # Morphological closing to group shapes/letters
        kernel = cv2.getStructuringElement(cv2.MORPH_RECT, (7, 7))
        closed = cv2.morphologyEx(thresh, cv2.MORPH_CLOSE, kernel)

        # Find contours
        contours, _ = cv2.findContours(closed, cv2.RETR_EXTERNAL, cv2.CHAIN_APPROX_SIMPLE)

        for cnt in contours:
            x, y, w_cnt, h_cnt = cv2.boundingRect(cnt)
            area = cv2.contourArea(cnt)
            bbox_area = w_cnt * h_cnt

            # Filters:
            # - Ignore tiny noise by setting min bounding area to 200px
            # - Ignore massive segments (backgrounds/walls) by setting max area to 5% of image
            if bbox_area < 200 or bbox_area > (h * w * 0.05):
                continue

            # Limit width & height of watermarks (watermark overlays are compact)
            if w_cnt > (w * 0.15) or h_cnt > (h * 0.10):
                continue

            # Location filter: restrict to horizontal margins or corners
            # This completely avoids middle-left or middle-right wall texture smudging
            is_in_top_margin = y < margin_y
            is_in_bottom_margin = y + h_cnt > h - margin_y
            is_in_top_left_corner = x < margin_x and y < int(margin_y * 1.5)
            is_in_bottom_left_corner = x < margin_x and y + h_cnt > h - int(margin_y * 1.5)
            is_in_top_right_corner = x + w_cnt > w - margin_x and y < int(margin_y * 1.5)
            is_in_bottom_right_corner = x + w_cnt > w - margin_x and y + h_cnt > h - int(margin_y * 1.5)

            is_valid_location = (
                is_in_top_margin or 
                is_in_bottom_margin or 
                is_in_top_left_corner or 
                is_in_bottom_left_corner or 
                is_in_top_right_corner or 
                is_in_bottom_right_corner
            )

            if not is_valid_location:
                continue

            # Solidity filter (watermark characters/logos are reasonably solid shapes)
            hull = cv2.convexHull(cnt)
            hull_area = cv2.contourArea(hull)
            solidity = float(area) / hull_area if hull_area > 0 else 0
            if solidity < 0.4:
                continue

            # Aspect ratio check (filters out long line artifacts like rails or stairs)
            aspect_ratio = float(w_cnt) / h_cnt
            if aspect_ratio < 0.2 or aspect_ratio > 5.0:
                continue

            # Verify contrast inside the region (watermarks have high local contrast)
            roi = gray[y:y+h_cnt, x:x+w_cnt]
            min_val, max_val, _, _ = cv2.minMaxLoc(roi)
            if (max_val - min_val) < 40:
                continue

            # Draw onto mask with a small padding to guarantee full eraser coverage
            padding = 4
            x1 = max(0, x - padding)
            y1 = max(0, y - padding)
            x2 = min(w, x + w_cnt + padding)
            y2 = min(h, y + h_cnt + padding)

            cv2.rectangle(mask, (x1, y1), (x2, y2), 255, -1)
            watermark_found = True

    # 3. Restore image via Navier-Stokes/Telea inpainting
    if watermark_found:
        restored = cv2.inpaint(img, mask, inpaintRadius=3, flags=cv2.INPAINT_TELEA)
    else:
        restored = img.copy()

    # Save output file
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
