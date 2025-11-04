package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	apiURL           = "https://api.lemonfox.ai/v1/audio/transcriptions"
	defaultWorkers   = 1
	maxWorkers       = 25
	configFile       = "config.json"
	language         = "english"
	responseFormat   = "srt"
	maxFileSizeBytes = 100 * 1024 * 1024 // 100MB API limit
)

// Job represents a transcription job
type Job struct {
	FilePath    string
	Index       int
	IsChunk     bool   // True if this job is for a file chunk
	ChunkNumber int    // 1-based chunk number (1 or 2)
	OriginalFile string // Path to original file (for chunk jobs)
}

// ChunkGroup tracks chunks that need to be merged
type ChunkGroup struct {
	OriginalFile   string
	Chunk1Path     string
	Chunk2Path     string
	Chunk1Duration float64
	Chunk1SRT      string
	Chunk2SRT      string
	Chunk1Done     bool
	Chunk2Done     bool
	mu             sync.Mutex
}

// Result represents the outcome of a transcription job
type Result struct {
	FilePath    string
	Success     bool
	Error       error
	IsChunk     bool   // True if this result is for a chunk
	ChunkNumber int    // Chunk number (1 or 2)
	SRTContent  string // SRT content (for chunk results, used in merging)
}

// Statistics tracks processing metrics
type Statistics struct {
	Total     int
	Processed int32
	Success   int32
	Failed    int32
	Skipped   int32
	StartTime time.Time
	mu        sync.Mutex
}

// Config represents the application configuration
type Config struct {
	APIKey string `json:"api_key"`
}

