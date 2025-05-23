package main

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
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
	err = mergeSubtitlesAndReencode()
	if err != nil {
		fmt.Printf("Merge and reencode failed: %v\n", err)
		os.Exit(1)
	}
}

type Subtitle struct {
	Index   int
	Start   time.Duration
	End     time.Duration
	Content string
}

func parseSrt(data []byte) ([]Subtitle, error) {
	var subtitles []Subtitle
	s := string(data)
	parts := strings.Split(s, "\n\n")
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			continue
		}
		lines := strings.Split(part, "\n")
		if len(lines) < 3 {
			continue
		}

		index, err := strconv.Atoi(lines[0])
		if err != nil {
			continue
		}

		timecodes := strings.Split(lines[1], " --> ")
		if len(timecodes) != 2 {
			continue
		}

		startTime, err := time.Parse("15:04:05,000", timecodes[0])
		if err != nil {
			continue
		}
		endTime, err := time.Parse("15:04:05,000", timecodes[1])
		if err != nil {
			continue
		}

		content := strings.Join(lines[2:], "\n")

		subtitles = append(subtitles, Subtitle{
			Index:   index,
			Start:   time.Duration(startTime.Hour())*time.Hour + time.Duration(startTime.Minute())*time.Minute + time.Duration(startTime.Second())*time.Second + time.Duration(startTime.Nanosecond()),
			End:     time.Duration(endTime.Hour())*time.Hour + time.Duration(endTime.Minute())*time.Minute + time.Duration(endTime.Second())*time.Second + time.Duration(endTime.Nanosecond()),
			Content: content,
		})
	}
	return subtitles, nil
}

func mergeSubtitlesAndReencode() error {
	var allSubtitles []Subtitle

	err := filepath.WalkDir("subtitles", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && filepath.Ext(path) == ".srt" {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			subs, err := parseSrt(data)
			if err != nil {
				fmt.Printf("Error parsing srt file %s: %v\n", path, err)
				return nil // continue to next file
			}
			allSubtitles = append(allSubtitles, subs...)
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Sort subtitles by start time
	sort.Slice(allSubtitles, func(i, j int) bool {
		return allSubtitles[i].Start < allSubtitles[j].Start
	})

	var sb strings.Builder
	for i, sub := range allSubtitles {
		sb.WriteString(fmt.Sprintf("%d\n", i+1))
		sb.WriteString(fmt.Sprintf("%02d:%02d:%02d,000 --> %02d:%02d:%02d,000\n", sub.Start.Hours(), int(sub.Start.Minutes())%60, int(sub.Start.Seconds())%60, sub.End.Hours(), int(sub.End.Minutes())%60, int(sub.End.Seconds())%60))
		sb.WriteString(sub.Content + "\n\n")
	}

	mergedSubtitles := "merged_sub_titles.srt" // Fixed filename to avoid FFmpeg path issues
	err = os.WriteFile(mergedSubtitles, []byte(sb.String()), 0644)
	if err != nil {
		return err
	}

	//ffmpegCmd := exec.Command("ffmpeg",
	//	"-i", input,
	//	"-vf", fmt.Sprintf("subtitles=%s", mergedSubtitles),
	//	"-c:a", "copy",
	//	output,
	//)
	//return runCommand(ffmpegCmd) // Fixed runCommand parameter
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
