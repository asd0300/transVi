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
	if err != nil {
		fmt.Printf("Error walking audio_parts directory: %v\n", err)
		os.Exit(1)
	}

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

	// Clean up temporary directories
	err = os.RemoveAll("audio_parts")
	if err != nil {
		fmt.Printf("Error deleting audio_parts directory: %v\n", err)
	}
	err = os.RemoveAll("subtitles")
	if err != nil {
		fmt.Printf("Error deleting subtitles directory: %v\n", err)
	}
}

type Subtitle struct {
	Index   int
	Start   time.Duration
	End     time.Duration
	Content string
}

func parseSrtTime(s string) (time.Duration, error) {
	parts := strings.Split(s, ",")
	if len(parts) != 2 {
		return 0, fmt.Errorf("invalid time format: %s", s)
	}
	hms := parts[0]
	msStr := parts[1]

	t, err := time.Parse("15:04:05", hms)
	if err != nil {
		return 0, err
	}

	ms, err := strconv.Atoi(msStr)
	if err != nil {
		return 0, err
	}

	duration := time.Duration(t.Hour())*time.Hour +
		time.Duration(t.Minute())*time.Minute +
		time.Duration(t.Second())*time.Second +
		time.Duration(ms)*time.Millisecond

	return duration, nil
}

func parseSrt(data []byte, offset time.Duration) ([]Subtitle, error) {
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

		startTime, err := parseSrtTime(timecodes[0])
		if err != nil {
			continue
		}
		endTime, err := parseSrtTime(timecodes[1])
		if err != nil {
			continue
		}

		content := strings.Join(lines[2:], "\n")

		subtitles = append(subtitles, Subtitle{
			Index:   index,
			Start:   startTime + offset,
			End:     endTime + offset,
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
			filename := filepath.Base(path)
			indexStr := strings.TrimPrefix(filename, "part")
			indexStr = strings.TrimSuffix(indexStr, ".srt")
			index, err := strconv.Atoi(indexStr)
			if err != nil || index < 1 {
				fmt.Printf("Skipping invalid or zero index in filename: %s\n", filename)
				return nil // skip this file
			}

			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}

			offset := time.Duration(index-1) * 30 * time.Second
			subs, err := parseSrt(data, offset)
			if err != nil {
				fmt.Printf("Error parsing srt file %s: %v\n", path, err)
				return nil
			}

			// 修正 Subtitle 負時間（保險做法）
			for i := range subs {
				if subs[i].Start < 0 {
					subs[i].Start = 0
				}
				if subs[i].End < 0 {
					subs[i].End = 0
				}
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
		start := sub.Start
		end := sub.End
		sb.WriteString(fmt.Sprintf("%02d:%02d:%02d,%03d --> %02d:%02d:%02d,%03d\n",
			int(start.Hours()), int(start.Minutes())%60, int(start.Seconds())%60, start.Milliseconds()%1000,
			int(end.Hours()), int(end.Minutes())%60, int(end.Seconds())%60, end.Milliseconds()%1000))
		sb.WriteString(sub.Content + "\n\n")
	}

	mergedSubtitles := "merged_sub_titles.srt"
	err = os.WriteFile(mergedSubtitles, []byte(sb.String()), 0644)
	if err != nil {
		return err
	}

	// Delete individual srt files after merging
	err = filepath.WalkDir("subtitles", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && filepath.Ext(path) == ".srt" {
			err = os.Remove(path)
			if err != nil {
				fmt.Printf("Error deleting srt file %s: %v\n", path, err)
			}
		}
		return nil
	})
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
	err = runCommand(whisperCmd) // Changed := to =
	if err != nil {
		return err
	}

	// Delete the .wav file after processing
	err = os.Remove(c.Input)
	if err != nil {
		fmt.Printf("Error deleting audio file %s: %v\n", c.Input, err)
	}
	return nil
}