func main() {
	// Parse command-line arguments
	dirPath, workers, err := parseArgs()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		printUsage()
		os.Exit(1)
	}

	// Load configuration
	config, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		fmt.Fprintf(os.Stderr, "Please ensure %s exists with your API key\n", configFile)
		os.Exit(1)
	}

	if config.APIKey == "" {
		fmt.Fprintf(os.Stderr, "Error: API key is not set in %s\n", configFile)
		os.Exit(1)
	}

	apiKey := config.APIKey

	fmt.Printf("LemonFox Audio Transcriber\n")
	fmt.Printf("==========================\n\n")

	// Discover MP3 files
	fmt.Printf("Scanning directory: %s\n", dirPath)
	allMP3Files, err := discoverMP3Files(dirPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error scanning directory: %v\n", err)
		os.Exit(1)
	}

	if len(allMP3Files) == 0 {
		fmt.Println("No MP3 files found in the specified directory.")
		os.Exit(0)
	}

	fmt.Printf("Found %d MP3 file(s)\n", len(allMP3Files))

	// Filter files that need transcription
	stats := &Statistics{
		StartTime: time.Now(),
	}

	var filesToProcess []string
	for _, mp3File := range allMP3Files {
		if hasCorrespondingSRT(mp3File) {
			atomic.AddInt32(&stats.Skipped, 1)
			fmt.Printf("[SKIP] %s (SRT already exists)\n", mp3File)
		} else {
			filesToProcess = append(filesToProcess, mp3File)
		}
	}

	stats.Total = len(filesToProcess)
	if stats.Total == 0 {
		fmt.Println("\nAll MP3 files already have corresponding SRT files. Nothing to process.")
		os.Exit(0)
	}

	fmt.Printf("\n%d file(s) need transcription\n", stats.Total)

	// Pre-process: split large files and create jobs
	fmt.Println("Checking file sizes and splitting large files...")
	var jobList []Job
	chunkGroups := make(map[string]*ChunkGroup) // Map chunk file path to its group
	jobIndex := 0

	for _, filePath := range filesToProcess {
		fileSize, err := getFileSize(filePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[ERROR] Failed to check size of %s: %v (skipping)\n", filepath.Base(filePath), err)
			atomic.AddInt32(&stats.Failed, 1)
			stats.Total--
			continue
		}

		if fileSize > maxFileSizeBytes {
			// File exceeds 100MB - split it now
			fmt.Printf("[SPLIT] %s (%.1f MB) - splitting into chunks...\n", filepath.Base(filePath), float64(fileSize)/(1024*1024))

			chunk1Path, chunk2Path, chunk1Duration, err := splitAudioFile(filePath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[ERROR] Failed to split %s: %v (skipping)\n", filepath.Base(filePath), err)
				atomic.AddInt32(&stats.Failed, 1)
				stats.Total--
				continue
			}

			// Validate chunk sizes
			chunk1Size, _ := getFileSize(chunk1Path)
			chunk2Size, _ := getFileSize(chunk2Path)
			if chunk1Size > maxFileSizeBytes || chunk2Size > maxFileSizeBytes {
				fmt.Fprintf(os.Stderr, "[ERROR] %s: chunks still exceed 100MB (file >200MB, skipping)\n", filepath.Base(filePath))
				os.Remove(chunk1Path)
				os.Remove(chunk2Path)
				atomic.AddInt32(&stats.Failed, 1)
				stats.Total--
				continue
			}

			// Create chunk group for tracking
			group := &ChunkGroup{
				OriginalFile:   filePath,
				Chunk1Path:     chunk1Path,
				Chunk2Path:     chunk2Path,
				Chunk1Duration: chunk1Duration,
			}
			chunkGroups[chunk1Path] = group
			chunkGroups[chunk2Path] = group

			// Create jobs for both chunks
			jobIndex++
			jobList = append(jobList, Job{
				FilePath:     chunk1Path,
				Index:        jobIndex,
				IsChunk:      true,
				ChunkNumber:  1,
				OriginalFile: filePath,
			})

			jobIndex++
			jobList = append(jobList, Job{
				FilePath:     chunk2Path,
				Index:        jobIndex,
				IsChunk:      true,
				ChunkNumber:  2,
				OriginalFile: filePath,
			})

			fmt.Printf("[SPLIT] Created chunks: %s, %s\n", filepath.Base(chunk1Path), filepath.Base(chunk2Path))
		} else {
			// Normal file - create single job
			jobIndex++
			jobList = append(jobList, Job{
				FilePath: filePath,
				Index:    jobIndex,
				IsChunk:  false,
			})
		}
	}

	if len(jobList) == 0 {
		fmt.Println("\nNo files to process after pre-processing.")
		os.Exit(0)
	}

	fmt.Printf("Starting %d worker(s) to process %d job(s)\n\n", workers, len(jobList))

	// Create job and result channels
	jobs := make(chan Job, len(jobList))
	results := make(chan Result, len(jobList))

	// Start worker pool
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go worker(i+1, jobs, results, apiKey, stats, &wg)
	}

	// Send jobs to workers
	for _, job := range jobList {
		jobs <- job
	}
	close(jobs)

	// Start progress reporter in a separate goroutine
	done := make(chan bool)
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				printProgress(stats)
			case <-done:
				return
			}
		}
	}()

	// Wait for all workers to complete
	wg.Wait()
	close(results)
	done <- true

	// Collect results and merge chunks
	for result := range results {
		if result.IsChunk {
			// Handle chunk result
			group := chunkGroups[result.FilePath]
			if group == nil {
				fmt.Fprintf(os.Stderr, "[ERROR] Chunk result for unknown file: %s\n", result.FilePath)
				continue
			}

			group.mu.Lock()
			if result.ChunkNumber == 1 {
				group.Chunk1Done = true
				if result.Success {
					group.Chunk1SRT = result.SRTContent
				}
			} else if result.ChunkNumber == 2 {
				group.Chunk2Done = true
				if result.Success {
					group.Chunk2SRT = result.SRTContent
				}
			}

			// Check if both chunks are complete
			bothDone := group.Chunk1Done && group.Chunk2Done
			bothSuccess := group.Chunk1SRT != "" && group.Chunk2SRT != ""
			group.mu.Unlock()

			if bothDone {
				// Both chunks processed - merge or fail
				if bothSuccess {
					// Merge SRTs
					fmt.Printf("[MERGE] Combining chunks for: %s\n", filepath.Base(group.OriginalFile))
					mergedSRT, err := mergeSRTs(group.Chunk1SRT, group.Chunk2SRT, group.Chunk1Duration)
					if err != nil {
						fmt.Fprintf(os.Stderr, "[ERROR] Failed to merge SRT for %s: %v\n", filepath.Base(group.OriginalFile), err)
						atomic.AddInt32(&stats.Failed, 1)
					} else {
						// Save merged SRT
						srtPath := strings.TrimSuffix(group.OriginalFile, filepath.Ext(group.OriginalFile)) + ".srt"
						err = saveSRT(srtPath, mergedSRT)
						if err != nil {
							fmt.Fprintf(os.Stderr, "[ERROR] Failed to save merged SRT for %s: %v\n", filepath.Base(group.OriginalFile), err)
							atomic.AddInt32(&stats.Failed, 1)
						} else {
							fmt.Printf("[SUCCESS] Merged: %s -> %s\n", filepath.Base(group.OriginalFile), filepath.Base(srtPath))
							atomic.AddInt32(&stats.Success, 1)
						}
					}
				} else {
					// At least one chunk failed
					fmt.Fprintf(os.Stderr, "[ERROR] One or more chunks failed for: %s\n", filepath.Base(group.OriginalFile))
					atomic.AddInt32(&stats.Failed, 1)
				}

				// Cleanup chunk files
				os.Remove(group.Chunk1Path)
				os.Remove(group.Chunk2Path)
			}
		} else {
			// Normal file result
			if result.Success {
				atomic.AddInt32(&stats.Success, 1)
			} else {
				atomic.AddInt32(&stats.Failed, 1)
			}
		}
	}

	// Print final summary
	printSummary(stats)
}

