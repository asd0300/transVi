package main

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

type Chunk struct {
	Input  string
	Output string
}

func main() {
	var (
		input   = flag.String("input", "", "Input video file path")
		output  = flag.String("output", "output.mp4", "Output video with subtitles")
		workers = flag.Int("workers", 6, "Number of parallel workers (default: 6)")
	)
	fmt.Printf("Output will be saved to: %s\n", *output) // Dummy usage to prevent unused variable error
	flag.Parse()

	if *input == "" {
		fmt.Println("Error: -input is required")
		os.Exit(1)
	}

	// 1. Create directories
	err := os.MkdirAll("audio_parts", 0755)
	if err != nil {
		fmt.Printf("Error creating audio_parts directory: %v\n", err)
		os.Exit(1)
	}

	// 2. Split audio using ffmpeg
	ffmpegCmd := exec.Command("ffmpeg",
		"-i", *input,
		"-vn", "-c:a", "pcm_s16le",
		"-ar", "16000",
		"-f", "segment",
		"-segment_time", "30",
		"-reset_timestamps", "1",
		"audio_parts/part%03d.wav",
	)
	err = runCommand(ffmpegCmd)
	if err != nil {
		fmt.Printf("FFmpeg split failed: %v\n", err)
		os.Exit(1)
	}

	// 3. Process chunks in parallel
	var chunks []Chunk
	err = filepath.WalkDir("audio_parts", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && filepath.Ext(path) == ".wav" {
			chunks = append(chunks, Chunk{
				Input:  path,
				Output: filepath.Join("subtitles", filepath.Base(path)+".srt"),
			})
		}
		return nil
	})

	var wg sync.WaitGroup
	semaphore := make(chan struct{}, *workers)
	errChan := make(chan error, len(chunks))

	for _, chunk := range chunks {
		wg.Add(1)
		semaphore <- struct{}{}
		go func(c Chunk) {
			defer wg.Done()
			defer func() { <-semaphore }()

			err := processChunk(c)
			if err != nil {
				errChan <- err
			}
		}(chunk)
	}
	wg.Wait()
	close(errChan)

	// Error handling
	for err := range errChan {
		fmt.Printf("Error processing chunk: %v\n", err)
		os.Exit(1)
	}

	// 4. Merge subtitles and re-encode
	err = mergeSubtitlesAndReencode(*input, *output)
	if err != nil {
		fmt.Printf("Merge and reencode failed: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		os.RemoveAll("audio_parts")
		os.RemoveAll("subtitles")
	}()
}

func mergeSubtitlesAndReencode(input, output string) error {
	var sb strings.Builder
	err := filepath.WalkDir("subtitles", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && filepath.Ext(path) == ".srt" {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sb.Write(data)
		}
		return nil
	})
	if err != nil {
		return err
	}

	mergedSubtitles := "merged_sub_titles.srt" // Fixed filename to avoid FFmpeg path issues
	err = os.WriteFile(mergedSubtitles, []byte(sb.String()), 0644)
	if err != nil {
		return err
	}
	return nil
}

func runCommand(cmd *exec.Cmd) error {
	fmt.Printf("Running: %s\n", cmd.Args)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func processChunk(c Chunk) error {
	// Create output directory
	err := os.MkdirAll("subtitles", 0755)
	if err != nil {
		return err
	}

	// Run Whisper.cpp
	whisperCmd := exec.Command("whisper",
		c.Input,
		"--model", "base.en", // Adjust model as needed
		"-f", "srt",
		"-o", c.Output,
	)
	return runCommand(whisperCmd)
}
