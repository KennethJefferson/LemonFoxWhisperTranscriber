# LemonFox Audio Transcriber

A concurrent Go CLI application that automatically transcribes MP3 files to SRT subtitles using the LemonFox Speech-to-Text API.

## Features

- Recursive directory scanning for MP3 files
- Concurrent processing with configurable worker pool (1-25 workers)
- Automatic SRT file generation
- Smart skipping of already-transcribed files
- Real-time progress tracking and verbose logging
- Comprehensive error handling and summary statistics
- Simple JSON config file for API key management

## Prerequisites

- Go 1.16 or higher
- LemonFox API key (get one at https://www.lemonfox.ai)

## Installation

### Build from source

```bash
go build -o lemonfox-transcriber
```

### Or run directly

```bash
go run main.go
```

## Configuration

Create a `config.json` file in the same directory as the executable (or in your current working directory):

1. Copy the example config file:
   ```bash
   cp config.example.json config.json
   ```

2. Edit `config.json` and add your API key:
   ```json
   {
     "api_key": "your_actual_lemonfox_api_key_here"
   }
   ```

**Note:** The application will first look for `config.json` in the executable's directory, then fall back to the current working directory.

## Usage

```bash
lemonfox-transcriber <directory> [workers]
```

### Arguments

- `directory` (required): Directory to recursively search for MP3 files
- `workers` (optional): Number of parallel workers (default: 1, max: 25)

### Examples

**Process with 1 worker (sequential):**
```bash
lemonfox-transcriber C:\Videos
```

**Process with 5 concurrent workers:**
```bash
lemonfox-transcriber C:\Videos 5
```

**Process current directory with 10 workers:**
```bash
lemonfox-transcriber . 10
```

## How It Works

1. **Discovery**: Recursively scans the specified directory for all `.mp3` files
2. **Filtering**: Checks each MP3 file for a corresponding `.srt` file
3. **Processing**:
   - Files without SRT files are queued for transcription
   - Files with existing SRT files are automatically skipped
4. **Transcription**: Uploads MP3 files to LemonFox API using a worker pool
5. **Output**: Saves the returned SRT content with the same base filename as the MP3

## Output

The application provides verbose output including:

- File discovery progress
- Skipped files (with existing SRT)
- Real-time worker status
- Processing progress every 2 seconds
- Success/failure notifications
- Final summary statistics

### Example Output

```
LemonFox Audio Transcriber
==========================

Scanning directory: C:\Videos
Found 15 MP3 file(s)
[SKIP] C:\Videos\episode1.mp3 (SRT already exists)
[SKIP] C:\Videos\episode2.mp3 (SRT already exists)

13 file(s) need transcription
Starting 5 worker(s)

[Worker 1] Processing (1/13): episode3.mp3
[Worker 2] Processing (2/13): episode4.mp3
[Worker 3] Processing (3/13): episode5.mp3
[Worker 4] Processing (4/13): episode6.mp3
[Worker 5] Processing (5/13): episode7.mp3
[Worker 1] SUCCESS: episode3.mp3 -> episode3.srt

[PROGRESS] 5/13 processed | Success: 5 | Failed: 0 | Elapsed: 45s

...

==========================
Processing Complete
==========================
Total files found:     15
Skipped (SRT exists):  2
Processed:             13
  - Successful:        13
  - Failed:            0
Total time:            2m15s
Average per file:      10s
==========================
```

## Error Handling

- If transcription fails, the error is logged and processing continues with other files
- Failed files are counted in the final summary
- API errors are displayed with status codes and error messages
- Network timeouts are set to 10 minutes per file

## API Details

- **Endpoint**: https://api.lemonfox.ai/v1/audio/transcriptions
- **Language**: English (fixed)
- **Response Format**: SRT (SubRip subtitle format)
- **File Size Limit**: 100MB per file (direct upload)
- **Pricing**: $0.50 per 3 hours of audio

## Limitations

- Maximum 25 concurrent workers (configurable limit)
- Files must be in MP3 format
- Requires active internet connection
- API key required for all operations

## Troubleshooting

### "Error loading config: config file not found"
Ensure `config.json` exists in either the executable's directory or your current working directory. You can copy `config.example.json` to `config.json` as a starting point.

### "Error: API key is not set in config.json"
Open `config.json` and ensure the `api_key` field is populated with your actual LemonFox API key.

### "No MP3 files found"
Verify the directory path is correct and contains MP3 files.

### "API error (status 401)"
Check that your API key in `config.json` is valid and correct.

### "API error (status 413)"
The MP3 file exceeds the 100MB upload limit. Consider splitting the file.

## License

This project is open source and available for personal and commercial use.