// parseArgs parses and validates command-line arguments
func parseArgs() (string, int, error) {
	if len(os.Args) < 2 {
		return "", 0, fmt.Errorf("missing required argument: directory path")
	}

	dirPath := os.Args[1]

	// Check if directory exists
	info, err := os.Stat(dirPath)
	if err != nil {
		return "", 0, fmt.Errorf("invalid directory path: %v", err)
	}
	if !info.IsDir() {
		return "", 0, fmt.Errorf("path is not a directory: %s", dirPath)
	}

	// Parse worker count
	workers := defaultWorkers
	if len(os.Args) >= 3 {
		w, err := strconv.Atoi(os.Args[2])
		if err != nil {
			return "", 0, fmt.Errorf("invalid worker count: %v", err)
		}
		if w < 1 {
			return "", 0, fmt.Errorf("worker count must be at least 1")
		}
		if w > maxWorkers {
			return "", 0, fmt.Errorf("worker count cannot exceed %d", maxWorkers)
		}
		workers = w
	}

	return dirPath, workers, nil
}

// printUsage prints usage information
func printUsage() {
	fmt.Println("Usage: lemonfox-transcriber <directory> [workers]")
	fmt.Println()
	fmt.Println("Arguments:")
	fmt.Println("  directory    Directory to recursively search for MP3 files")
	fmt.Printf("  workers      Number of parallel workers (default: %d, max: %d)\n", defaultWorkers, maxWorkers)
	fmt.Println()
	fmt.Printf("Configuration:\n")
	fmt.Printf("  Requires %s file with your LemonFox API key\n", configFile)
}

// loadConfig loads the configuration from the config file
func loadConfig() (*Config, error) {
	// Get the executable directory
	exePath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("failed to get executable path: %v", err)
	}
	exeDir := filepath.Dir(exePath)
	configPath := filepath.Join(exeDir, configFile)

	// Check if config file exists
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		// Try current working directory as fallback
		configPath = configFile
		if _, err := os.Stat(configPath); os.IsNotExist(err) {
			return nil, fmt.Errorf("config file not found (looked in executable directory and current directory)")
		}
	}

	// Read config file
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %v", err)
	}

	// Parse JSON
	var config Config
	err = json.Unmarshal(data, &config)
	if err != nil {
		return nil, fmt.Errorf("failed to parse config file: %v", err)
	}

	return &config, nil
}

// discoverMP3Files recursively finds all MP3 files in a directory
func discoverMP3Files(rootPath string) ([]string, error) {
	var mp3Files []string

	err := filepath.Walk(rootPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if !info.IsDir() && strings.ToLower(filepath.Ext(path)) == ".mp3" {
			mp3Files = append(mp3Files, path)
		}

		return nil
	})

	return mp3Files, err
}

