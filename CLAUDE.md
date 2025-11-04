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
- API timeout: 10 minutes per file
- Progress update interval: 2 seconds
- Response format: SRT only (hardcoded)
- Language: English only (hardcoded)

## Output Format

The application generates SRT files with the same base filename as the input MP3 file (e.g., `episode.mp3` → `episode.srt`). SRT files are saved with `0644` permissions.

## Error Handling Philosophy

- Non-blocking: Failed transcriptions don't stop other workers
- Verbose logging: All errors written to `stderr` with worker ID and filename
- Final summary: Complete statistics including success/failure counts
- Graceful degradation: Missing config files provide helpful troubleshooting messages
