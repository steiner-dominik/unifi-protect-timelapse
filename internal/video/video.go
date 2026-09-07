// Package video renders a day of frames into an MP4 by driving ffmpeg.
//
// Frames are fed to ffmpeg over stdin as a JPEG stream rather than by pattern
// matching a directory, because the archive holds several capture series in one
// day directory and their filenames are not a contiguous numbered sequence.
package video

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/steiner-dominik/unifi-protect-timelapse/internal/config"
)

// ErrUnavailable reports that ffmpeg is not installed or not executable.
var ErrUnavailable = errors.New("ffmpeg is not available")

// Renderer turns a list of JPEG files into an MP4.
type Renderer struct {
	cfg *config.Video
	log *slog.Logger
}

// New returns a Renderer.
func New(cfg *config.Video, log *slog.Logger) *Renderer {
	return &Renderer{cfg: cfg, log: log}
}

// Available reports whether rendering is possible in this image.
func (r *Renderer) Available() bool {
	if !r.cfg.Enabled {
		return false
	}
	_, err := exec.LookPath(r.cfg.FFmpeg)
	return err == nil
}

// Render writes an MP4 of the given frames to out. Frames must already be in
// the order they should appear.
func (r *Renderer) Render(ctx context.Context, frames []string, fps int, out io.Writer) error {
	if !r.Available() {
		return ErrUnavailable
	}
	if len(frames) == 0 {
		return errors.New("no frames to render")
	}
	if fps < 1 || fps > 120 {
		fps = r.cfg.FPS
	}
	if r.cfg.MaxFrames > 0 && len(frames) > r.cfg.MaxFrames {
		return fmt.Errorf("selection has %d frames, which exceeds VIDEO_MAX_FRAMES=%d",
			len(frames), r.cfg.MaxFrames)
	}

	ctx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()

	args := []string{
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-f", "image2pipe", "-framerate", strconv.Itoa(fps), "-i", "-",
		"-c:v", "libx264",
		"-crf", strconv.Itoa(r.cfg.CRF),
		"-preset", "veryfast",
		"-pix_fmt", "yuv420p",
		// H.264 requires even dimensions; archived frames are not guaranteed
		// to have them, so round down rather than fail.
		"-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2",
		// Puts the index at the front so the file can play while downloading.
		"-movflags", "+faststart+frag_keyframe+empty_moov",
		"-f", "mp4",
		"-",
	}

	cmd := exec.CommandContext(ctx, r.cfg.FFmpeg, args...)
	cmd.Stdout = out

	stderr := &limitedBuffer{limit: 8 << 10}
	cmd.Stderr = stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting ffmpeg: %w", err)
	}

	writeErr := streamFrames(ctx, stdin, frames)
	// Closing stdin is what tells ffmpeg the stream has ended; it must happen
	// whether or not the frames were written successfully.
	_ = stdin.Close()

	if err := cmd.Wait(); err != nil {
		if writeErr != nil {
			return fmt.Errorf("feeding frames to ffmpeg: %w", writeErr)
		}
		return fmt.Errorf("ffmpeg failed: %w: %s", err, stderr.String())
	}
	return writeErr
}

// streamFrames copies each JPEG into ffmpeg's stdin in order. A frame that
// cannot be read is skipped rather than failing the whole render, because a
// single unreadable file should not cost the operator the other several hundred.
func streamFrames(ctx context.Context, w io.Writer, frames []string) error {
	for _, path := range frames {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		file, err := os.Open(path)
		if err != nil {
			continue
		}
		_, err = io.Copy(w, file)
		_ = file.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// limitedBuffer keeps only the first limit bytes, so a chatty failure cannot
// hold an unbounded amount of memory.
type limitedBuffer struct {
	limit int
	data  []byte
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if remaining := b.limit - len(b.data); remaining > 0 {
		b.data = append(b.data, p[:min(remaining, len(p))]...)
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string { return string(b.data) }

// EstimateDuration reports how long the rendered clip will be.
func EstimateDuration(frames, fps int) time.Duration {
	if fps < 1 {
		return 0
	}
	return time.Duration(frames) * time.Second / time.Duration(fps)
}