// hasCorrespondingSRT checks if an SRT file exists for the given MP3 file
func hasCorrespondingSRT(mp3Path string) bool {
	srtPath := strings.TrimSuffix(mp3Path, filepath.Ext(mp3Path)) + ".srt"
	_, err := os.Stat(srtPath)
	return err == nil
}

// worker processes transcription jobs
func worker(id int, jobs <-chan Job, results chan<- Result, apiKey string, stats *Statistics, wg *sync.WaitGroup) {
	defer wg.Done()

	for job := range jobs {
		if job.IsChunk {
			// Process chunk job
			fmt.Printf("[Worker %d] Processing chunk %d/2: %s\n", id, job.ChunkNumber, filepath.Base(job.FilePath))
		} else {
			// Normal job
			fmt.Printf("[Worker %d] Processing (%d): %s\n", id, job.Index, filepath.Base(job.FilePath))
		}

		srtContent, err := uploadToLemonFox(job.FilePath, apiKey)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[Worker %d] ERROR: Failed to transcribe %s: %v\n", id, filepath.Base(job.FilePath), err)
			results <- Result{
				FilePath:    job.FilePath,
				Success:     false,
				Error:       err,
				IsChunk:     job.IsChunk,
				ChunkNumber: job.ChunkNumber,
			}
			atomic.AddInt32(&stats.Processed, 1)
			continue
		}

		if job.IsChunk {
			// Chunk job - don't save yet, return SRT content for merging
			fmt.Printf("[Worker %d] SUCCESS (chunk %d/2): %s\n", id, job.ChunkNumber, filepath.Base(job.FilePath))
			results <- Result{
				FilePath:    job.FilePath,
				Success:     true,
				IsChunk:     true,
				ChunkNumber: job.ChunkNumber,
				SRTContent:  srtContent,
			}
		} else {
			// Normal job - save SRT immediately
			srtPath := strings.TrimSuffix(job.FilePath, filepath.Ext(job.FilePath)) + ".srt"
			err = saveSRT(srtPath, srtContent)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[Worker %d] ERROR: Failed to save SRT for %s: %v\n", id, filepath.Base(job.FilePath), err)
				results <- Result{FilePath: job.FilePath, Success: false, Error: err}
				atomic.AddInt32(&stats.Processed, 1)
				continue
			}

			fmt.Printf("[Worker %d] SUCCESS: %s -> %s\n", id, filepath.Base(job.FilePath), filepath.Base(srtPath))
			results <- Result{FilePath: job.FilePath, Success: true}
		}

		atomic.AddInt32(&stats.Processed, 1)
	}
}

// uploadToLemonFox uploads an MP3 file to the LemonFox API and returns the SRT content
func uploadToLemonFox(filePath string, apiKey string) (string, error) {
	// Open the file
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to open file: %v", err)
	}
	defer file.Close()

	// Create multipart form
	var requestBody bytes.Buffer
	writer := multipart.NewWriter(&requestBody)

	// Add file field
	part, err := writer.CreateFormFile("file", filepath.Base(filePath))
	if err != nil {
		return "", fmt.Errorf("failed to create form file: %v", err)
	}
	_, err = io.Copy(part, file)
	if err != nil {
		return "", fmt.Errorf("failed to copy file: %v", err)
	}

	// Add language field
	_ = writer.WriteField("language", language)

	// Add response_format field
	_ = writer.WriteField("response_format", responseFormat)

	err = writer.Close()
	if err != nil {
		return "", fmt.Errorf("failed to close writer: %v", err)
	}

	// Create HTTP request
	req, err := http.NewRequest("POST", apiURL, &requestBody)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %v", err)
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+apiKey)

	// Send request
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request: %v", err)
	}
	defer resp.Body.Close()

	// Read response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %v", err)
	}

	// Check for HTTP errors
	if resp.StatusCode != http.StatusOK {
		// Try to parse error as JSON
		var errorResp map[string]interface{}
		if json.Unmarshal(body, &errorResp) == nil {
			return "", fmt.Errorf("API error (status %d): %v", resp.StatusCode, errorResp)
		}
		return "", fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(body))
	}

	return string(body), nil
}

