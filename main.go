package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
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
)

// Job represents a transcription job
type Job struct {
	FilePath string
	Index    int
}

// Result represents the outcome of a transcription job
type Result struct {
	FilePath string
	Success  bool
	Error    error
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
	fmt.Printf("Starting %d worker(s)\n\n", workers)

	// Create job and result channels
	jobs := make(chan Job, stats.Total)
	results := make(chan Result, stats.Total)

	// Start worker pool
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go worker(i+1, jobs, results, apiKey, stats, &wg)
	}

	// Send jobs to workers
	for i, filePath := range filesToProcess {
		jobs <- Job{
			FilePath: filePath,
			Index:    i + 1,
		}
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

	// Collect results
	for result := range results {
		if result.Success {
			atomic.AddInt32(&stats.Success, 1)
		} else {
			atomic.AddInt32(&stats.Failed, 1)
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
		fmt.Printf("[Worker %d] Processing (%d/%d): %s\n", id, job.Index, stats.Total, filepath.Base(job.FilePath))

		srtContent, err := uploadToLemonFox(job.FilePath, apiKey)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[Worker %d] ERROR: Failed to transcribe %s: %v\n", id, filepath.Base(job.FilePath), err)
			results <- Result{FilePath: job.FilePath, Success: false, Error: err}
			atomic.AddInt32(&stats.Processed, 1)
			continue
		}

		// Save SRT file
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
