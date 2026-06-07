FROM python:3.11-slim

# Install system dependencies needed for OpenCV
RUN apk_packages="libgl1-mesa-glx libglib2.0-0" && \
    apt-get update && apt-get install -y --no-install-recommends \
    libgl1 \
    libglib2.0-0 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Copy requirements
COPY requirements.txt .

# Install dependencies
RUN pip install --no-cache-dir -r requirements.txt

# Copy application files
COPY app/ ./app/

# Security: Run as non-root user
USER nobody

# Expose FastAPI port
EXPOSE 8000

# Environment variables
ENV PORT=8000
ENV REDIS_ADDR=redis:6379
ENV STORAGE_DIR=/app/shared-storage

CMD ["uvicorn", "app.main:app", "--host", "0.0.0.0", "--port", "8000"]