// saveSRT saves the SRT content to a file
func saveSRT(filePath string, content string) error {
	return os.WriteFile(filePath, []byte(content), 0644)
}

// getFileSize returns the size of a file in bytes
func getFileSize(filePath string) (int64, error) {
	info, err := os.Stat(filePath)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// getAudioDuration uses ffmpeg to get the duration of an audio file in seconds
func getAudioDuration(filePath string) (float64, error) {
	// Run ffprobe to get duration
	cmd := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", filePath)
	output, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe failed (is ffmpeg installed?): %v", err)
	}

	// Parse duration
	durationStr := strings.TrimSpace(string(output))
	duration, err := strconv.ParseFloat(durationStr, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse duration: %v", err)
	}

	return duration, nil
}

// splitAudioFile splits an audio file into two halves using ffmpeg
// Returns paths to chunk files and duration of first chunk
func splitAudioFile(filePath string) (chunk1Path, chunk2Path string, chunk1Duration float64, err error) {
	// Get total duration
	totalDuration, err := getAudioDuration(filePath)
	if err != nil {
		return "", "", 0, err
	}

	// Calculate half duration
	halfDuration := totalDuration / 2.0

	// Generate chunk file paths
	dir := filepath.Dir(filePath)
	baseName := strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath))
	chunk1Path = filepath.Join(dir, baseName+"-01.mp3")
	chunk2Path = filepath.Join(dir, baseName+"-02.mp3")

	// Split first half (from start to half duration)
	cmd1 := exec.Command("ffmpeg", "-i", filePath, "-ss", "0", "-t", fmt.Sprintf("%.3f", halfDuration), "-c", "copy", "-y", chunk1Path)
	if err := cmd1.Run(); err != nil {
		return "", "", 0, fmt.Errorf("failed to create first chunk: %v", err)
	}

	// Split second half (from half duration to end)
	cmd2 := exec.Command("ffmpeg", "-i", filePath, "-ss", fmt.Sprintf("%.3f", halfDuration), "-c", "copy", "-y", chunk2Path)
	if err := cmd2.Run(); err != nil {
		// Clean up first chunk on failure
		os.Remove(chunk1Path)
		return "", "", 0, fmt.Errorf("failed to create second chunk: %v", err)
	}

	return chunk1Path, chunk2Path, halfDuration, nil
}

// parseSRTTimestamp converts SRT timestamp (HH:MM:SS,mmm) to seconds
func parseSRTTimestamp(timestamp string) (float64, error) {
	// SRT format: HH:MM:SS,mmm
	re := regexp.MustCompile(`(\d+):(\d+):(\d+),(\d+)`)
	matches := re.FindStringSubmatch(timestamp)
	if len(matches) != 5 {
		return 0, fmt.Errorf("invalid SRT timestamp format: %s", timestamp)
	}

	hours, _ := strconv.Atoi(matches[1])
	minutes, _ := strconv.Atoi(matches[2])
	seconds, _ := strconv.Atoi(matches[3])
	milliseconds, _ := strconv.Atoi(matches[4])

	totalSeconds := float64(hours)*3600 + float64(minutes)*60 + float64(seconds) + float64(milliseconds)/1000.0
	return totalSeconds, nil
}

// formatSRTTimestamp converts seconds to SRT timestamp format (HH:MM:SS,mmm)
func formatSRTTimestamp(seconds float64) string {
	hours := int(seconds / 3600)
	minutes := int((seconds - float64(hours)*3600) / 60)
	secs := int(seconds - float64(hours)*3600 - float64(minutes)*60)
	milliseconds := int((seconds - float64(int(seconds))) * 1000)

	return fmt.Sprintf("%02d:%02d:%02d,%03d", hours, minutes, secs, milliseconds)
}

