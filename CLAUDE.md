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
Discovery → Filtering → Pre-Processing (Split) → Job Creation → Worker Pool → Post-Processing (Merge) → Summary
```

**Phase 1: Discovery & Pre-Processing**
1. **Discovery** (`discoverMP3Files`): Recursively walks directory tree using `filepath.Walk` to find all `.mp3` files
2. **Filtering** (`hasCorrespondingSRT`): Checks for existing `.srt` files to skip already-processed audio
3. **File Size Check**: Each file is checked against 100MB limit
4. **Large File Splitting**: Files >100MB are split into chunks using ffmpeg BEFORE workers start
5. **Job Creation**: Creates either single jobs (normal files) or chunk jobs (split files)

**Phase 2: Concurrent Processing**
6. **Job Distribution**: Main goroutine pushes jobs to channel, then closes it
7. **Worker Pool Execution**: Workers pull jobs from channel and process in parallel
8. **Chunk Jobs**: Workers transcribe chunks and return SRT content (don't save to disk yet)
9. **Normal Jobs**: Workers transcribe and save SRT files immediately

**Phase 3: Post-Processing**
10. **Result Collection**: Main thread waits for all workers (`wg.Wait()`), then collects results
11. **Chunk Merging**: For chunk results, merge SRT content with timestamp/index adjustment
12. **Cleanup**: Delete temporary chunk MP3 files
13. **Summary**: Display final statistics

### Data Structures

**Job**
```go
type Job struct {
    FilePath     string  // Path to MP3 file (or chunk)
    Index        int     // Sequential job number
    IsChunk      bool    // True if this is a chunk job
    ChunkNumber  int     // 1 or 2 for chunk jobs
    OriginalFile string  // Path to original file (for chunks)
}
```

**Result**
```go
type Result struct {
    FilePath    string  // Path to processed file
    Success     bool    // Transcription success/failure
    Error       error   // Error if failed
    IsChunk     bool    // True if this is a chunk result
    ChunkNumber int     // 1 or 2 for chunks
    SRTContent  string  // SRT text (for chunks only)
}
```

**ChunkGroup**
```go
type ChunkGroup struct {
    OriginalFile   string      // Original MP3 file path
    Chunk1Path     string      // Path to first chunk
    Chunk2Path     string      // Path to second chunk
    Chunk1Duration float64     // Duration of first chunk (for timestamp offset)
    Chunk1SRT      string      // SRT content from chunk 1
    Chunk2SRT      string      // SRT content from chunk 2
    Chunk1Done     bool        // Chunk 1 transcription complete
    Chunk2Done     bool        // Chunk 2 transcription complete
    mu             sync.Mutex  // Protects concurrent updates
}
```

**Statistics**
```go
type Statistics struct {
    Total     int         // Files to process
    Processed int32       // Completed jobs (atomic)
    Success   int32       // Successful transcriptions (atomic)
    Failed    int32       // Failed transcriptions (atomic)
    Skipped   int32       // Already had SRT (atomic)
    StartTime time.Time   // For elapsed time calculation
}
```

### Key Functions

**Core Processing:**
- `worker()`: Core worker goroutine that processes jobs from channel, handles both chunk and normal jobs
- `uploadToLemonFox()`: Handles multipart form upload with 10-minute timeout, returns raw SRT content

**Large File Handling:**
- `getFileSize()`: Returns file size in bytes for 100MB threshold check
- `getAudioDuration()`: Uses ffprobe to detect audio duration in seconds
- `splitAudioFile()`: Creates two equal-duration chunks using ffmpeg with stream copy
- `parseSRTTimestamp()`: Converts SRT timestamp format to seconds (float64)
- `formatSRTTimestamp()`: Converts seconds back to SRT timestamp format
- `adjustSRTTimestamps()`: Adds time offset to all timestamps in an SRT string
- `mergeSRTs()`: Combines two SRT files with timestamp adjustment and subtitle index renumbering

**Utilities:**
- `discoverMP3Files()`: Recursively finds all MP3 files in directory tree
- `hasCorrespondingSRT()`: Checks if SRT file already exists for an MP3
- `loadConfig()`: Two-stage config file lookup (executable dir, then CWD)
- `printProgress()`: Displays real-time statistics using atomic loads for thread safety
- `saveSRT()`: Writes SRT content to file with 0644 permissions

### Thread Safety

**Statistics Tracking:**
- The `Statistics` struct uses `atomic.AddInt32()` and `atomic.LoadInt32()` for counters to avoid mutex overhead
- Atomic operations ensure thread-safe updates from multiple workers without locks

**Chunk Result Coordination:**
- `ChunkGroup` struct uses `sync.Mutex` to protect chunk result updates
- Each chunk group tracks completion status and SRT content for both chunks
- Mutex ensures safe concurrent access when workers complete chunk jobs
- Map of chunk groups (`chunkGroups`) is only accessed from main thread (no concurrent access)

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

### Pre-Processing Phase (Before Workers Start)

During job creation, the application checks file sizes and splits large files:
1. **File Size Check**: `getFileSize()` detects files exceeding `maxFileSizeBytes` (100MB)
2. **Audio Splitting**: `splitAudioFile()` uses ffmpeg to split the audio exactly in half by duration
   - Chunk 1: `[basename]-01.mp3` (first half)
   - Chunk 2: `[basename]-02.mp3` (second half)
   - Uses `ffmpeg -c copy` for fast stream copying (no re-encoding)
   - Splitting happens in the main thread BEFORE workers start processing
3. **Size Validation**: Verifies chunks are <100MB (files >200MB will fail with clear error)
4. **Job Creation**: Creates two chunk jobs (one for each chunk) instead of a single file job

### Worker Processing

5. **Parallel Transcription**: Workers pick up chunk jobs from the queue and transcribe them normally
6. **Chunk Results**: Workers return SRT content without saving (for chunks only)
7. **Result Tracking**: Main thread tracks chunk results using `ChunkGroup` structures

### Post-Processing and Merging

8. **Result Collection**: After all workers complete, chunk results are collected
9. **SRT Timestamp Adjustment**:
   - `parseSRTTimestamp()` and `formatSRTTimestamp()` handle SRT time format (`HH:MM:SS,mmm`)
   - `adjustSRTTimestamps()` adds the first chunk's duration offset to all timestamps in chunk 2
   - Example: If chunk 1 is 330.5 seconds, chunk 2's timestamps are shifted by +00:05:30,500
10. **Index Renumbering**: `mergeSRTs()` renumbers subtitle indices to be sequential across both chunks
11. **Save Merged SRT**: Final merged SRT is saved to disk
12. **Cleanup**: Temporary chunk files (`*-01.mp3`, `*-02.mp3`) are automatically deleted after merging

### Workflow Summary

**Pre-Processing (Main Thread):**
- Check file sizes, split large files into chunks
- Create `ChunkGroup` tracking structures for each split file
- Generate job list with chunk jobs or normal jobs

**Processing (Worker Pool):**
- Workers transcribe files/chunks in parallel
- Chunk jobs return SRT content without saving
- Normal jobs save SRT files immediately

**Post-Processing (Main Thread):**
- Collect results, detect when both chunks of a file complete
- Merge chunk SRTs with timestamp/index adjustment
- Save merged SRT, cleanup temporary chunk files

### Dependencies

- **ffmpeg**: Required for audio splitting (`ffmpeg` command)
- **ffprobe**: Required for duration detection (`ffprobe` command)

Both must be available in system PATH. If not found, the application will fail with a clear error message: "ffprobe failed (is ffmpeg installed?)".

### Error Handling

- **Chunk transcription failure**: If either chunk fails, the entire file is marked as failed (no partial results)
- **File too large (>200MB)**: Clear error message indicating chunks would exceed 100MB
- **Temporary file cleanup**: Chunks are deleted after merging (success or failure)
- **Split failure**: If ffmpeg fails during splitting, the file is marked failed and skipped

### Performance Benefits

**Why Split Before Worker Pool:**
1. **No Worker Blocking**: File splitting happens before workers start, so workers spend 100% of time on API calls
2. **Parallel Chunk Processing**: With multiple workers, both chunks can be transcribed simultaneously
3. **Better Resource Utilization**: Splitting uses CPU (ffmpeg), transcription uses network I/O - no blocking
4. **Clear Progress Indication**: User sees splitting progress immediately, then chunk processing

**Example Timeline (150MB file, 2 workers):**
- **Old approach**: Worker blocks 45s splitting, then 2 minutes transcribing chunk 1, then 2 minutes chunk 2 = ~5 minutes total
- **New approach**: Main thread splits 45s, then both chunks transcribe in parallel 2 minutes = ~3 minutes total (40% faster)

## Output Format

The application generates SRT files with the same base filename as the input MP3 file (e.g., `episode.mp3` → `episode.srt`). SRT files are saved with `0644` permissions.

For large files that were split and merged, the output SRT file is indistinguishable from a single-pass transcription, with properly sequential subtitle indices and continuous timestamps.

### Console Output Examples

**Normal file (<100MB):**
```
[Worker 1] Processing (1): small-audio.mp3
[Worker 1] SUCCESS: small-audio.mp3 -> small-audio.srt
```

**Large file (>100MB) - Pre-processing:**
```
Checking file sizes and splitting large files...
[SPLIT] large-audio.mp3 (150.2 MB) - splitting into chunks...
[SPLIT] Created chunks: large-audio-01.mp3, large-audio-02.mp3
Starting 5 worker(s) to process 2 job(s)
```

**Large file - Worker processing:**
```
[Worker 1] Processing chunk 1/2: large-audio-01.mp3
[Worker 2] Processing chunk 2/2: large-audio-02.mp3
[Worker 1] SUCCESS (chunk 1/2): large-audio-01.mp3
[Worker 2] SUCCESS (chunk 2/2): large-audio-02.mp3
```

**Large file - Post-processing:**
```
[MERGE] Combining chunks for: large-audio.mp3
[SUCCESS] Merged: large-audio.mp3 -> large-audio.srt
```

## Error Handling Philosophy

- Non-blocking: Failed transcriptions don't stop other workers
- Verbose logging: All errors written to `stderr` with worker ID and filename
- Final summary: Complete statistics including success/failure counts
- Graceful degradation: Missing config files provide helpful troubleshooting messages
