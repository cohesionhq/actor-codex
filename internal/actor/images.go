package actor

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/cohesionhq/actor-codex/internal/cohesion"
)

// Images between Codex and Cohesion: the user's go in as data URLs, and the
// ones Codex shows (an image it viewed, one an MCP tool gave back) come back
// with the transcript message they belong to, checked against the rules
// cohesiond refuses a message for.

// MaxImages is the most images one message carries.
const MaxImages = 10

// MaxImageLength is the most one image's base64 may be: 5 MB.
const MaxImageLength = 5 * 1024 * 1024

var mediaTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

var base64Pattern = regexp.MustCompile(`^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$`)

// IsBase64 is whether `data` is standard, padded base64, with nothing around it.
func IsBase64(data string) bool {
	return len(data) > 0 && len(data)%4 == 0 && base64Pattern.MatchString(data)
}

// Refusal is why cohesiond would refuse `image`, or "" when it wouldn't.
func Refusal(image cohesion.Image) string {
	if !mediaTypes[image.MediaType] {
		if image.MediaType == "" {
			return "it is of no type"
		}
		return "it is " + image.MediaType
	}
	if !IsBase64(image.Data) {
		return "it isn't base64"
	}
	if len(image.Data) > MaxImageLength {
		return "it is over 5 MB"
	}
	return ""
}

var (
	dataPrefix  = regexp.MustCompile(`^data:[^,]*,`)
	whitespace  = regexp.MustCompile(`\s+`)
	unpadded    = regexp.MustCompile(`^[A-Za-z0-9+/]+$`)
	dataURLForm = regexp.MustCompile(`^data:([^;,]+);base64,(.*)$`)
)

// Tidy is `image` the way cohesiond wants it: whitespace, a `data:` prefix
// and missing padding gone, and `image/jpg` named `image/jpeg`.
func Tidy(image cohesion.Image) cohesion.Image {
	mediaType := strings.ToLower(strings.TrimSpace(image.MediaType))
	if mediaType == "image/jpg" {
		mediaType = "image/jpeg"
	}
	data := whitespace.ReplaceAllString(dataPrefix.ReplaceAllString(image.Data, ""), "")
	if unpadded.MatchString(data) && len(data)%4 > 1 {
		data += strings.Repeat("=", 4-len(data)%4)
	}
	return cohesion.Image{MediaType: mediaType, Data: data}
}

// Sendable is what a message can carry of `images`, and why the rest can't go.
func Sendable(images []cohesion.Image) (kept []cohesion.Image, dropped []string) {
	for _, image := range images {
		image = Tidy(image)
		why := fmt.Sprintf("a message carries at most %d", MaxImages)
		if len(kept) < MaxImages {
			why = Refusal(image)
		}
		if why != "" {
			dropped = append(dropped, why)
		} else {
			kept = append(kept, image)
		}
	}
	return kept, dropped
}

// DataURL is `image` as Codex takes it in a turn's input.
func DataURL(image cohesion.Image) string {
	return "data:" + image.MediaType + ";base64," + image.Data
}

// FromDataURL is the image a `data:` URL holds, or false for any other URL.
func FromDataURL(url string) (cohesion.Image, bool) {
	match := dataURLForm.FindStringSubmatch(url)
	if match == nil {
		return cohesion.Image{}, false
	}
	return cohesion.Image{MediaType: match[1], Data: match[2]}, true
}

// ReadsImage is whether `path` names an image file Cohesion can show.
func ReadsImage(path string) bool {
	_, ok := imageType(path)
	return ok
}

func imageType(path string) (string, bool) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png", true
	case ".jpg", ".jpeg":
		return "image/jpeg", true
	case ".gif":
		return "image/gif", true
	case ".webp":
		return "image/webp", true
	}
	return "", false
}

// ImageFile is the image at `path`, or false when it isn't one or can't be read.
func ImageFile(path string) (cohesion.Image, bool) {
	mediaType, ok := imageType(path)
	if !ok {
		return cohesion.Image{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cohesion.Image{}, false
	}
	return cohesion.Image{MediaType: mediaType, Data: base64.StdEncoding.EncodeToString(data)}, true
}

func decodeBase64(data string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(data)
}