// adjustSRTTimestamps adds an offset to all timestamps in an SRT string
func adjustSRTTimestamps(srtContent string, offsetSeconds float64) (string, error) {
	// Regex to match SRT timestamp lines (e.g., "00:00:10,500 --> 00:00:15,200")
	re := regexp.MustCompile(`(\d+:\d+:\d+,\d+) --> (\d+:\d+:\d+,\d+)`)

	result := re.ReplaceAllStringFunc(srtContent, func(match string) string {
		parts := strings.Split(match, " --> ")
		if len(parts) != 2 {
			return match // Skip malformed lines
		}

		// Parse and adjust start timestamp
		startTime, err := parseSRTTimestamp(parts[0])
		if err != nil {
			return match
		}
		newStart := formatSRTTimestamp(startTime + offsetSeconds)

		// Parse and adjust end timestamp
		endTime, err := parseSRTTimestamp(parts[1])
		if err != nil {
			return match
		}
		newEnd := formatSRTTimestamp(endTime + offsetSeconds)

		return newStart + " --> " + newEnd
	})

	return result, nil
}

// mergeSRTs combines two SRT files, adjusting timestamps and indices for the second chunk
func mergeSRTs(srt1Content, srt2Content string, offsetSeconds float64) (string, error) {
	// Parse first SRT to find the last subtitle index
	lines1 := strings.Split(strings.TrimSpace(srt1Content), "\n")
	lastIndex := 0
	for _, line := range lines1 {
		line = strings.TrimSpace(line)
		if idx, err := strconv.Atoi(line); err == nil && idx > lastIndex {
			lastIndex = idx
		}
	}

	// Adjust timestamps in second SRT
	adjustedSRT2, err := adjustSRTTimestamps(srt2Content, offsetSeconds)
	if err != nil {
		return "", fmt.Errorf("failed to adjust timestamps: %v", err)
	}

	// Renumber indices in second SRT
	lines2 := strings.Split(strings.TrimSpace(adjustedSRT2), "\n")
	var renumberedLines []string
	currentIndex := lastIndex

	for i := 0; i < len(lines2); i++ {
		line := strings.TrimSpace(lines2[i])

		// Check if this line is a subtitle index (numeric line at start of block)
		if idx, err := strconv.Atoi(line); err == nil && idx > 0 {
			// This is a subtitle index - renumber it
			currentIndex++
			renumberedLines = append(renumberedLines, fmt.Sprintf("%d", currentIndex))
		} else {
			// Keep the line as-is
			renumberedLines = append(renumberedLines, lines2[i])
		}
	}

	// Combine both SRTs
	merged := strings.TrimSpace(srt1Content) + "\n\n" + strings.Join(renumberedLines, "\n")
	return merged, nil
}

// printProgress prints the current processing progress
func printProgress(stats *Statistics) {
	processed := atomic.LoadInt32(&stats.Processed)
	success := atomic.LoadInt32(&stats.Success)
	failed := atomic.LoadInt32(&stats.Failed)

	elapsed := time.Since(stats.StartTime)
	fmt.Printf("\n[PROGRESS] %d/%d processed | Success: %d | Failed: %d | Elapsed: %s\n\n",
		processed, stats.Total, success, failed, elapsed.Round(time.Second))
}

// printSummary prints the final processing summary
func printSummary(stats *Statistics) {
	elapsed := time.Since(stats.StartTime)

	fmt.Printf("\n")
	fmt.Printf("==========================\n")
	fmt.Printf("Processing Complete\n")
	fmt.Printf("==========================\n")
	fmt.Printf("Total files found:     %d\n", stats.Total+int(atomic.LoadInt32(&stats.Skipped)))
	fmt.Printf("Skipped (SRT exists):  %d\n", atomic.LoadInt32(&stats.Skipped))
	fmt.Printf("Processed:             %d\n", atomic.LoadInt32(&stats.Processed))
	fmt.Printf("  - Successful:        %d\n", atomic.LoadInt32(&stats.Success))
	fmt.Printf("  - Failed:            %d\n", atomic.LoadInt32(&stats.Failed))
	fmt.Printf("Total time:            %s\n", elapsed.Round(time.Second))

	if stats.Total > 0 && elapsed > 0 {
		avgTime := elapsed / time.Duration(atomic.LoadInt32(&stats.Processed))
		fmt.Printf("Average per file:      %s\n", avgTime.Round(time.Second))
	}
	fmt.Printf("==========================\n")
}
