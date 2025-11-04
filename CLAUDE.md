# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

LemonFox Audio Transcriber is a concurrent Go CLI application that automatically transcribes MP3 files to SRT subtitles using the LemonFox Speech-to-Text API (https://api.lemonfox.ai/v1/audio/transcriptions). The application is designed for batch processing with configurable concurrency and smart skip logic for already-transcribed files.

## Build and Run Commands

**Build executable:**
```bash
go build -o lemonfox-transcriber
```

**Run directly:**
```bash
go run main.go <directory> [workers]
```

**Example usage:**
```bash
# Single worker (sequential processing)
lemonfox-transcriber C:\Videos

# 5 concurrent workers
lemonfox-transcriber C:\Videos 5

# Current directory with 10 workers
lemonfox-transcriber . 10
```

## Configuration

The application requires a `config.json` file containing the LemonFox API key. The config loader follows this search order:
1. Executable's directory (using `os.Executable()`)
2. Current working directory (fallback)

Config format:
```json
{
  "api_key": "your_lemonfox_api_key"
}
```

## Architecture

### Core Concurrency Pattern

The application implements a **worker pool pattern** with the following key components:

1. **Job Queue**: Buffered channel (`jobs`) that holds transcription tasks
2. **Result Collector**: Buffered channel (`results`) that collects outcomes
3. **Worker Pool**: Fixed number of goroutines (1-25) processing jobs concurrently
4. **Statistics Tracker**: Thread-safe atomic counters for real-time metrics
5. **Progress Reporter**: Separate goroutine with 2-second ticker for status updates

### Processing Flow

```
Discovery → Filtering → Job Creation → Worker Pool → Results Collection → Summary
```

1. **Discovery** (`discoverMP3Files`): Recursively walks directory tree using `filepath.Walk` to find all `.mp3` files
2. **Filtering** (`hasCorrespondingSRT`): Checks for existing `.srt` files to skip already-processed audio
3. **Job Distribution**: Main goroutine pushes jobs to channel, then closes it
4. **Concurrent Processing**: Workers pull jobs from channel until exhausted
5. **Result Aggregation**: Results channel collects outcomes after `wg.Wait()`

### Key Functions

- `worker()` (main.go:286): Core worker goroutine that processes jobs from channel, calls API, and saves SRT files
- `uploadToLemonFox()` (main.go:317): Handles multipart form upload with 10-minute timeout, returns raw SRT content
- `printProgress()` (main.go:392): Displays real-time statistics using atomic loads for thread safety
- `loadConfig()` (main.go:224): Two-stage config file lookup (executable dir, then CWD)

### Thread Safety

The `Statistics` struct uses `atomic.AddInt32()` and `atomic.LoadInt32()` for counters to avoid mutex overhead. The only mutex (`mu`) is declared but currently unused, suggesting future extension points.

### API Integration

The LemonFox API expects:
- **Method**: POST with `multipart/form-data`
- **Fields**: `file` (binary), `language` (fixed: "english"), `response_format` (fixed: "srt")
- **Headers**: `Authorization: Bearer <api_key>`
- **Timeout**: 10 minutes per request
- **Response**: Plain text SRT content (not JSON)

Error handling includes both HTTP status codes and JSON error parsing fallback.

## Constants and Limits

- Default workers: 1
- Maximum workers: 25 (hardcoded limit in `maxWorkers`)
- Maximum file size: 100MB (API limit - `maxFileSizeBytes`)
- API timeout: 10 minutes per file
- Progress update interval: 2 seconds
- Response format: SRT only (hardcoded)
- Language: English only (hardcoded)

## Large File Handling (100MB+)

The application automatically handles files exceeding the 100MB API limit through an intelligent splitting and merging workflow:

### Automatic Detection and Splitting

When a worker processes a file >100MB:
1. **File Size Check**: `getFileSize()` detects files exceeding `maxFileSizeBytes` (100MB)
2. **Audio Splitting**: `splitAudioFile()` uses ffmpeg to split the audio exactly in half by duration
   - Chunk 1: `[basename]-01.mp3` (first half)
   - Chunk 2: `[basename]-02.mp3` (second half)
   - Uses `ffmpeg -c copy` for fast stream copying (no re-encoding)
3. **Size Validation**: Verifies chunks are <100MB (files >200MB will fail with clear error)

### Transcription and Merging

4. **Sequential Transcription**: Both chunks are transcribed using the LemonFox API
5. **SRT Timestamp Adjustment**:
   - `parseSRTTimestamp()` and `formatSRTTimestamp()` handle SRT time format (`HH:MM:SS,mmm`)
   - `adjustSRTTimestamps()` adds the first chunk's duration offset to all timestamps in chunk 2
   - Example: If chunk 1 is 330.5 seconds, chunk 2's timestamps are shifted by +00:05:30,500
6. **Index Renumbering**: `mergeSRTs()` renumbers subtitle indices to be sequential across both chunks
7. **Cleanup**: Temporary chunk files (`*-01.mp3`, `*-02.mp3`) are automatically deleted after successful merge

### Key Functions

- `processLargeFile()` (main.go:567): Orchestrates the entire split-transcribe-merge workflow
- `getAudioDuration()` (main.go:412): Uses ffprobe to detect audio duration in seconds
- `splitAudioFile()` (main.go:430): Creates two equal-duration chunks using ffmpeg
- `mergeSRTs()` (main.go:525): Combines SRT files with timestamp and index adjustment

### Dependencies

- **ffmpeg**: Required for audio splitting (`ffmpeg` command)
- **ffprobe**: Required for duration detection (`ffprobe` command)

Both must be available in system PATH. If not found, the application will fail with a clear error message: "ffprobe failed (is ffmpeg installed?)".

### Error Handling

- **Chunk transcription failure**: If either chunk fails, the entire file is marked as failed (no partial results)
- **File too large (>200MB)**: Clear error message indicating chunks would exceed 100MB
- **Temporary file cleanup**: Chunks are deleted via `defer` statements, ensuring cleanup even on failure

## Output Format

The application generates SRT files with the same base filename as the input MP3 file (e.g., `episode.mp3` → `episode.srt`). SRT files are saved with `0644` permissions.

For large files that were split and merged, the output SRT file is indistinguishable from a single-pass transcription, with properly sequential subtitle indices and continuous timestamps.

## Error Handling Philosophy

- Non-blocking: Failed transcriptions don't stop other workers
- Verbose logging: All errors written to `stderr` with worker ID and filename
- Final summary: Complete statistics including success/failure counts
- Graceful degradation: Missing config files provide helpful troubleshooting messages
